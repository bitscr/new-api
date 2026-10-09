#!/usr/bin/env python3
"""Replay credit-error content through an isolated real gateway.

Never reads production data or credentials. Run inside a loopback-only network
namespace: unshare --net -- sh -c 'ip link set lo up && exec python3 -B
scripts/test-auto-credit-errors.py /absolute/binary'.
"""
import argparse
from contextlib import ExitStack
import hashlib
import http.server
import importlib.util
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import sqlite3
import tempfile
import threading
import time
from typing import cast
import urllib.error
import urllib.request

TARGET = 'moonshotai/kimi-k2.7-code'
RESPONSE_MODEL = 'moonshotai/Kimi-K2.7-Code'
HEALTHY = 'probe-ok'
CREDIT_ERROR = '[Upstream Error: 402 "You have no remaining credits. Purchase pre-paid credits to continue using Inference Providers. Alternatively, subscribe to PRO to get monthly included credits."]'
BANNER = '\n' + CREDIT_ERROR + '\n'
CASES = (
    'stream-banner', 'stream-split', 'nonstream-banner', 'stream-unterminated',
    'stream-quoted-prose', 'stream-prose-after', 'stream-tools', 'stream-reasoning',
    'stream-lookalike', 'stream-multiple-choices', 'stream-independent-choices',
    'stream-banner-named', 'nonstream-banner-named', 'normal', 'nonstream-normal',
    'stream-code-quote', 'stream-warning-split',
)
BAD_CASES = {'stream-banner', 'stream-split', 'nonstream-banner', 'stream-unterminated',
             'stream-banner-named', 'nonstream-banner-named', 'stream-warning-split'}

spec = importlib.util.spec_from_file_location('credit_fixture_base', Path(__file__).with_name('test-auto-sse-errors.py'))
assert spec is not None and spec.loader is not None
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


def content_for(case):
    if case in ('normal', 'nonstream-normal'):
        return 'fixture-ok'
    if case == 'stream-quoted-prose':
        return 'Example only: ' + BANNER + ' The real request succeeded.'
    if case == 'stream-prose-after':
        return BANNER + ' This is a real answer.'
    if case == 'stream-code-quote':
        return '```text\n' + CREDIT_ERROR + '\n```'
    if case == 'stream-lookalike':
        return '[Upstream ErrorRate: this is a metric name, not an error]'
    if case == 'stream-multiple-choices':
        return BANNER + 'fixture-ok'
    if case == 'stream-unterminated':
        return '\n' + CREDIT_ERROR[:-1]
    if case == 'stream-warning-split':
        return '[Gateway Warning: upstream is overloaded]'
    if case not in CASES:
        raise ValueError('undeclared fixture case')
    return BANNER


def fixture_body(case, model, stream):
    selected = case if model == TARGET else 'normal'
    text = content_for(selected)
    body = {'id': 'credit-fixture', 'object': 'chat.completion.chunk' if stream else 'chat.completion',
            'created': 1, 'model': RESPONSE_MODEL if model == TARGET else HEALTHY}
    if not stream:
        body['choices'] = [{'index': 0, 'message': {'role': 'assistant', 'content': text}, 'finish_reason': 'stop'}]
        body['usage'] = {'prompt_tokens': 8, 'completion_tokens': 2, 'total_tokens': 10}
        return json.dumps(body).encode()
    fragments = list(text) if selected in ('stream-split', 'stream-warning-split') else [text]
    if selected == 'stream-prose-after':
        fragments = [BANNER, ' This is a real answer.']
    choice_deltas = [[{'index': 0, 'delta': {'content': part}, 'finish_reason': None}] for part in fragments]
    if selected == 'stream-tools':
        choice_deltas[-1][0]['delta']['tool_calls'] = [{'index': 0, 'id': 'call_credit', 'type': 'function',
            'function': {'name': 'lookup', 'arguments': '{}'}}]
    if selected == 'stream-reasoning':
        choice_deltas[-1][0]['delta']['reasoning_content'] = 'Useful reasoning.'
    if selected == 'stream-multiple-choices':
        choice_deltas = [[{'index': 0, 'delta': {'content': BANNER}, 'finish_reason': None},
                          {'index': 1, 'delta': {'content': 'fixture-ok'}, 'finish_reason': None}]]
    if selected == 'stream-independent-choices':
        choice_deltas = [[{'index': 0, 'delta': {'content': BANNER[:3]}, 'finish_reason': None},
                          {'index': 1, 'delta': {'content': BANNER[3:]}, 'finish_reason': None}]]
    indices = sorted({choice['index'] for choices in choice_deltas for choice in choices})
    choice_deltas.append([{'index': index, 'delta': {}, 'finish_reason': 'stop'} for index in indices])
    return (''.join('data: ' + json.dumps(dict(body, choices=choices)) + '\n\n'
                    for choices in choice_deltas) + 'data: [DONE]\n\n').encode()


