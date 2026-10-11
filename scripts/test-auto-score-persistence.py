#!/usr/bin/env python3
"""Real-binary passive score-persistence regression; no real upstreams.

Run with an existing, private TMPDIR and permission to create a network namespace:
  unshare --net -- sh -c 'ip link set lo up && exec python3 -B \
scripts/test-auto-score-persistence.py "$@"' sh /absolute/binary

Both channel-cache modes run by default. --list-cases needs no binary or namespace.
PASS=0, behavioral FAIL=1, fixture/bootstrap/network ERROR=2 (argparse also exits 2).
--negative-control --cases idle-no-probes injects one unsolicited synthetic GET per
selected mode and must FAIL/1. Add --negative-control-kind parser431 to send an
oversized header that HTTP parsing rejects before do_GET. Both must FAIL/1.
Every other upstream call follows an explicitly sent
fixture business request. The sibling SSE helper is imported by path, unchanged.

Each case owns a fresh DB/log directory. Results and raw request/response bytes are
retained under TMPDIR; credentials and inherited provider/DB environments are not
reported. No real provider, production DB, existing environment file or service is
read or modified. A separate loopback-only netns is required before bootstrap I/O.

Persistence is proved while the gateway is still running, before stopping it: this
does NOT assume SIGTERM executes Go defers or flushes the latest unsaved interval.
The startup-ranking case also checks count continuation, so a lucky cold tie cannot
prove restore. Legacy idle/priority/cooldown controls remain useful on old binaries;
without the score table the three persistence cases fail explicitly, not with SQL
setup errors. With the table present, score-enhanced legacy controls are mandatory.
"""
import argparse
import base64
from contextlib import ExitStack, closing
import hashlib
import http.client
import http.server
import importlib.util
import json
import math
import os
from pathlib import Path
import secrets
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
from types import SimpleNamespace

_NAMESPACE_SPEC = importlib.util.spec_from_file_location(
    'fixture_namespace', Path(__file__).with_name('fixture_namespace.py'))
assert _NAMESPACE_SPEC is not None and _NAMESPACE_SPEC.loader is not None
fixture_namespace = importlib.util.module_from_spec(_NAMESPACE_SPEC)
_NAMESPACE_SPEC.loader.exec_module(fixture_namespace)

CASES = ('idle-no-probes', 'feedback-save-reload', 'startup-two-level-ranking',
         'expired-scores-neutral', 'priority-over-score', 'strict-pair-cooldown',
         'cooldown-expiry-passive')

# 衰减窗口现在是可配置的(AutoModelScoreDecayMinutes,默认 30 分钟),所以这个
# 预言模型必须显式声明窗口并把它钉给被测网关,不能在计算里写死 10 分钟:
# 那会让断言测的是早已不存在的默认值。
#
# 注意:环境变量只是启动种子,而本套件预置的 DB 里没有这个 option 键,
# loadOptionsFromDatabase 找不到键就不会覆盖种子,所以 env 在这里生效。
# 一旦有人往预置 DB 里写 AutoModelScoreDecayMinutes,就必须同步这个常量。
DECAY_WINDOW_MS = 30 * 60 * 1000

SCORE_COLUMNS = ('scope', 'group', 'model', 'channel_id', 'score', 'latency_ms',
                 'observations', 'observed_at', 'version')


def read_scores(root):
    """Read only this case's fresh DB; absence of the new schema is behavior."""
    with closing(sqlite3.connect((root / 'one-api.db').as_uri() + '?mode=ro', uri=True)) as conn:
        conn.row_factory = sqlite3.Row
        present = conn.execute("SELECT 1 FROM sqlite_master WHERE type='table' "
                               "AND name='auto_model_scores'").fetchone() is not None
        columns = [row['name'] for row in conn.execute('PRAGMA table_info(auto_model_scores)')]
        rows = []
        if present and set(SCORE_COLUMNS) <= set(columns):
            sql_columns = ','.join('"' + name + '"' for name in SCORE_COLUMNS)
            rows = [dict(row) for row in conn.execute(
                'SELECT ' + sql_columns + ' FROM auto_model_scores '
                'ORDER BY scope,"group",model,channel_id')]
    return {'present': present, 'columns': columns, 'rows': rows, 'read_at_ns': time.time_ns()}


def score_key(row):
    return tuple(row.get(key) for key in ('scope', 'group', 'model', 'channel_id'))


def check_scores(snapshot, expected_keys=None):
    if not snapshot['present']:
        return ['missing auto_model_scores table']
    missing = sorted(set(SCORE_COLUMNS) - set(snapshot['columns']))
    if missing:
        return ['missing auto_model_scores columns: ' + repr(missing)]
    failures = []
    keys = [score_key(row) for row in snapshot['rows']]
    if len(keys) != len(set(keys)):
        failures.append('duplicate score scope/key rows')
    if expected_keys is not None and set(keys) != set(expected_keys):
        failures.append('score scope/key set differs: ' + repr(keys))
    for row in snapshot['rows']:
        scope, group, model, channel = score_key(row)
        if (scope not in ('channel', 'model') or not isinstance(group, str) or not group
                or not isinstance(model, str) or (scope == 'channel') != (model == '')
                or type(channel) is not int or channel <= 0):
            failures.append('invalid score scope/key')
        if row.get('version') != 1:
            failures.append('score version is not 1')
        if type(row.get('observed_at')) is not int or not 0 < row['observed_at'] <= 2**63 - 1:
            failures.append('observed_at is not an actual UnixNano int64')
        for field, low, high in (('score', 0, 1), ('latency_ms', 0, math.inf),
                                 ('observations', 0, 20)):
            value = row.get(field)
            if (type(value) not in (int, float) or not math.isfinite(value)
                    or not low <= value <= high or (field == 'observations' and value == 0)):
                failures.append('invalid score numeric field: ' + field)
    return failures


def check_manifest(expected_ids, results):
    observed = [row['id'] for row in results]
    failures = []
    if len(expected_ids) != len(set(expected_ids)):
        failures.append('duplicate selected case IDs')
    if len(observed) != len(set(observed)):
        failures.append('duplicate case results')
    if len(observed) != len(expected_ids) or set(observed) != set(expected_ids):
        failures.append('missing or unrequested case results')
    return failures


def check_response(response, expected_body):
    failures = []
    if response['status'] != 200:
        failures.append('successful fixture HTTP status changed')
    if response['body_b64'] != base64.b64encode(expected_body).decode('ascii'):
        failures.append('successful fixture response bytes changed')
    return failures


def check_first_score(row):
    """Expired history must not become a confident or slow first observation."""
    failures = []
    if not (abs(row['observations'] - 1) < 0.0001 and abs(row['score'] - 0.65) < 0.0001):
        failures.append('first success did not produce one observation / score 0.65')
    if not 0 < row['latency_ms'] < 20000:
        failures.append('first success latency was not freshly observed')
    return failures


