#!/usr/bin/env python3
"""Real-binary auto retry/attribution regression, synthetic loopback only.

Run from a loopback-only network namespace, for example:
  unshare --net -- sh -c 'ip link set dev lo up && exec python3 -B scripts/test-auto-routing-policy.py "$@"' sh /absolute/binary --cache false
Fresh DBs, logs and incremental results live under TMPDIR, never production.
Behavior mismatches exit 1; fixture/bootstrap faults exit 2. No real providers.
"""
import argparse
import contextlib
import errno
import hashlib
import http.server
import importlib.util
import json
import os
import pathlib
import secrets
import shutil
import socket
import sqlite3
import sys
import tempfile
import threading
import time
from typing import cast
import urllib.error
import urllib.request

SPEC = importlib.util.spec_from_file_location(
    'auto_sse_helpers', pathlib.Path(__file__).with_name('test-auto-sse-errors.py'))
assert SPEC is not None and SPEC.loader is not None
h = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(h)
DEFAULT_CODES = '401-403,413,429,500-599'


def case(name, status, code, message, channels, *, codes=DEFAULT_CODES, **extra):
    return dict(name=name, status=status, code=code, message=message,
                channels=channels, codes=codes, **extra)


def cases():
    # Literal behavioral expectations, not a copy of the production classifier.
    return [
        case('account401', 401, 'invalid_api_key', 'Invalid API key', [1, 2]),
        case('account402', 402, 'insufficient_quota', 'Account balance exhausted', [1, 2]),
        case('account403', 403, 'access_denied', 'Credential access forbidden', [1, 2]),
        case('model403', 403, 'model_access_denied', 'Access to this model is denied', [1, 1, 1, 2]),
        case('account429', 429, 'rate_limit_exceeded', 'Account rate limit exceeded', [1, 2], retry_after='60'),
        case('unknown429', 429, 'rate_limit_exceeded', 'Too many requests', [1, 2]),
        case('account429-model-hint', 429, 'rate_limit_exceeded', "Account-wide rate limit exceeded for model 'gpt-4o'.", [1, 2], retries=1),
        case('model429', 429, 'model_rate_limit_exceeded', 'Rate limit exceeded for this model', [1, 1, 1, 2]),
        case('model-only-account-qualifier', 429, 'model_rate_limit_exceeded',
             "Your account rate limit is exceeded only for model 'gpt-4o'; other models remain available.",
             [1, 1, 1, 2], codes='429', client_status=200),
        case('account-owner-conditional', 429, 'rate_limit_exceeded',
             "Your account rate limit exceeded for model 'gpt-4o'; only if the estimate is correct.",
             [1, 2], codes='429', client_status=200),
        case('account-owner-conditional-with-is', 429, 'rate_limit_exceeded',
             "Your account rate limit is exceeded for model 'gpt-4o'; only if the estimate is correct.",
             [1, 2], codes='429', client_status=200),
        case('account-owner-affirmative', 429, 'rate_limit_exceeded',
             "Your account rate limit exceeded for model 'gpt-4o'.",
             [1, 1, 1, 2], codes='429', client_status=200),
        case('account-owner-model-first', 429, 'rate_limit_exceeded',
             "Your account rate limit for model 'gpt-4o' exceeded.",
             [1, 1, 1, 2], codes='429', client_status=200),
        case('account-owner-structured-code', 429, 'model_rate_limit_exceeded',
             "Your account rate limit exceeded for model 'gpt-4o'; only if the estimate is correct.",
             [1, 1, 1, 2], codes='429', client_status=200),
        case('account-owner-excluded-status', 429, 'rate_limit_exceeded',
             "Your account rate limit exceeded for model 'gpt-4o'; only if the estimate is correct.",
             [1], codes='503', client_status=429),
        case('account-owner-later-model-clause', 429, 'rate_limit_exceeded',
             "Your account rate limit exceeded for model 'gpt-4o'; only if the estimate is correct. This model is overloaded.",
             [1, 1, 1, 2], codes='429', client_status=200),
        case('excluded429', 429, 'rate_limit_exceeded', 'Account rate limit exceeded', [1], codes='500-599', client_status=429),
        case('generic500', 500, 'server_error', 'Upstream unavailable', [1, 2]),
        case('generic503', 503, 'server_error', 'Upstream unavailable', [1, 2]),
        case('model503', 503, 'model_overloaded', 'This model is overloaded', [1, 1, 1, 2]),
        case('negated-model-overload', 503, 'upstream_error',
             'No model is overloaded; upstream maintenance is in progress.',
             [1, 2], codes='503', client_status=200),
        case('excluded-model503', 503, 'model_overloaded', 'This model is overloaded', [1], codes='429', client_status=503),
        case('excluded408', 408, 'request_timeout', 'Upstream request timeout', [1], client_status=408),
        case('enabled408', 408, 'request_timeout', 'Upstream request timeout', [1, 2], codes=DEFAULT_CODES + ',408'),
        case('unrelated-semicolon-excluded', 400, 'invalid_request',
             'This endpoint does not support streaming; a validation error occurred with this model.',
             [1], codes='429', client_status=400),
        case('unrelated-semicolon-enabled', 400, 'invalid_request',
             'This endpoint does not support streaming; a validation error occurred with this model.',
             [1, 2], codes='400', client_status=200),
        case('unrelated-colon-excluded', 400, 'invalid_request',
             'This endpoint does not support streaming: a validation error occurred with this model.',
             [1], codes='429', client_status=400),
        case('unrelated-colon-enabled', 400, 'invalid_request',
             'This endpoint does not support streaming: a validation error occurred with this model.',
             [1, 2], codes='400', client_status=200),
        case('generic400', 400, 'invalid_request', 'Invalid request field independent of model', [1], client_status=400),
        case('model400', 400, 'model_not_found', 'Requested model not found', [1, 1, 1, 2], codes='429'),
        case('negated-model-compat', 400, 'invalid_request',
             'No model is unsupported. The request JSON is malformed.',
             [1], codes='429', client_status=400),
        case('generic404', 404, 'route_not_found', 'Endpoint not found', [1], client_status=404),
        case('model404', 404, 'model_not_found', 'Requested model does not exist', [1, 1, 1, 2]),
        case('generic405', 405, 'method_not_allowed', 'HTTP method not allowed', [1], client_status=405),
        case('generic422', 422, 'invalid_request', 'Invalid request structure', [1], client_status=422),
        case('model422', 422, 'unsupported_model_feature', 'This model does not support the requested feature', [1, 1, 1, 2]),
        case('generic501', 501, 'not_implemented', 'Endpoint is not implemented', [1, 2]),
        case('excluded501', 501, 'not_implemented', 'Endpoint is not implemented', [1], codes='429', client_status=501),
        case('upstream413', 413, 'payload_too_large', 'Upstream body size limit exceeded', [1, 2]),
        case('context400', 400, 'context_length_exceeded', 'Maximum context length exceeded', [1, 1, 1, 2]),
        case('context-capacity-only', 400, 'invalid_request',
             "This model's maximum context length is 8192 tokens; the request JSON is malformed.",
             [1], codes='429', client_status=400),
        case('account-model-only-colon', 429, 'model_rate_limit_exceeded',
             "Your account rate limit is exceeded: only for model 'gpt-4o'; other models remain available.",
             [1, 1, 1, 2], codes='429', client_status=200),
        case('written200-error', 200, 'upstream_error', 'Explicit protocol failure', [1], style='sse_error', client_status=200),
        case('budget-zero', 503, 'server_error', 'Upstream unavailable', [1], retries=0, client_status=503),
        case('budget-one', 503, 'server_error', 'Upstream unavailable', [1, 2], retries=1),
        case('many-models503', 503, 'server_error', 'Upstream unavailable', [1, 2], model_count=55),
        case('model-budget', 404, 'model_not_found', 'Requested model not found', [1, 1, 1], retries=2, client_status=404),
        case('local-max', 200, '', '', [], style='healthy', first_model='gpt-5.4', model_count=1,
             effort='max', client_status=400, cooldown_count=0),
        case('local-high-control', 200, '', '', [1], style='healthy', first_model='gpt-5.4', model_count=1,
             effort='high', cooldown_count=0),
        case('second-local-reset', 503, 'server_error', 'Upstream unavailable', [1],
             good_model='gpt-5.4', effort='max', client_status=400),
        case('connection-refused', 503, 'server_error', 'Unused server response', [2],
             refuse=True, cooldown_count=1, first_model='probe-refused', model_count=1),
        case('request-only-skip', 401, 'invalid_api_key', 'Invalid API key', [1, 2], followup=True),
        case('restart-pair-only', 503, 'server_error', 'Upstream unavailable', [1, 2], followup=True, restart=True),
    ]