def answer_text(response, stream):
    if stream:
        payloads = [json.loads(line[6:]) for line in response['body'].splitlines() if line.startswith('data: {')]
    else:
        payloads = [json.loads(response['body'])]
    return ''.join(choice.get('delta' if stream else 'message', {}).get('content', '') or ''
                   for payload in payloads for choice in payload.get('choices', []))


class Upstream(http.server.ThreadingHTTPServer):
    def __init__(self):
        self.lock = threading.Lock()
        self.case = None
        self.calls = []
        self.errors = []
        super().__init__(('127.0.0.1', 0), Handler)


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        state = cast(Upstream, self.server)
        try:
            if self.headers.get('Transfer-Encoding'):
                chunks = []
                while True:
                    size = int(self.rfile.readline().split(b';', 1)[0], 16)
                    if size == 0:
                        while self.rfile.readline().strip():
                            pass
                        break
                    chunks.append(self.rfile.read(size))
                    if self.rfile.read(2) != b'\r\n':
                        raise ValueError('invalid chunk terminator')
                raw = b''.join(chunks)
            else:
                raw = self.rfile.read(int(self.headers['Content-Length']))
            request = json.loads(raw)
            channel = {'/channel-1/v1/chat/completions': 1, '/channel-2/v1/chat/completions': 2}[self.path]
            model = request['model']
            if model != {1: TARGET, 2: HEALTHY}[channel]:
                raise ValueError('request model did not match independently observed channel path')
            with state.lock:
                case = state.case
                state.calls.append({'channel': channel, 'model': model, 'path': self.path})
            body = fixture_body(case, model, request.get('stream', False))
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream' if request.get('stream') else 'application/json')
        except Exception as exc:
            with state.lock:
                state.errors.append(repr(exc))
            body = b'{"error":{"message":"fixture failure"}}'
            self.send_response(500)
            self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        self.wfile.flush()


def query(root, sql, values=()):
    with sqlite3.connect('file:' + str(root / 'one-api.db') + '?mode=ro', uri=True) as conn:
        conn.row_factory = sqlite3.Row
        return [dict(row) for row in conn.execute(sql, values)]


def cooldowns(root):
    return query(root, 'SELECT "group",model,channel_id,level,until,updated_at FROM auto_model_cooldowns ORDER BY id')


def ask(root, port, token, case, index):
    stream = not case.startswith('nonstream')
    model = TARGET if case.endswith('-named') else 'auto'
    payload = {'model': model, 'messages': [{'role': 'user', 'content': 'Synthetic credit-error fixture.'}],
               'max_tokens': 32, 'stream': stream}
    request = urllib.request.Request(f'http://127.0.0.1:{port}/v1/chat/completions', data=json.dumps(payload).encode(),
                                    headers={'Authorization': 'Bearer sk-' + token, 'Content-Type': 'application/json'})
    try:
        response = urllib.request.urlopen(request, timeout=15)
    except urllib.error.HTTPError as exc:
        response = exc
    with response:
        body = response.read()
        result = {'status': response.status, 'body': body.decode(),
                  'request_id': response.headers.get('X-Oneapi-Request-Id', '')}
    (root / f'response-{index}.txt').write_bytes(body)
    time.sleep(0.15)  # Deferred feedback and async usage logging follow the final frame.
    return result