def latency_update_bounds(before, sample_bounds_ms, observation_window_ns):
    """Predict from prior history + external timings, never the row under test."""
    low, high = sample_bounds_ms
    start, end = observation_window_ns
    if (any(type(value) not in (int, float) or not math.isfinite(value) for value in (low, high))
            or not 0 < low <= high or type(start) is not int or type(end) is not int
            or not 0 < start <= end):
        raise ValueError('invalid independent latency measurement bounds')
    factors = [max(0, 1 - max(0, (at - before['observed_at']) // 1000000) / DECAY_WINDOW_MS)
               for at in (end, start)]
    if before['latency_ms'] == 0 or factors[1] == 0:
        return [low, high]
    if factors[0] == 0:
        raise ValueError('history expired inside latency measurement window')
    return [before['latency_ms'] * factors[0] * 0.8 + low * 0.2,
            before['latency_ms'] * factors[1] * 0.8 + high * 0.2]


def check_update(before, after, sample_latency_ms=None, observation_window_ns=None):
    """One real success must fold into restored history, not restart at n=1."""
    failures = []
    if score_key(before) != score_key(after):
        failures.append('updated score key changed')
    if after['observed_at'] <= before['observed_at']:
        failures.append('last actual observation time did not advance')
    age_ms = max(0, (after['observed_at'] - before['observed_at']) // 1000000)
    factor = max(0, 1 - age_ms / DECAY_WINDOW_MS)
    old_score = 0.5 + (before['score'] - 0.5) * factor
    old_n = min(20, max(0, before['observations'])) * factor
    expected = {'score': old_score + 0.30 * (1 - old_score),
                'observations': min(20, old_n + 1)}
    if isinstance(sample_latency_ms, (list, tuple)):
        low, high = latency_update_bounds(before, sample_latency_ms, observation_window_ns)
        if not low - 0.0001 <= after['latency_ms'] <= high + 0.0001:
            failures.append('restored latency_ms update differs: '
                            f'{after["latency_ms"]!r} not in independently measured bounds [{low}, {high}]')
    elif sample_latency_ms is not None:
        old_latency = before['latency_ms'] * factor
        expected['latency_ms'] = (old_latency * 0.8 + sample_latency_ms * 0.2
                                  if old_latency else sample_latency_ms)
    for field, value in expected.items():
        if not math.isclose(after[field], value, rel_tol=0, abs_tol=0.0001):
            failures.append(f'restored {field} update differs: {after[field]!r} != {value!r}')
    return failures


def check_idle(before, after):
    """Compare the complete fake-upstream request log across an idle window."""
    return ['unexpected upstream request during idle'] if after != before else []


def check_calls(observed, expected):
    """A POST body alone cannot establish upstream identity or absence of probes."""
    identities = [tuple(call.get(key) for key in ('method', 'path', 'channel', 'model'))
                  for call in observed]
    return [] if identities == list(expected) else [
        'upstream method/path/channel/model sequence or call count differs: '
        + repr(identities) + ' != ' + repr(list(expected))]


REPLIES = {
    'score-a': b'{"id":"score-fixture","object":"chat.completion","created":1,"model":"score-a","choices":[{"index":0,"message":{"role":"assistant","content":"fixture-ok:score-a"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}',
    'score-b': b'{"id":"score-fixture","object":"chat.completion","created":1,"model":"score-b","choices":[{"index":0,"message":{"role":"assistant","content":"fixture-ok:score-b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}',
    'score-c': b'{"id":"score-fixture","object":"chat.completion","created":1,"model":"score-c","choices":[{"index":0,"message":{"role":"assistant","content":"fixture-ok:score-c"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}',
}


def json_evidence(value):
    """Keep malformed nonfinite DB values explicit without breaking evidence I/O."""
    if isinstance(value, float) and not math.isfinite(value):
        return {'nonfinite_float': repr(value)}
    if isinstance(value, dict):
        return {key: json_evidence(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_evidence(item) for item in value]
    return value


def append_jsonl(path, value):
    with path.open('a', encoding='utf-8') as out:
        out.write(json.dumps(json_evidence(value), ensure_ascii=False, allow_nan=False) + '\n')
        out.flush()
        os.fsync(out.fileno())


class Upstream(http.server.ThreadingHTTPServer):
    daemon_threads = False
    block_on_close = True

    def __init__(self, root):
        require_namespace()
        self.lock = threading.Lock()
        self.calls = []
        self.errors = []
        self.case_id = None
        self.root = root
        self.delay = 0.03  # A measurable ordinary success, far below slow/cooldown thresholds.
        super().__init__(('127.0.0.1', 0), UpstreamHandler)

    def snapshot(self):
        with self.lock:
            return [dict(row) for row in self.calls]

    def handle_error(self, request, client_address):
        with self.lock:
            self.errors.append('uncaught synthetic upstream handler error')


class UpstreamHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, format, *args):
        pass

    def record_request_line(self):
        """Count before HTTP parsing; even rejected input is upstream traffic."""
        if self.request_row is not None or not self.raw_requestline:
            return
        words = self.raw_requestline.decode('iso-8859-1').split()
        method = words[0] if words else None
        path = words[1] if len(words) > 1 else None
        first = path.split('/')[1:2] if path is not None else []
        channel = {'channel-1': 1, 'channel-2': 2}.get(first[0] if first else '')
        state = self.server
        with state.lock:
            self.request_row = {'sequence': len(state.calls) + 1, 'case_id': state.case_id,
                'at_ns': time.time_ns(), 'method': method, 'path': path, 'channel': channel,
                'model': None, 'body_b64': '', 'parser_rejected': False,
                'request_line_b64': base64.b64encode(self.raw_requestline).decode('ascii')}
            state.calls.append(self.request_row)
            try:
                append_jsonl(state.root / 'upstream-requests.jsonl', dict(self.request_row, event='received'))
            except Exception as exc:
                state.errors.append('upstream evidence write: ' + repr(exc))

    def handle_one_request(self):
        self.request_row = None
        self.raw_requestline = b''
        self.connection.settimeout(5)
        try:
            super().handle_one_request()
        finally:
            # Oversized request lines bypass parse_request; blank/malformed input
            # need not cause send_error at all. An empty TCP EOF is not a request.
            self.record_request_line()

    def parse_request(self):
        self.record_request_line()
        accepted = super().parse_request()
        if not accepted and not self.request_row['parser_rejected']:
            with self.server.lock:
                self.request_row.update(parser_rejected=True, status=None)
                append_jsonl(self.server.root / 'upstream-requests.jsonl',
                             dict(self.request_row, event='parser-rejected'))
        return accepted

    def send_error(self, code, message=None, explain=None):
        # BaseHTTPRequestHandler calls this for 400/414/431 before any do_*.
        self.record_request_line()
        with self.server.lock:
            self.request_row.update(parser_rejected=True, status=int(code), parser_error=message)
            append_jsonl(self.server.root / 'upstream-requests.jsonl',
                         dict(self.request_row, event='parser-rejected'))
        return super().send_error(code, message, explain)

    def __getattr__(self, name):
        # BaseHTTPRequestHandler normally drops unknown verbs as unobserved 501s.
        if name.startswith('do_'):
            return self.observe_request
        raise AttributeError(name)

    def read_body(self):
        limit = 2 * 1024 * 1024
        if self.headers.get('Transfer-Encoding', '').lower() == 'chunked':
            chunks, total = [], 0
            while True:
                size = int(self.rfile.readline(256).split(b';', 1)[0], 16)
                if size == 0:
                    while self.rfile.readline(8192).strip():
                        pass
                    return b''.join(chunks)
                total += size
                if not 0 < size <= limit or total > limit:
                    raise ValueError('oversized fixture request')
                part = self.rfile.read(size)
                if len(part) != size or self.rfile.read(2) != b'\r\n':
                    raise ValueError('incomplete fixture chunk')
                chunks.append(part)
        size = int(self.headers.get('Content-Length', '0'))
        if not 0 <= size <= limit:
            raise ValueError('oversized fixture request')
        body = self.rfile.read(size)
        if len(body) != size:
            raise ValueError('incomplete fixture request')
        return body

    def observe_request(self):
        state = self.server
        self.connection.settimeout(5)
        self.close_connection = True
        first = self.path.split('/')[1:2]
        channel = {'channel-1': 1, 'channel-2': 2}.get(first[0] if first else '')
        row = self.request_row  # Already counted at the parser boundary, never again in do_*.
        with state.lock:
            row.update(method=self.command, path=self.path, channel=channel)
        try:
            raw = self.read_body()
            try:
                request = json.loads(raw) if raw else {}
            except (ValueError, UnicodeError):
                request = {}
            model = request.get('model') if isinstance(request, dict) else None
            with state.lock:
                row.update(model=model, body_b64=base64.b64encode(raw).decode('ascii'))
            valid = (self.command == 'POST' and channel is not None
                     and self.path == f'/channel-{channel}/v1/chat/completions'
                     and model in REPLIES and request.get('stream') is False)
            body = REPLIES[model] if valid else b'{"fixture":"unrequested method/path/body"}'
            status = 200 if valid else 404
            if valid:
                with state.lock:
                    row['delay_started_mono_ns'] = time.monotonic_ns()
                time.sleep(state.delay)
                with state.lock:
                    row['reply_started_mono_ns'] = time.monotonic_ns()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.send_header('Connection', 'close')
            self.end_headers()
            if self.command != 'HEAD':
                self.wfile.write(body)
                self.wfile.flush()
            with state.lock:
                row.update(completed_ns=time.time_ns(), status=status,
                           reply_b64=base64.b64encode(body).decode('ascii'))
                append_jsonl(state.root / 'upstream-requests.jsonl', dict(row, event='completed'))
        except Exception as exc:
            with state.lock:
                state.errors.append('synthetic upstream I/O: ' + repr(exc))


def start_upstream(root):
    with ExitStack() as rollback:
        upstream = Upstream(root)
        rollback.callback(upstream.server_close)
        worker = threading.Thread(target=lambda: upstream.serve_forever(poll_interval=0.05),
                                  name='auto-score-upstream', daemon=False)
        worker.start()
        rollback.callback(worker.join, timeout=5)
        rollback.callback(upstream.shutdown)
        rollback.pop_all()
    return upstream, worker


def stop_upstream(upstream, worker):
    errors = []
    for action in (upstream.shutdown, upstream.server_close, lambda: worker.join(timeout=5)):
        try:
            action()
        except Exception as exc:
            errors.append('upstream cleanup: ' + repr(exc))
    if worker.is_alive() or upstream.socket.fileno() != -1:
        errors.append('synthetic upstream was not fully stopped/closed')
    return errors


def require_namespace():
    return fixture_namespace.require_namespace()


def load_fixture():
    """Use the installed sibling helper, never a checkout-specific absolute path."""
    sys.dont_write_bytecode = True
    path = Path(__file__).with_name('test-auto-sse-errors.py')
    spec = importlib.util.spec_from_file_location('score_fixture_base', path)
    if spec is None or spec.loader is None:
        raise RuntimeError('cannot import sibling fixture helper')
    fixture = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(fixture)
    return fixture


def bootstrap(source):
    isolation = require_namespace()  # Also protect callers other than the CLI.
    tmp = Path(os.environ['TMPDIR'])
    if not tmp.is_absolute() or not tmp.is_dir():
        raise RuntimeError('TMPDIR must name an existing absolute scratch directory')
    with ExitStack() as rollback:
        base = Path(tempfile.mkdtemp(prefix='auto-score-persistence-', dir=tmp))
        rollback.callback(shutil.rmtree, base)
        source_hash = hashlib.sha256(source.read_bytes()).hexdigest()
        binary = base / 'new-api'
        shutil.copy2(source, binary)
        if hashlib.sha256(binary.read_bytes()).hexdigest() != source_hash:
            raise RuntimeError('binary changed while copying')
        fixture = load_fixture()
        upstream, worker = start_upstream(base)
        rollback.callback(stop_upstream, upstream, worker)
        state = SimpleNamespace(base=base, binary=binary, sha256=source_hash, isolation=isolation,
                                fixture=fixture, upstream=upstream, worker=worker)
        rollback.pop_all()
    return state


def http_request(port, method, path, body=None, headers=None, timeout=15):
    """Direct loopback client: never consult proxy environment or follow redirects."""
    started = time.time_ns()
    started_mono = time.monotonic_ns()
    with closing(http.client.HTTPConnection('127.0.0.1', port, timeout=timeout)) as conn:
        conn.request(method, path, body=body, headers=headers or {})
        with conn.getresponse() as response:
            raw = response.read()
            return {'status': response.status, 'body': raw.decode('utf-8', errors='replace'),
                    'body_b64': base64.b64encode(raw).decode('ascii'),
                    'body_sha256': hashlib.sha256(raw).hexdigest(),
                    'request_id': response.getheader('X-Oneapi-Request-Id', ''),
                    'started_ns': started, 'ended_ns': time.time_ns(),
                    'started_mono_ns': started_mono, 'ended_mono_ns': time.monotonic_ns()}


def measured_latency_bounds(response, call):
    """Bracket the attempt with measured stub delay and client settlement time.

    The lower limit is the actual pre-header sleep (not its configured duration).
    The upper includes local conversion/delivery and deferred feedback settling.
    Millisecond rounding brackets Go's Duration.Milliseconds truncation. No DB
    result supplies a sample, a tolerance or an expected latency.
    """
    start, end, settled = (response[key] for key in ('started_mono_ns', 'ended_mono_ns', 'settled_mono_ns'))
    delay, reply = call['delay_started_mono_ns'], call['reply_started_mono_ns']
    if not start <= delay < reply <= end <= settled:
        raise RuntimeError('synthetic request timing boundaries are incomplete or unordered')
    return [max(1, (reply - delay) // 1000000), math.ceil((settled - start) / 1000000)]


def wait_ready(process, port):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError('isolated gateway exited during startup')
        try:
            response = http_request(port, 'GET', '/api/status', timeout=1)
            payload = json.loads(response['body'])
            if response['status'] == 200 and payload.get('success') is True:
                return payload['data']['version']
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(0.1)
    raise RuntimeError('isolated gateway startup deadline exceeded')


class Gateway:
    def __init__(self, fixture, binary, root, port, cache, row):
        self.fixture, self.binary, self.root = fixture, binary, root
        self.port, self.cache, self.row = port, cache, row
        self.process = self.log = None

    def start(self):
        require_namespace()
        if self.process is not None or self.log is not None:
            raise RuntimeError('gateway already owns a process/log')
        # Deliberate allowlist, not a copy of the host's provider/DB/tester env.
        env = {key: os.environ[key] for key in ('PATH', 'LANG') if key in os.environ}
        env.update({'HOME': str(self.root), 'TMPDIR': str(self.root),
                    'SQLITE_PATH': str(self.root / 'one-api.db'), 'GIN_MODE': 'release',
                    'MEMORY_CACHE_ENABLED': self.cache, 'ERROR_LOG_ENABLED': 'false',
                    'SESSION_SECRET': secrets.token_hex(32),
                    # 把预言模型用的衰减窗口钉给被测网关,避免默认值漂移让
                    # 断言在某个版本悄悄测错窗口。见 DECAY_WINDOW_MS 的说明。
                    'AUTO_MODEL_SCORE_DECAY_MINUTES': str(DECAY_WINDOW_MS // 60000)})
        log_path = self.root / ('process-' + str(time.time_ns()) + '.log')
        self.log = log_path.open('wb')
        try:
            self.process = subprocess.Popen(
                [str(self.binary), '--port', str(self.port), '--log-dir', str(self.root / 'logs')],
                cwd=self.root, env=env, stdin=subprocess.DEVNULL, stdout=self.log,
                stderr=subprocess.STDOUT, start_new_session=True)
            version = wait_ready(self.process, self.port)
            self.row.setdefault('starts', []).append({'pid': self.process.pid,
                'ready_ns': time.time_ns(), 'version': version, 'log': str(log_path)})
            return version
        except BaseException:
            self.stop()
            raise

    def ensure_live(self):
        if self.process is None or self.process.poll() is not None:
            raise RuntimeError('isolated gateway exited during the observation window')

    def stop(self):
        process, log = self.process, self.log
        if process is None and log is None:
            return
        try:
            if process is not None:
                self.fixture.stop(process, log)
        except Exception as exc:
            self.row['fixture_errors'].append('gateway cleanup: ' + repr(exc))
        finally:
            try:
                if process is not None and process.poll() is None:
                    process.kill()
                    process.wait(timeout=5)
            except Exception as exc:
                self.row['fixture_errors'].append('gateway reap: ' + repr(exc))
            finally:
                if log is not None:
                    log.close()
                reaped = process is None or process.poll() is not None
                self.row['cleanup'].append({'pid': process.pid if process else None,
                    'reaped': reaped, 'log_closed': log is None or log.closed,
                    'returncode': process.returncode if process else None})
                self.process = self.log = None
                if not reaped:
                    self.row['fixture_errors'].append('gateway process was not reaped')


class BehaviorFailure(Exception):
    """Only an explicit evidence assertion may become FAIL/1."""


def require(failures):
    if failures:
        raise BehaviorFailure('; '.join(failures))


def query(root, sql, values=()):
    with closing(sqlite3.connect((root / 'one-api.db').as_uri() + '?mode=ro', uri=True)) as conn:
        conn.row_factory = sqlite3.Row
        return [dict(row) for row in conn.execute(sql, values)]


def seed_fixture(root, port, routes, priorities):
    """Fresh synthetic identities only. No host DB, key, dotenv or admin login."""
    tokens = {name: secrets.token_hex(24) for name in ('all', 'score-a', 'score-b', 'score-c')}
    now = int(time.time())
    with closing(sqlite3.connect(root / 'one-api.db')) as conn, conn:
        conn.execute('INSERT INTO users (id,username,password,display_name,role,status,quota,used_quota,request_count,"group",aff_code,aff_count,aff_quota) VALUES (1,\'fixture\',\'unusable-password-hash\',\'fixture\',100,1,100000000,0,0,\'default\',\'fixture\',0,0)')
        for index, (name, token) in enumerate(tokens.items(), 1):
            conn.execute('INSERT INTO tokens (id,user_id,key,status,name,created_time,accessed_time,expired_time,remain_quota,unlimited_quota,model_limits_enabled,model_limits,"group",groups,cross_group_retry) VALUES (?,1,?,1,\'fixture\',?,?,-1,100000000,1,?,?,\'default\',\'default\',0)',
                         (index, token, now, now, int(name != 'all'), '' if name == 'all' else name))
        for channel, models in routes.items():
            conn.execute('INSERT INTO channels (id,type,key,status,name,weight,created_time,base_url,models,"group",used_quota,priority,auto_ban,setting) VALUES (?,1,\'synthetic-only\',1,?,0,?,?,?,\'default\',0,?,0,?)',
                         (channel, 'fixture-channel-' + str(channel), now,
                          f'http://127.0.0.1:{port}/channel-{channel}', ','.join(models),
                          priorities[channel], json.dumps({'show_error_details': True, 'force_format': False})))
            for model in models:
                conn.execute('INSERT INTO abilities ("group",model,channel_id,enabled,priority,weight,tag) VALUES (\'default\',?,?,1,?,0,\'\')',
                             (model, channel, priorities[channel]))
        options = {'ModelRatio': json.dumps({name: 1 for name in REPLIES}), 'RetryTimes': '0',
                   'performance_setting.monitor_enabled': 'false',
                   'monitor_setting.auto_test_channel_enabled': 'false'}
        for key, value in options.items():
            conn.execute('INSERT INTO options (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value', (key, value))
    return tokens


def model_key(model, channel):
    return ('model', 'default', model, channel)


def channel_key(channel):
    return ('channel', 'default', '', channel)


def stored(key, score, observations, latency_ms, observed_at):
    return dict(zip(SCORE_COLUMNS, (*key, score, latency_ms, observations, observed_at, 1)))


def indexed(snapshot):
    return {score_key(row): row for row in snapshot['rows']}


def cooldowns(root):
    return query(root, 'SELECT "group",model,channel_id,level,until,updated_at '
                 'FROM auto_model_cooldowns ORDER BY "group",model,channel_id')


def seed_cooldowns(root, pairs, until):
    now = int(time.time())
    with closing(sqlite3.connect(root / 'one-api.db')) as conn, conn:
        for model, channel in pairs:
            conn.execute('INSERT INTO auto_model_cooldowns ("group",model,channel_id,until,level,reason,updated_at) '
                         'VALUES (\'default\',?,?,?,1,\'synthetic fixture\',?) '
                         'ON CONFLICT("group",model,channel_id) DO UPDATE SET until=excluded.until,level=1,updated_at=excluded.updated_at',
                         (model, channel, until, now))
    return cooldowns(root)


class Case:
    def __init__(self, state, cache, name, row):
        self.state, self.cache, self.name, self.row = state, cache, name, row
        self.root = state.base / (cache + '-' + name)
        self.gateway = None
        self.tokens = {}
        self.stage = 'case-directory'

    def event(self, kind, value):
        append_jsonl(self.root / 'events.jsonl', {'event': kind, 'at_ns': time.time_ns(), 'value': value})

    def prepare(self, routes=None, priorities=None):
        self.root.mkdir()
        self.row['work'] = str(self.root)
        routes = routes or {1: ('score-a', 'score-b'), 2: ('score-c',)}
        priorities = priorities or {1: 100, 2: 0}
        self.gateway = Gateway(self.state.fixture, self.state.binary, self.root,
                               self.state.fixture.free_port(), self.cache, self.row)
        self.start('empty-db-bootstrap')
        self.gateway.stop()
        self.stage = 'fresh-fixture-seed'
        self.tokens = seed_fixture(self.root, self.state.upstream.server_port, routes, priorities)
        self.row['routes'] = query(self.root, 'SELECT "group",model,channel_id,priority,enabled '
                                  'FROM abilities ORDER BY channel_id,model')
        self.row['automatic_test_option'] = query(self.root,
            'SELECT key,value FROM options WHERE key=\'monitor_setting.auto_test_channel_enabled\'')
        self.snapshot('initial-score-schema')

    def start(self, label='startup'):
        self.stage = label
        before = self.state.upstream.snapshot()
        started = time.time_ns()
        self.gateway.start()
        after = self.state.upstream.snapshot()
        window = {'label': label, 'started_ns': started, 'ended_ns': time.time_ns(),
                  'before_count': len(before), 'after_count': len(after)}
        self.row['windows'].append(window)
        self.event('window', window)
        require(check_idle(before, after))

    def idle(self, label, seconds=6, inject=False):
        self.stage = label
        before = self.state.upstream.snapshot()
        started, mono = time.time_ns(), time.monotonic()
        if inject:
            headers = {'X-Oversized-Fixture': 'x' * 65537} if inject == 'parser431' else None
            response = http_request(self.state.upstream.server_port, 'GET', '/deliberate-unsolicited',
                                    headers=headers)
            self.row['negative_control_response'] = response
            if inject == 'parser431' and response['status'] != 431:
                raise RuntimeError('parser431 negative control did not reach HTTP header rejection')
        deadline = mono + seconds
        while time.monotonic() < deadline:
            self.gateway.ensure_live()
            time.sleep(min(0.1, max(0, deadline - time.monotonic())))
        self.gateway.ensure_live()
        after = self.state.upstream.snapshot()
        window = {'label': label, 'started_ns': started, 'ended_ns': time.time_ns(),
                  'duration_seconds': time.monotonic() - mono,
                  'before_count': len(before), 'after_count': len(after)}
        self.row['windows'].append(window)
        self.event('window', window)
        require(check_idle(before, after))

    def snapshot(self, label):
        snapshot = read_scores(self.root)
        self.row['scores'].append({'label': label, 'snapshot': snapshot})
        self.event('scores', self.row['scores'][-1])
        return snapshot

    def seed_scores(self, rows, legacy_control=False):
        snapshot = self.snapshot('before-score-seed')
        if not snapshot['present'] and legacy_control:
            self.row['stored_score_control_applied'] = False
            return False
        require(check_scores(snapshot, []))
        columns = ','.join('"' + key + '"' for key in SCORE_COLUMNS)
        with closing(sqlite3.connect(self.root / 'one-api.db')) as conn, conn:
            for row in rows:
                conn.execute('INSERT INTO auto_model_scores (' + columns + ') VALUES (?,?,?,?,?,?,?,?,?)',
                             tuple(row[key] for key in SCORE_COLUMNS))
        actual = self.snapshot('seeded-scores')
        require(check_scores(actual, [score_key(row) for row in rows]))
        require([] if indexed(actual) == {score_key(row): row for row in rows} else ['score fixture seed did not round-trip'])
        self.row['stored_score_control_applied'] = True
        return True

    def ask(self, token_name, route, delay=0.03):
        self.stage = 'explicit-business-request'
        ordinal = len(self.row['responses']) + 1
        expected = None
        if route is not None:
            channel, model = route
            expected = ('POST', f'/channel-{channel}/v1/chat/completions', channel, model)
            self.row['expected_calls'].append(expected)
        intent = {'ordinal': ordinal, 'client_model': 'auto', 'token_model_limit': token_name,
                  'expected_upstream': expected, 'sent_ns': time.time_ns()}
        self.row['business_requests'].append(intent)
        self.event('explicit-business-request', intent)
        payload = {'model': 'auto', 'stream': False, 'max_tokens': 32,
                   'messages': [{'role': 'user', 'content': 'Synthetic passive score fixture.'}]}
        with self.state.upstream.lock:
            begin = len(self.state.upstream.calls)
            self.state.upstream.delay = delay
        response = http_request(self.gateway.port, 'POST', '/v1/chat/completions',
                                json.dumps(payload).encode(), {'Content-Type': 'application/json',
                                'Authorization': 'Bearer sk-' + self.tokens[token_name], 'Connection': 'close'})
        (self.root / f'response-{ordinal}.bin').write_bytes(base64.b64decode(response['body_b64']))
        time.sleep(1)  # Include deferred billing/feedback after client delivery in the measured bracket.
        self.gateway.ensure_live()
        response['settled_ns'] = time.time_ns()
        response['settled_mono_ns'] = time.monotonic_ns()
        if route is not None:
            calls = self.state.upstream.snapshot()[begin:]
            require(check_calls(calls, [expected]))
            response['sample_latency_ms'] = measured_latency_bounds(response, calls[0])
            response['upstream_sequence'] = calls[0]['sequence']
        self.row['responses'].append(response)
        self.event('response', response)
        if route is not None:
            require(check_response(response, REPLIES[route[1]]))
        else:
            require([] if response['status'] >= 400 else ['all-cooling request unexpectedly succeeded'])
        return response

    def wait_scores(self, label, keys, changed=(), response=None):
        self.stage = label
        deadline = time.monotonic() + 12  # More than two default 5s flush periods.
        while True:
            self.gateway.ensure_live()
            snapshot = read_scores(self.root)
            invalid = check_scores(snapshot)
            if invalid:
                self.row['scores'].append({'label': label, 'snapshot': snapshot})
                self.event('scores', self.row['scores'][-1])
                require(invalid)
            rows = indexed(snapshot)
            fresh = response is None or all(key in rows and rows[key]['observed_at'] >= response['started_ns'] for key in changed)
            if set(rows) == set(keys) and fresh:
                break
            if time.monotonic() >= deadline:
                self.row['scores'].append({'label': label, 'snapshot': snapshot})
                self.event('scores', self.row['scores'][-1])
                require(check_scores(snapshot, keys) + ['periodic score write not confirmed while gateway was running'])
            time.sleep(0.1)
        self.row['scores'].append({'label': label, 'snapshot': snapshot, 'gateway_running': True})
        self.event('scores', self.row['scores'][-1])
        require(check_scores(snapshot, keys))
        if response is not None:
            for key in changed:
                require([] if response['started_ns'] <= rows[key]['observed_at'] <= response['settled_ns'] + 1000000000
                        else ['score timestamp was refreshed outside the actual business observation'])
        return snapshot

    def first(self, snapshot, keys):
        rows = indexed(snapshot)
        for key in keys:
            require(check_first_score(rows[key]))

    def updates(self, before, after, keys, distinct_latency=False):
        previous, current = indexed(before), indexed(after)
        for key in keys:
            response = self.row['responses'][-1]
            sample = response['sample_latency_ms']
            window = [response['started_ns'], response['settled_ns']]
            predicted = latency_update_bounds(previous[key], sample, window)
            if not window[0] <= current[key]['observed_at'] <= window[1]:
                raise RuntimeError('business observation escaped independent latency measurement window')
            if distinct_latency and not (predicted[0] > sample[1] or predicted[1] < sample[0]):
                raise RuntimeError('latency fixture cannot distinguish continuation from sample reset')
            evidence = {'key': key, 'prior_latency_ms': previous[key]['latency_ms'],
                        'prior_observed_at': previous[key]['observed_at'], 'sample_latency_ms': sample,
                        'observation_window_ns': window, 'expected_latency_ms': predicted,
                        'actual_latency_ms': current[key]['latency_ms'], 'distinct_latency': distinct_latency}
            self.row.setdefault('latency_checks', []).append(evidence)
            self.event('latency-update-check', evidence)
            require(check_update(previous[key], current[key], sample_latency_ms=sample,
                                 observation_window_ns=window))
        for key in set(previous) - set(keys):
            require([] if current.get(key) == previous[key] else ['independent score row changed without feedback'])


def case_idle(ctx, negative_control):
    ctx.prepare()
    ctx.start('configured-startup')
    ctx.idle('two-worker-ticks-without-business', 11, inject=negative_control)
    snapshot = ctx.snapshot('idle-after-two-ticks')
    if snapshot['present']:
        require(check_scores(snapshot, []))
    ctx.gateway.stop()
    ctx.start('idle-restart')
    ctx.idle('worker-tick-after-idle-restart', 6)
    snapshot = ctx.snapshot('idle-after-restart')
    if snapshot['present']:
        require(check_scores(snapshot, []))


def case_feedback(ctx, negative_control):
    ctx.prepare()
    ctx.start('feedback-startup')
    a, b, c = model_key('score-a', 1), model_key('score-b', 1), model_key('score-c', 2)
    one, two = channel_key(1), channel_key(2)
    response = ctx.ask('score-a', (1, 'score-a'), delay=3)
    first = ctx.wait_scores('first-periodic-write', [a, one], [a, one], response)
    ctx.first(first, [a, one])
    response = ctx.ask('score-b', (1, 'score-b'), delay=3)
    second = ctx.wait_scores('independent-sibling-model', [a, b, one], [b, one], response)
    ctx.first(second, [b])
    ctx.updates(first, second, [one])
    response = ctx.ask('score-c', (2, 'score-c'), delay=3)
    keys = [a, b, c, one, two]
    third = ctx.wait_scores('independent-second-channel', keys, [c, two], response)
    ctx.first(third, [c, two])
    ctx.updates(second, third, [])
    ctx.idle('saved-evidence-idle-no-refresh', 6)
    stable = ctx.snapshot('confirmed-save-before-stop')
    require([] if stable['rows'] == third['rows'] else ['idle flush changed last actual observation or score'])
    ctx.row['periodic_write_confirmed_before_stop'] = True
    ctx.gateway.stop()  # No claim that SIGTERM runs Go defers or flushes unsaved scores.
    ctx.start('restore-after-confirmed-periodic-write')
    restored = ctx.snapshot('restored-before-business')
    require([] if restored['rows'] == stable['rows'] else ['restart changed still-fresh stored summaries'])
    response = ctx.ask('score-b', (1, 'score-b'))
    updated = ctx.wait_scores('decisive-post-restart-update', keys, [b, one], response)
    ctx.updates(stable, updated, [b, one], distinct_latency=True)
    ctx.idle('post-reload-business-idle', 6)
    require([] if ctx.snapshot('post-reload-idle-scores')['rows'] == updated['rows']
            else ['idle recovery refreshed observed_at or changed evidence'])


def case_startup_ranking(ctx, negative_control):
    ctx.prepare(priorities={1: 0, 2: 0})
    at = time.time_ns() - 1000000000
    seeds = [stored(channel_key(1), 0.97, 12, 4000, at), stored(channel_key(2), 0.05, 12, 4000, at),
             stored(model_key('score-a', 1), 0.05, 12, 4000, at),
             stored(model_key('score-b', 1), 0.60, 12, 4000, at),
             stored(model_key('score-c', 2), 0.99, 12, 4000, at)]
    ctx.seed_scores(seeds)
    before = ctx.snapshot('fresh-ranking-before-start')
    ctx.row['ranking_expectation'] = 'channel 1 beats channel 2 despite model c; model b beats a inside channel 1'
    ctx.start('restore-before-first-http-business')
    response = ctx.ask('all', (1, 'score-b'))  # No warmup request and no cold-tie negative proof.
    updated = ctx.wait_scores('ranking-and-restored-count-update', [score_key(row) for row in seeds],
                              [model_key('score-b', 1), channel_key(1)], response)
    # A lucky cold tie cannot pass these continuation assertions if restore is ablated.
    ctx.updates(before, updated, [model_key('score-b', 1), channel_key(1)], distinct_latency=True)
    ctx.idle('ranking-after-business-idle', 6)


def case_expired(ctx, negative_control):
    ctx.prepare(routes={1: ('score-a',), 2: ('score-c',)})
    at = time.time_ns() - (DECAY_WINDOW_MS + 1000) * 1000000  # 刚过衰减窗口,历史必须已经失效。
    keys = [model_key('score-a', 1), channel_key(1), model_key('score-c', 2), channel_key(2)]
    ctx.seed_scores([stored(key, 0.99, 19, 60000, at) for key in keys])
    ctx.start('expired-score-restore')
    ctx.wait_scores('expired-snapshots-pruned', [])
    ctx.idle('expired-restore-idle-worker-ticks', 11)
    response = ctx.ask('score-a', (1, 'score-a'))
    fresh_keys = [model_key('score-a', 1), channel_key(1)]
    updated = ctx.wait_scores('expired-history-not-revived', fresh_keys, fresh_keys, response)
    ctx.first(updated, fresh_keys)  # Deterministic n=1/score=.65/fresh latency, never a cold tie.


def case_priority(ctx, negative_control):
    ctx.prepare(routes={1: ('score-a',), 2: ('score-c',)}, priorities={1: 100, 2: 0})
    at = time.time_ns() - 1000000000
    seeds = [stored(key, score, 12, 10, at) for key, score in
             ((model_key('score-a', 1), 0.01), (channel_key(1), 0.01),
              (model_key('score-c', 2), 0.99), (channel_key(2), 0.99))]
    enhanced = ctx.seed_scores(seeds, legacy_control=True)
    before = ctx.snapshot('priority-before-start')
    ctx.start('priority-startup')
    response = ctx.ask('all', (1, 'score-a'))
    if enhanced:
        updated = ctx.wait_scores('priority-still-outranks-restored-score', [score_key(row) for row in seeds],
                                  [model_key('score-a', 1), channel_key(1)], response)
        ctx.updates(before, updated, [model_key('score-a', 1), channel_key(1)])
    ctx.gateway.stop()
    ctx.start('priority-restart')
    ctx.ask('all', (1, 'score-a'))
    ctx.idle('priority-control-idle', 6)


def case_strict_cooldown(ctx, negative_control):
    ctx.prepare()
    at = time.time_ns() - 1000000000
    seeds = [stored(key, score, 12, 10, at) for key, score in
             ((model_key('score-a', 1), 0.99), (model_key('score-b', 1), 0.10),
              (channel_key(1), 0.99), (model_key('score-c', 2), 0.90), (channel_key(2), 0.90))]
    enhanced = ctx.seed_scores(seeds, legacy_control=True)
    before = ctx.snapshot('cooldown-before-start')
    until = int(time.time()) + 900
    original = seed_cooldowns(ctx.root, [('score-a', 1)], until)
    ctx.row['cooldowns'] = [{'label': 'one-pair-seeded', 'rows': original}]
    ctx.start('excellent-score-but-pair-cooling')
    response = ctx.ask('all', (1, 'score-b'))
    if enhanced:
        updated = ctx.wait_scores('sibling-allowed-cooled-pair-excluded', [score_key(row) for row in seeds],
                                  [model_key('score-b', 1), channel_key(1)], response)
        ctx.updates(before, updated, [model_key('score-b', 1), channel_key(1)])
    after = cooldowns(ctx.root)
    ctx.row['cooldowns'].append({'label': 'after-sibling-success', 'rows': after})
    require([] if after == original else ['sibling success changed another pair cooldown'])
    ctx.gateway.stop()
    ctx.start('strict-cooldown-restart')
    ctx.ask('all', (1, 'score-b'))
    require([] if cooldowns(ctx.root) == original else ['pair cooldown changed across restart'])
    ctx.gateway.stop()
    all_cooling = seed_cooldowns(ctx.root, [('score-b', 1), ('score-c', 2)], until)
    ctx.row['cooldowns'].append({'label': 'all-pairs-cooling', 'rows': all_cooling})
    require([] if [(row['group'], row['model'], row['channel_id']) for row in all_cooling]
            == [('default', 'score-a', 1), ('default', 'score-b', 1), ('default', 'score-c', 2)]
            else ['cooldown fixture keys are not strictly pair scoped'])
    ctx.start('all-cooling-startup')
    ctx.ask('all', None)  # Must not fail open, even with excellent persisted scores.
    ctx.idle('all-cooling-idle', 6)
    require([] if cooldowns(ctx.root) == all_cooling else ['blocked request changed cooldown rows'])


def case_cooldown_expiry(ctx, negative_control):
    ctx.prepare(routes={1: ('score-a',), 2: ('score-c',)})
    at = time.time_ns() - 1000000000
    seeds = [stored(key, score, 12, 10, at) for key, score in
             ((model_key('score-a', 1), 0.99), (channel_key(1), 0.99),
              (model_key('score-c', 2), 0.10), (channel_key(2), 0.10))]
    enhanced = ctx.seed_scores(seeds, legacy_control=True)
    until = int(time.time()) + 8
    ctx.row['cooldown_until'] = until
    ctx.row['cooldowns'] = [{'label': 'short-active-pair',
                            'rows': seed_cooldowns(ctx.root, [('score-a', 1)], until)}]
    ctx.start('expiry-startup')
    if time.time() >= until - 1:
        raise RuntimeError('short cooldown fixture expired before its first business request')
    backup = ctx.ask('all', (2, 'score-c'))
    ctx.idle('cooldown-expiry-and-idle-recovery', max(11, until + 6 - time.time()))
    require([] if time.time() > until else ['idle observation did not cross cooldown expiry'])
    ctx.row['cooldowns'].append({'label': 'expired-before-later-business', 'rows': cooldowns(ctx.root)})
    before = ctx.snapshot('expired-before-later-business')
    if enhanced:
        before = ctx.wait_scores('backup-periodic-write', [score_key(row) for row in seeds],
                                 [model_key('score-c', 2), channel_key(2)], backup)
        unchanged = indexed(before)
        for row in seeds:
            if row['channel_id'] == 1:
                require([] if unchanged[score_key(row)] == row else ['idle expiry refreshed excellent stored evidence'])
    response = ctx.ask('all', (1, 'score-a'))
    if enhanced:
        updated = ctx.wait_scores('later-explicit-request-reassesses-expired-pair', [score_key(row) for row in seeds],
                                  [model_key('score-a', 1), channel_key(1)], response)
        ctx.updates(before, updated, [model_key('score-a', 1), channel_key(1)])
    cleared = cooldowns(ctx.root)
    ctx.row['cooldowns'].append({'label': 'cleared-by-real-success', 'rows': cleared})
    require([] if cleared == [] else ['later real success failed to clear expired pair cooldown'])


CASE_HANDLERS = {
    'idle-no-probes': case_idle,
    'feedback-save-reload': case_feedback,
    'startup-two-level-ranking': case_startup_ranking,
    'expired-scores-neutral': case_expired,
    'priority-over-score': case_priority,
    'strict-pair-cooldown': case_strict_cooldown,
    'cooldown-expiry-passive': case_cooldown_expiry,
}

# Full successful-case manifest: business requests, upstream calls, idle seconds.
# These declarations do not grow dynamically with whichever steps happened to run.
CASE_COUNTS = {
    'idle-no-probes': (0, 0, 17),
    'feedback-save-reload': (4, 4, 12),
    'startup-two-level-ranking': (1, 1, 6),
    'expired-scores-neutral': (1, 1, 11),
    'priority-over-score': (2, 2, 6),
    'strict-pair-cooldown': (3, 2, 6),
    'cooldown-expiry-passive': (2, 2, 11),
}


def run_case(state, cache, name, negative_control=False):
    row = {'id': cache + ':' + name, 'case': name, 'cache': cache,
           'failures': [], 'fixture_errors': [], 'cleanup': [], 'calls': [], 'expected_calls': [],
           'scores': [], 'responses': [], 'windows': [], 'business_requests': [], 'started_ns': time.time_ns()}
    ctx = Case(state, cache, name, row)
    with state.upstream.lock:
        state.upstream.case_id = row['id']
        begin, error_begin = len(state.upstream.calls), len(state.upstream.errors)
    try:
        CASE_HANDLERS[name](ctx, negative_control)
        business, calls, idle_seconds = CASE_COUNTS[name]
        require([] if len(row['business_requests']) == len(row['responses']) == business
                and len(row['expected_calls']) == calls else ['incomplete literal case request/response count'])
        require([] if sum(window.get('duration_seconds', 0) for window in row['windows']) >= idle_seconds
                else ['incomplete literal case idle observation window'])
    except BehaviorFailure as exc:
        row['failures'].append(ctx.stage + ': ' + str(exc))
    except Exception as exc:
        row['fixture_errors'].append(ctx.stage + ': ' + repr(exc))
    finally:
        if ctx.gateway is not None:
            ctx.gateway.stop()
        row['calls'] = state.upstream.snapshot()[begin:]
        row['failures'].extend(check_calls(row['calls'], row['expected_calls']))
        with state.upstream.lock:
            row['fixture_errors'].extend(state.upstream.errors[error_begin:])
            state.upstream.case_id = None
        row['ended_ns'] = time.time_ns()
        row['actual_upstream_call_count'] = len(row['calls'])
        row['expected_upstream_call_count'] = len(row['expected_calls'])
        row['declared_complete_counts'] = dict(zip(('business_requests', 'upstream_calls', 'idle_seconds'), CASE_COUNTS[name]))
    row['status'] = verdict(row['failures'], row['fixture_errors'])
    return row


def verdict(failures, errors):
    return 'ERROR' if errors else 'FAIL' if failures else 'PASS'


def write_json(path, value):
    temporary = path.with_suffix(path.suffix + '.new')
    with temporary.open('w', encoding='utf-8') as out:
        json.dump(json_evidence(value), out, ensure_ascii=False, allow_nan=False, indent=2)
        out.write('\n')
        out.flush()
        os.fsync(out.fileno())
    temporary.replace(path)


def execute_matrix(state, args):
    modes = ('false', 'true') if args.cache == 'both' else (args.cache,)
    ids = [cache + ':' + case for cache in modes for case in args.cases]
    report = {'source_binary': str(args.binary), 'executed_binary': str(state.binary),
              'sha256': state.sha256, 'isolation': state.isolation,
              'expected_cases': len(ids), 'expected_ids': ids, 'results': [],
              'failures': [], 'fixture_errors': [], 'stub_stopped': False,
              'negative_control': args.negative_control, 'started_ns': time.time_ns()}
    report['negative_control_kind'] = args.negative_control_kind if args.negative_control else None
    try:
        write_json(state.base / 'results.json', report)
        for cache in modes:
            for case in args.cases:
                try:
                    row = run_case(state, cache, case, negative_control=(
                        args.negative_control_kind if args.negative_control else False))
                except Exception as exc:
                    # Ordinary AssertionError from fixture setup is NOT a behavioral RED.
                    row = {'id': cache + ':' + case, 'failures': [], 'calls': [], 'cleanup': [],
                           'fixture_errors': ['case infrastructure: ' + repr(exc)]}
                row['status'] = verdict(row['failures'], row['fixture_errors'])
                report['results'].append(row)
                append_jsonl(state.base / 'cases.jsonl', row)  # Durable before the next case.
                write_json(state.base / 'results.json', report)
                print(json.dumps({key: row[key] for key in
                                  ('id', 'status', 'failures', 'fixture_errors')}), flush=True)
    except BaseException as exc:
        report['fixture_errors'].append('matrix infrastructure: ' + repr(exc))
    finally:
        report['fixture_errors'].extend(stop_upstream(state.upstream, state.worker))
        report['stub_stopped'] = not state.worker.is_alive() and state.upstream.socket.fileno() == -1
        report['fixture_errors'].extend(state.upstream.errors)
        report['fixture_errors'].extend(check_manifest(ids, report['results']))
        observed = state.upstream.snapshot()
        covered = [call['sequence'] for row in report['results'] for call in row['calls']]
        if covered != [call['sequence'] for call in observed]:
            report['failures'].append('upstream requests exist outside complete case evidence')
        report['upstream_calls'] = observed
        report['upstream_call_count'] = len(observed)
        report['ended_ns'] = time.time_ns()
        try:
            write_json(state.base / 'results.json', report)
        except Exception as exc:
            report['fixture_errors'].append('final evidence write: ' + repr(exc))
    errors = bool(report['fixture_errors']) or any(row['fixture_errors'] for row in report['results'])
    failures = bool(report['failures']) or any(row['failures'] for row in report['results'])
    code = 2 if errors else int(failures)
    print(json.dumps({'status': verdict(failures, errors), 'results': str(state.base / 'results.json'),
        'expected_cases': len(ids), 'cases': len(report['results']), 'sha256': state.sha256,
        'passed': sum(row['status'] == 'PASS' for row in report['results']),
        'failed': sum(row['status'] == 'FAIL' for row in report['results']),
        'errors': sum(row['status'] == 'ERROR' for row in report['results']),
        'fixture_errors': report['fixture_errors'], 'failures': report['failures']}), flush=True)
    return code


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('binary', type=Path, nargs='?')
    parser.add_argument('--cache', choices=('false', 'true', 'both'), default='both')
    parser.add_argument('--cases', nargs='+', choices=CASES, default=list(CASES))
    parser.add_argument('--list-cases', action='store_true')
    parser.add_argument('--negative-control', action='store_true',
                        help='inject one synthetic unsolicited GET in the idle case; expect FAIL/1')
    parser.add_argument('--negative-control-kind', choices=('ordinary', 'parser431'), default='ordinary',
                        help='with --negative-control: ordinary GET or oversized-header GET rejected as 431')
    args = parser.parse_args(argv)
    if args.list_cases:
        print(json.dumps({'cases': CASES}))
        return 0
    if args.binary is None:
        parser.error('binary is required unless --list-cases is used')
    if len(args.cases) != len(set(args.cases)):
        parser.error('duplicate selected cases')
    if args.negative_control and 'idle-no-probes' not in args.cases:
        parser.error('--negative-control requires idle-no-probes')
    if args.negative_control_kind != 'ordinary' and not args.negative_control:
        parser.error('--negative-control-kind requires --negative-control')
    try:
        require_namespace()
    except Exception as exc:
        parser.error(str(exc))
    try:
        state = bootstrap(args.binary)
    except Exception as exc:
        print(json.dumps({'status': 'ERROR', 'fixture_errors': ['bootstrap: ' + repr(exc)]}), flush=True)
        return 2
    return execute_matrix(state, args)


if __name__ == '__main__':
    raise SystemExit(main())