def read_body(handler):
    if handler.headers.get('Transfer-Encoding', '').lower() != 'chunked':
        return handler.rfile.read(int(handler.headers.get('Content-Length', '0')))
    chunks = []
    while True:
        line = handler.rfile.readline()
        size = int(line.split(b';', 1)[0].strip(), 16)
        if not size:
            while handler.rfile.readline() not in (b'\r\n', b'\n', b''):
                pass
            return b''.join(chunks)
        chunk = handler.rfile.read(size)
        if len(chunk) != size or handler.rfile.read(2) != b'\r\n':
            raise ValueError('invalid chunk framing')
        chunks.append(chunk)


def route_failure(call, spec, bad_models):
    channel, model, path = call['channel'], call['model'], call['path']
    declared = (bad_models if channel == 1 and not spec.get('refuse') else
                [spec.get('good_model', 'probe-ok')] if channel == 2 else ())
    if (path != f'/channel-{channel}/v1/chat/completions'
            or not isinstance(model, str) or model not in declared):
        return f'undeclared upstream route/path: channel={channel!r}, model={model!r}, path={path!r}'
    return None


class Stub(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        state = cast(Server, self.server)
        try:
            payload = json.loads(read_body(self))
            model = payload.get('model') if isinstance(payload, dict) else None
            # Transport identity must never be inferred from the requested model.
            channel = next((c for c in (1, 2) if self.path.startswith(f'/channel-{c}/')), None)
            call = {'channel': channel, 'model': model, 'at': time.monotonic(), 'path': self.path}
            with state.lock:
                spec = state.spec
                bad = channel == 1 and not state.recovered
                state.calls.append(call)
                invalid = route_failure(call, spec, state.bad_models)
            style = spec.get('style', 'http') if bad else 'healthy'
            status = spec['status'] if style == 'http' else 200
            if invalid:
                status, content_type = 400, 'application/json'
                body = json.dumps({'error': {'message': invalid, 'type': 'invalid_request_error',
                                            'code': 'fixture_route_mismatch'}}).encode()
            elif style == 'http':
                body = json.dumps({'error': {'message': spec['message'],
                                            'type': 'invalid_request_error', 'code': spec['code']}}).encode()
                content_type = 'application/json'
            else:
                frame = ({'error': {'message': spec['message'], 'type': 'upstream_error', 'code': spec['code']}}
                         if style == 'sse_error' else h.fixture_chunk('normal', model))
                body = ('data: ' + json.dumps(frame) + '\n\ndata: [DONE]\n\n').encode()
                content_type = 'text/event-stream'
            self.send_response(status)
            self.send_header('Content-Type', content_type)
            self.send_header('Content-Length', str(len(body)))
            if status == 429 and spec.get('retry_after'):
                self.send_header('Retry-After', spec['retry_after'])
            self.end_headers()
            self.wfile.write(body)
            self.wfile.flush()
        except Exception as exc:
            with state.lock:
                state.fixture_errors.append(type(exc).__name__ + ': ' + str(exc))
            self.close_connection = True


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, spec, bad_models):
        self.spec, self.bad_models = spec, set(bad_models)
        self.lock = threading.Lock()
        self.calls, self.fixture_errors = [], []
        self.recovered = False
        super().__init__(('127.0.0.1', 0), Stub)