def run_case(binary, base, upstream, cache, case, baseline):
    root = base / (cache + '-' + case)
    root.mkdir()
    port = fixture.free_port()
    cleanup = []
    process = log = None

    def stop():
        nonlocal process, log
        if process is not None:
            assert log is not None
            fixture.stop(process, log)
            cleanup.append({'pid': process.pid, 'reaped': process.poll() is not None, 'log_closed': log.closed})
            process = log = None

    row = {'id': cache + ':' + case, 'case': case, 'cache': cache, 'work': str(root),
           'failures': [], 'fixture_errors': [], 'cleanup': cleanup}
    try:
        process, log, version = fixture.start(binary, root, port, cache)
        row['assets'] = fixture.smoke_assets(port)
        row['version'] = version
        stop()
        token, admin = secrets.token_hex(24), secrets.token_hex(16)
        fixture.seed(root, token, f'http://127.0.0.1:{upstream.server_port}', 'normal', admin)
        with sqlite3.connect(root / 'one-api.db') as conn:
            conn.execute('UPDATE channels SET models=? WHERE id=1', (TARGET,))
            conn.execute('UPDATE abilities SET model=? WHERE channel_id=1', (TARGET,))
            for channel in (1, 2):
                conn.execute('UPDATE channels SET base_url=? WHERE id=?',
                             (f'http://127.0.0.1:{upstream.server_port}/channel-{channel}', channel))
            conn.execute('UPDATE options SET value=? WHERE key=?', (json.dumps({TARGET: 1, HEALTHY: 1}), 'ModelRatio'))
        with upstream.lock:
            upstream.case = case
            begin, errors_begin = len(upstream.calls), len(upstream.errors)
        process, log, _ = fixture.start(binary, root, port, cache)
        responses = [ask(root, port, token, case, 1)]
        first_cooldowns = cooldowns(root)
        responses.append(ask(root, port, token, case, 2))
        second_cooldowns = cooldowns(root)
        stop()
        process, log, _ = fixture.start(binary, root, port, cache)
        responses.append(ask(root, port, token, case, 3))
        final_cooldowns = cooldowns(root)
        errors = fixture.usage_error_rows(root)
        views = fixture.usage_log_views(port, admin, token, responses[0]['request_id'])
        usage = query(root, 'SELECT model_name,channel_id,quota,prompt_tokens,completion_tokens FROM logs WHERE type=2 AND request_id=?', (responses[0]['request_id'],))
        with upstream.lock:
            calls = list(upstream.calls[begin:])
            row['fixture_errors'].extend(upstream.errors[errors_begin:])
        bad, named = case in BAD_CASES, case.endswith('-named')
        expected_calls = [(1, TARGET)] + ([(2, HEALTHY)] * 2 if bad and not named else [(1, TARGET)] * 2)
        if [(call['channel'], call['model']) for call in calls] != expected_calls:
            row['failures'].append('cooling target was dispatched again or a healthy target was skipped')
        expected_keys = [('default', TARGET, 1, 1)] if bad and not named else []
        for stage in (first_cooldowns, second_cooldowns, final_cooldowns):
            if [(x['group'], x['model'], x['channel_id'], x['level']) for x in stage] != expected_keys:
                row['failures'].append('missing or incorrectly scoped persistent cooldown')
            if any(x['until'] - x['updated_at'] != 900 for x in stage):
                row['failures'].append('first-trip cooldown did not use the existing 15-minute window')
        if first_cooldowns != second_cooldowns or first_cooldowns != final_cooldowns:
            row['failures'].append('other-model success or restart changed the failed pair cooldown')
        if [x['status'] for x in responses] != [200, 200, 200]:
            row['failures'].append('client HTTP status changed')
        if not all(x['request_id'] for x in responses):
            row['failures'].append('missing request IDs')
        for index, response in enumerate(responses):
            expected_text = 'fixture-ok' if index > 0 and bad and not named else content_for(case)
            try:
                observed_text = answer_text(response, not case.startswith('nonstream'))
            except (ValueError, TypeError, KeyError, AttributeError):
                row['failures'].append('client response is not a valid fixture completion')
            else:
                if observed_text != expected_text:
                    row['failures'].append('client response no longer contains exactly the expected answer')
        wanted_errors = 3 if bad and named else int(bad)
        if len(errors) != wanted_errors:
            row['failures'].append('incorrect persisted diagnostic error count')
        expected_error_ids = {x['request_id'] for x in responses} if bad and named else ({responses[0]['request_id']} if bad else set())
        if {x['request_id'] for x in errors} != expected_error_ids:
            row['failures'].append('diagnostic errors are not correlated to failed requests')
        for error in errors:
            if (error['model_name'] != TARGET or error['channel'] != 1 or error['quota'] != 0
                    or json.loads(error['other']).get('status_code') != 200):
                row['failures'].append('diagnostic attribution, quota or original status changed')
        if views['root']['total'] != int(bad) or len(views['root']['items']) != int(bad):
            row['failures'].append('request-filtered root error view disagrees with persistence')
        if views['missing']['total'] or views['missing']['items']:
            row['failures'].append('request filter included unrelated diagnostics')
        token_errors = [x for x in views['token'] if x['type'] == 5]
        if len(token_errors) != wanted_errors:
            row['failures'].append('token error view has the wrong diagnostic count')
        for error in token_errors:
            if set(json.loads(error['other'])) != {'error_code', 'error_type', 'status_code'}:
                row['failures'].append('token diagnostic leaked privileged detail')
        if baseline:
            previous = baseline['responses'][0]
            def comparable(value):
                return value['status'], value['body'].replace(value['request_id'], '<request-id>')
            if comparable(responses[0]) != comparable(previous) or usage != baseline['first_usage']:
                row['failures'].append('first response bytes/status or first-request billing changed')
        row.update({'calls': calls, 'responses': responses, 'first_cooldowns': first_cooldowns,
                    'second_cooldowns': second_cooldowns, 'cooldowns': final_cooldowns,
                    'errors': errors, 'log_views': views, 'first_usage': usage})
    except Exception as exc:
        row['fixture_errors'].append(repr(exc))
    finally:
        stop()
    if any(not x['reaped'] or not x['log_closed'] for x in cleanup):
        row['fixture_errors'].append('incomplete fixture cleanup')
    row['status'] = 'ERROR' if row['fixture_errors'] else 'FAIL' if row['failures'] else 'PASS'
    return row


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('binary', type=Path)
    parser.add_argument('--cache', choices=('false', 'true', 'both'), default='both')
    parser.add_argument('--cases', nargs='+', choices=CASES, default=list(CASES))
    parser.add_argument('--baseline-results', type=Path)
    args = parser.parse_args()
    if socket.if_nameindex() != [(1, 'lo')]:
        parser.error('verified loopback-only network namespace required before bootstrap')
    try:
        with ExitStack() as bootstrap:
            base = Path(tempfile.mkdtemp(prefix='auto-credit-errors-', dir=os.environ['TMPDIR']))
            bootstrap.callback(shutil.rmtree, base)
            binary = base / 'new-api'
            source_hash = hashlib.sha256(args.binary.read_bytes()).hexdigest()
            shutil.copy2(args.binary, binary)
            assert hashlib.sha256(binary.read_bytes()).hexdigest() == source_hash
            modes = ('false', 'true') if args.cache == 'both' else (args.cache,)
            baseline = {x['id']: x for x in json.loads(args.baseline_results.read_text())['results']} if args.baseline_results else {}
            ids = [cache + ':' + case for cache in modes for case in args.cases]
            if args.baseline_results and set(ids) - set(baseline):
                parser.error('baseline must contain every selected case')
            results = []
            report = {'source_binary': str(args.binary), 'executed_binary': str(binary), 'sha256': source_hash,
                      'expected_cases': len(ids), 'expected_ids': ids, 'interfaces': socket.if_nameindex(),
                      'results': results, 'stub_stopped': False}
            upstream = Upstream()
            bootstrap.callback(upstream.server_close)
            worker = threading.Thread(target=upstream.serve_forever, daemon=True)
            worker.start()
            bootstrap.callback(worker.join, timeout=5)
            bootstrap.callback(upstream.shutdown)
            # The existing matrix cleanup owns successfully bootstrapped resources.
            bootstrap.pop_all()
    except Exception as exc:
        print(json.dumps({'status': 'ERROR', 'fixture_errors': ['bootstrap: ' + repr(exc)]}), flush=True)
        return 2
    try:
        for cache in modes:
            for case in args.cases:
                row = run_case(binary, base, upstream, cache, case, baseline.get(cache + ':' + case))
                results.append(row)
                (base / 'results.json').write_text(json.dumps(report, indent=2))
                print(json.dumps({k: row[k] for k in ('id', 'status', 'failures', 'fixture_errors')}), flush=True)
    finally:
        upstream.shutdown()
        upstream.server_close()
        worker.join(timeout=5)
        report['stub_stopped'] = not worker.is_alive()
        (base / 'results.json').write_text(json.dumps(report, indent=2))
    assert len(results) == len({x['id'] for x in results}) == len(ids)
    print(json.dumps({'results': str(base / 'results.json'), 'cases': len(results),
                      'failed': sum(x['status'] == 'FAIL' for x in results),
                      'fixture_errors': sum(x['status'] == 'ERROR' for x in results)}))
    return 2 if not report['stub_stopped'] or any(x['fixture_errors'] for x in results) else int(any(x['failures'] for x in results))


if __name__ == '__main__':
    raise SystemExit(main())