@contextlib.contextmanager
def gateway(binary, root, port, cache, cleanup):
    process, log, _ = h.start(binary, root, port, cache)
    try:
        yield
    finally:
        h.stop(process, log)
        cleanup.append({'pid': process.pid, 'reaped': process.poll() is not None, 'log_closed': log.closed})


def seed(root, token, upstream, spec, bad_models, refusal_port=None):
    h.seed(root, token, upstream, 'normal', secrets.token_hex(24))
    good_model = spec.get('good_model', 'probe-ok')
    with contextlib.closing(sqlite3.connect(root / 'one-api.db')) as db, db:
        db.execute('DELETE FROM abilities')
        for channel, names, priority in ((1, bad_models, 100), (2, [good_model], 0)):
            db.execute('UPDATE channels SET models=?,base_url=? WHERE id=?',
                       (','.join(names), upstream + f'/channel-{channel}', channel))
            for model in names:
                db.execute('INSERT INTO abilities ("group",model,channel_id,enabled,priority,weight,tag) '
                           "VALUES ('default',?,?,1,?,0,'')", (model, channel, priority))
        for key, value in {'ModelRatio': json.dumps({m: 1 for m in bad_models + [good_model]}),
                           'RetryTimes': str(spec.get('retries', 50)),
                           'AutomaticRetryStatusCodes': spec['codes']}.items():
            db.execute('UPDATE options SET value=? WHERE key=?', (value, key))
        if spec.get('refuse'):
            if refusal_port is None:
                raise ValueError('a reserved refusal port is required')
            db.execute('UPDATE channels SET base_url=? WHERE id=1',
                       ('http://127.0.0.1:' + str(refusal_port),))


def ask(root, port, token, spec, index):
    payload = {'model': 'auto', 'messages': [{'role': 'user', 'content': 'Synthetic routing fixture.'}],
               'max_tokens': 32, 'stream': True}
    if 'effort' in spec:
        payload['reasoning_effort'] = spec['effort']
    request = urllib.request.Request('http://127.0.0.1:' + str(port) + '/v1/chat/completions',
                                     data=json.dumps(payload).encode(),
                                     headers={'Authorization': 'Bearer sk-' + token, 'Content-Type': 'application/json'})
    try:
        response = urllib.request.urlopen(request, timeout=20)
    except urllib.error.HTTPError as exc:
        response = exc
    with response:
        body = response.read()
        result = {'status': response.status, 'body': body.decode(errors='replace')}
    (root / ('response-' + str(index) + '.txt')).write_bytes(body)
    time.sleep(0.15)  # Deferred feedback follows the final response write.
    return result


def run_case(binary, root, spec, cache):
    root.mkdir()
    count = spec.get('model_count', 3)
    bad_models = [spec['first_model']] if 'first_model' in spec else [f'probe-bad-{i:02}' for i in range(count)]
    server = Server(spec, bad_models)
    thread = threading.Thread(target=server.serve_forever, kwargs={'poll_interval': 0.05}, daemon=True)
    thread.start()
    cleanup, responses, initial_calls, all_calls, cooldowns = [], [], [], [], []
    failures, errors = [], []
    after_restart, refusal, refused_socket = None, None, None
    try:
        if spec.get('refuse'):
            # Keep the port bound, but never listen: neither the gateway nor a
            # competing fixture can claim it while the request is in flight.
            refused_socket = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            refused_socket.bind(('127.0.0.1', 0))
            refusal = {'port': refused_socket.getsockname()[1]}
            with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
                probe.settimeout(1)
                refusal['connect_errno'] = probe.connect_ex(('127.0.0.1', refusal['port']))
            if refusal['connect_errno'] != errno.ECONNREFUSED:
                raise RuntimeError('reserved endpoint did not refuse connections')
        port, token = h.free_port(), secrets.token_hex(24)
        if refusal and refusal['port'] in (port, server.server_port):
            raise RuntimeError('refusal endpoint collides with a listener')
        with gateway(binary, root, port, cache, cleanup):
            pass
        seed(root, token, 'http://127.0.0.1:' + str(server.server_port), spec, bad_models,
             refusal_port=refusal['port'] if refusal else None)
        with gateway(binary, root, port, cache, cleanup):
            responses.append(ask(root, port, token, spec, 1))
            with server.lock:
                initial_calls = list(server.calls)
            cooldowns = h.rows(root)
            if refusal:
                endpoint = '127.0.0.1:' + str(refusal['port'])
                transport_rows, transport_lines = [], []
                for _ in range(40):
                    transport_rows = [row for row in h.usage_error_rows(root)
                                      if row['channel'] == 1 and row['model_name'] == bad_models[0]
                                      and json.loads(row['other']).get('error_code') == 'do_request_failed']
                    # Persistent errors deliberately hide the transport detail.
                    # Correlate their request ID with the real process diagnostic.
                    transport_lines = [line for path in root.glob('process-*.log') for line in path.read_text().splitlines()
                                       if endpoint in line and 'connection refused' in line.lower()
                                       and any(row.get('request_id') and row['request_id'] in line for row in transport_rows)]
                    if transport_rows and transport_lines:
                        break
                    time.sleep(0.05)
                refusal['gateway_error_rows'] = transport_rows
                refusal['gateway_transport_lines'] = transport_lines
                if not transport_rows or not transport_lines:
                    failures.append('gateway did not record the reserved endpoint connection refusal')
            if spec.get('followup') and not spec.get('restart'):
                with server.lock:
                    server.recovered = True
                responses.append(ask(root, port, token, spec, 2))
        if spec.get('restart'):
            with server.lock:
                server.recovered = True
            with gateway(binary, root, port, cache, cleanup):
                after_restart = h.rows(root)
                responses.append(ask(root, port, token, spec, 2))
        with server.lock:
            all_calls = list(server.calls)
            fixture_errors = list(server.fixture_errors)
        for call in all_calls:
            invalid = route_failure(call, spec, bad_models)
            if invalid:
                failures.append(invalid)
        wanted_status = spec.get('client_status', 200)
        if responses[0]['status'] != wanted_status:
            failures.append(f'client status {responses[0]["status"]} != {wanted_status}')
        observed = [c['channel'] for c in initial_calls]
        if observed != spec['channels']:
            failures.append(f'channel sequence {observed} != {spec["channels"]}')
        if len({(c['channel'], c['model']) for c in initial_calls}) != len(initial_calls):
            failures.append('a concrete route was dispatched twice')
        expected_cooldowns = spec.get('cooldown_count', spec['channels'].count(1))
        expected_keys = {('default', call['model'], 1) for call in initial_calls if call['channel'] == 1} if expected_cooldowns else set()
        if refusal:
            expected_keys = {('default', bad_models[0], 1)}
        snapshots = [('initial', cooldowns)]
        if spec.get('restart'):
            snapshots.append(('after_restart', after_restart))
        for stage, observed_rows in snapshots:
            if observed_rows is None:
                failures.append(stage + ' missing cooldown persistence snapshot')
                continue
            if len(observed_rows) != expected_cooldowns:
                failures.append(f'{stage} cooldown count {len(observed_rows)} != {expected_cooldowns}')
            keys = {(row['group'], row['model'], row['channel_id']) for row in observed_rows}
            if keys != expected_keys or any(row['level'] != 1 for row in observed_rows):
                failures.append(stage + ' cooldown pairs/levels differ from the observed failed routes')
        if spec.get('followup'):
            later = all_calls[len(initial_calls):]
            cooled_models = {r['model'] for r in cooldowns}
            if (len(later) != 1 or later[0]['channel'] != 1 or later[0]['model'] in cooled_models
                    or responses[1]['status'] != 200):
                failures.append('request-only channel exclusion leaked, or persisted pair cooldown was lost')
        if fixture_errors:
            errors.extend(fixture_errors)
    except Exception as exc:
        errors.append(type(exc).__name__ + ': ' + str(exc))
    finally:
        if refused_socket is not None:
            refused_socket.close()
            if refusal is not None:
                refusal['reserved_socket_closed'] = refused_socket.fileno() == -1
        server.shutdown()
        server.server_close()
        thread.join(timeout=3)
    if thread.is_alive() or any(not x['reaped'] or not x['log_closed'] for x in cleanup):
        errors.append('fixture cleanup incomplete')
    return {'case': spec['name'], 'cache': cache, 'responses': responses, 'calls': initial_calls,
            'followup_calls': all_calls[len(initial_calls):],
            'cooldowns': cooldowns, 'cooldowns_after_restart': after_restart, 'refusal': refusal,
            'cleanup': cleanup, 'failures': failures, 'fixture_errors': errors,
            'work': str(root)}


def run():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('binary', nargs='?')
    parser.add_argument('--cache', choices=('false', 'true'), default='false')
    parser.add_argument('--case', action='append', dest='selected')
    parser.add_argument('--list-cases', action='store_true')
    args = parser.parse_args()
    manifest = cases()
    if args.selected:
        missing = set(args.selected) - {c['name'] for c in manifest}
        if missing:
            parser.error('unknown cases: ' + ','.join(sorted(missing)))
        manifest = [c for c in manifest if c['name'] in args.selected]
    if args.list_cases:
        print(json.dumps({'cases': [c['name'] for c in manifest], 'count': len(manifest)}))
        return 0
    if not args.binary or not os.environ.get('TMPDIR'):
        parser.error('binary and TMPDIR are required')
    if {name for _, name in socket.if_nameindex()} != {'lo'}:
        parser.error('refusing wildcard bootstrap outside a loopback-only network namespace')
    source = pathlib.Path(args.binary).resolve(strict=True)
    work = pathlib.Path(tempfile.mkdtemp(prefix='auto-policy-', dir=os.environ['TMPDIR']))
    binary = work / 'gateway'
    shutil.copy2(source, binary)
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    output = {'binary_sha256': digest, 'expected_cases': len(manifest), 'results': []}
    for spec in manifest:
        row = run_case(binary, work / spec['name'], spec, args.cache)
        output['results'].append(row)
        (work / 'results.json').write_text(json.dumps(output, indent=2))
        print(json.dumps({k: row[k] for k in ('case', 'cache', 'failures', 'fixture_errors')}), flush=True)
    assert len(output['results']) == output['expected_cases']
    summary = {'results': str(work / 'results.json'), 'cases': len(output['results']),
               'behavior_failures': sum(bool(r['failures']) for r in output['results']),
               'fixture_errors': sum(bool(r['fixture_errors']) for r in output['results'])}
    print(json.dumps(summary), flush=True)
    return 2 if summary['fixture_errors'] else 1 if summary['behavior_failures'] else 0


def main():
    try:
        return run()
    except Exception as error:
        print(f'Infrastructure error: {type(error).__name__}: {error}', file=sys.stderr)
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
