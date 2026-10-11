"""Unit tests for the passive score-persistence runtime evidence oracle."""
import base64
from contextlib import closing, redirect_stderr, redirect_stdout
import http.client
import importlib.util
import io
import json
import os
import pathlib
import sqlite3
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location(
    'score_harness', pathlib.Path(__file__).with_name('test-auto-score-persistence.py'))
assert SPEC is not None and SPEC.loader is not None
h = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(h)


class IdleOracleTests(unittest.TestCase):
    def test_unsolicited_upstream_request_is_a_failure(self):
        prior = [{'channel': 1, 'model': 'score-a', 'path': '/channel-1/v1/chat/completions'}]
        later = prior + [{'channel': 1, 'model': 'score-a', 'path': '/channel-1/v1/chat/completions'}]
        self.assertEqual(h.check_idle(prior, later), ['unexpected upstream request during idle'])

    def test_no_request_is_a_valid_control(self):
        self.assertEqual(h.check_idle([], []), [])


class CallOracleTests(unittest.TestCase):
    def test_exact_method_path_channel_model_and_count_are_required(self):
        check = getattr(h, 'check_calls', lambda *_: [])
        wanted = [('POST', '/channel-1/v1/chat/completions', 1, 'score-a')]
        good = [{'method': 'POST', 'path': wanted[0][1], 'channel': 1,
                 'model': 'score-a', 'at_ns': 123}]
        self.assertEqual(check(good, wanted), [])
        for bad in ([], good + good,
                    [dict(good[0], method='GET')],
                    [dict(good[0], path='/channel-1/v1/models')],
                    [dict(good[0], channel=2)],
                    [dict(good[0], model='score-b')]):
            with self.subTest(observed=bad):
                self.assertTrue(check(bad, wanted), 'call identity/count mismatch was accepted')


class ScoreSchemaTests(unittest.TestCase):
    def test_score_reader_explicitly_closes_sqlite_connections(self):
        acquired = []
        original = sqlite3.connect
        class Tracked(sqlite3.Connection):
            closed = False
            def close(self):
                self.closed = True
                return super().close()
        def connect(*args, **kwargs):
            conn = original(*args, **kwargs, factory=Tracked)
            acquired.append(conn)
            return conn
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            original(root / 'one-api.db').close()
            try:
                with mock.patch.object(h.sqlite3, 'connect', side_effect=connect):
                    h.read_scores(root)
                self.assertTrue(acquired[0].closed, 'sqlite context manager did not close its connection')
            finally:
                for conn in acquired:
                    conn.close()

    def test_missing_score_table_is_named_behavior_failure_not_sql_error(self):
        self.assertTrue(callable(getattr(h, 'read_scores', None)), 'score evidence reader is missing')
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            with closing(sqlite3.connect(root / 'one-api.db')) as conn:
                conn.execute('CREATE TABLE unrelated (id INTEGER)')
                conn.commit()
            snapshot = h.read_scores(root)
            self.assertFalse(snapshot['present'])
            self.assertEqual(h.check_scores(snapshot, []), ['missing auto_model_scores table'])


def score_row(scope='model', model='score-a', channel=1, **changes):
    row = dict(scope=scope, group='default', model=model, channel_id=channel,
               score=0.65, latency_ms=20.0, observations=1.0,
               observed_at=1700000000000000000, version=1)
    row.update(changes)
    return row


def score_snapshot(rows):
    return dict(present=True, columns=list(h.SCORE_COLUMNS), rows=rows,
                read_at_ns=1700000000000000001)


class ScoreEvidenceTests(unittest.TestCase):
    def test_score_keys_cannot_be_missing_duplicated_or_cross_scoped(self):
        pair = score_row()
        channel = score_row('channel', '')
        wanted = [('model', 'default', 'score-a', 1), ('channel', 'default', '', 1)]
        self.assertEqual(h.check_scores(score_snapshot([pair, channel]), wanted), [])
        for rows in ([pair], [pair, channel, pair], [pair, dict(channel, model='score-a')],
                     [pair, dict(channel, group='other')], [pair, dict(channel, channel_id=2)]):
            with self.subTest(rows=rows):
                self.assertTrue(h.check_scores(score_snapshot(rows), wanted),
                                'wrong scope/key/count was accepted')

    def test_score_evidence_rejects_invalid_numeric_and_timestamp_fields(self):
        wanted = [('model', 'default', 'score-a', 1)]
        for changes in ({'score': float('nan')}, {'latency_ms': float('inf')},
                        {'observations': 0}, {'observations': 21}, {'score': 1.1},
                        {'latency_ms': -1}, {'observed_at': 1.7e18},
                        {'observed_at': 0}, {'version': 2}):
            with self.subTest(changes=changes):
                self.assertTrue(h.check_scores(score_snapshot([score_row(**changes)]), wanted),
                                'invalid score evidence was accepted')

    def test_incomplete_schema_is_a_behavior_failure(self):
        snapshot = score_snapshot([])
        snapshot['columns'].remove('observed_at')
        self.assertTrue(h.check_scores(snapshot, []), 'incomplete schema was accepted')


class UpdateOracleTests(unittest.TestCase):
    def test_expired_history_cannot_pass_as_a_fresh_first_observation(self):
        check = getattr(h, 'check_first_score', lambda *_: [])
        self.assertEqual(check(score_row()), [])
        for bad in (score_row(observations=19, score=0.99), score_row(latency_ms=60000),
                    score_row(observations=2), score_row(score=0.8), score_row(latency_ms=0)):
            with self.subTest(row=bad):
                self.assertTrue(check(bad), 'revived evidence passed the cold-start oracle')

    def test_bad_sut_timestamp_is_behavior_failure_not_fixture_type_error(self):
        with tempfile.TemporaryDirectory() as directory:
            ctx = h.Case(h.SimpleNamespace(base=pathlib.Path(directory)), 'false', 'unit', {'scores': []})
            ctx.root.mkdir()
            ctx.gateway = h.SimpleNamespace(ensure_live=lambda: None)
            bad = score_row(observed_at='not-an-int64')
            caught = None
            with mock.patch.object(h, 'read_scores', return_value=score_snapshot([bad])):
                try:
                    ctx.wait_scores('bad-row', [h.score_key(bad)], [h.score_key(bad)],
                                    {'started_ns': 1700000000000000000, 'settled_ns': 1700000001000000000})
                except Exception as exc:
                    caught = exc
            self.assertIsInstance(caught, h.BehaviorFailure,
                                  'malformed persisted timestamp was misclassified as infrastructure')

    def test_restored_update_preserves_decayed_count_score_and_latency(self):
        check = getattr(h, 'check_update', lambda *_args, **_kwargs: [])
        before = score_row(score=0.8, observations=8.0, latency_ms=200.0)
        # 期望值必须跟着声明的衰减窗口算,不能写死旧窗口下的数字。
        age_ms = (1700000006000000000 - before['observed_at']) // 1000000
        factor = max(0, 1 - age_ms / h.DECAY_WINDOW_MS)
        decayed_score = 0.5 + (before['score'] - 0.5) * factor
        decayed_n = min(20, before['observations']) * factor
        after = score_row(score=decayed_score + 0.30 * (1 - decayed_score),
                          observations=min(20, decayed_n + 1),
                          latency_ms=before['latency_ms'] * factor * 0.8 + 100 * 0.2,
                          observed_at=1700000006000000000)
        self.assertEqual(check(before, after, sample_latency_ms=100), [])
        for bad in (dict(after, observations=1.0, score=0.65),
                    dict(after, score=0.8), dict(after, latency_ms=100.0),
                    dict(after, observed_at=before['observed_at']),
                    dict(after, channel_id=2)):
            with self.subTest(after=bad):
                self.assertTrue(check(before, bad, sample_latency_ms=100),
                                'reset or wrong update was accepted as restored evidence')


class RuntimeLatencyBridgeTests(unittest.TestCase):
    def test_case_updates_rejects_latency_reset_using_independent_request_measurements(self):
        before = score_row(score=0.8, observations=8.0, latency_ms=200.0)
        observed_at = 1700000006000000000
        age_ms = (observed_at - before['observed_at']) // 1000000
        factor = max(0, 1 - age_ms / h.DECAY_WINDOW_MS)
        decayed_score = 0.5 + (before['score'] - 0.5) * factor
        decayed_n = min(20, before['observations']) * factor
        expected = dict(score=decayed_score + 0.30 * (1 - decayed_score),
                        observations=min(20, decayed_n + 1),
                        latency_ms=before['latency_ms'] * factor * 0.8 + 100 * 0.2)
        reset = score_row(observed_at=observed_at, **dict(expected, latency_ms=100.0))
        response = {'sample_latency_ms': [99, 101],
                    'started_ns': 1700000005950000000, 'settled_ns': 1700000006050000000}
        with tempfile.TemporaryDirectory() as directory:
            ctx = h.Case(h.SimpleNamespace(base=pathlib.Path(directory)), 'false', 'latency',
                         {'responses': [response]})
            ctx.root.mkdir()
            with self.assertRaisesRegex(h.BehaviorFailure, 'restored latency_ms update differs'):
                ctx.updates(score_snapshot([before]), score_snapshot([reset]), [h.score_key(before)])
            good = dict(reset, latency_ms=expected['latency_ms'])
            ctx.updates(score_snapshot([before]), score_snapshot([good]), [h.score_key(before)])


class LatencyMeasurementTests(unittest.TestCase):
    def test_real_request_bounds_are_measured_outside_the_score_row(self):
        with tempfile.TemporaryDirectory() as directory:
            server, worker = h.start_upstream(pathlib.Path(directory))
            server.delay = 0.075
            try:
                response = h.http_request(server.server_port, 'POST', '/channel-1/v1/chat/completions',
                                          b'{"model":"score-a","stream":false}')
                response['settled_mono_ns'] = h.time.monotonic_ns()
                calls = server.snapshot()
                self.assertEqual(len(calls), 1)
                self.assertIn('reply_started_mono_ns', calls[0],
                              'fake upstream did not measure its real pre-response delay')
                self.assertIn('started_mono_ns', response, 'client request lacked monotonic measurement')
                bounds = h.measured_latency_bounds(response, calls[0])
                self.assertGreaterEqual(bounds[0], 70)
                self.assertLessEqual(bounds[0], bounds[1])
                self.assertLess(bounds[1], 5000)
                self.assertEqual(bounds[0], max(1, (calls[0]['reply_started_mono_ns']
                                                   - calls[0]['delay_started_mono_ns']) // 1000000))
                predicted = h.latency_update_bounds(score_row(latency_ms=1000), bounds,
                                                      [1700000000000000000, 1700000001000000000])
                self.assertEqual(predicted, [1000 * (1 - 1000 / h.DECAY_WINDOW_MS) * 0.8 + bounds[0] * 0.2,
                                             1000 * 0.8 + bounds[1] * 0.2])
            finally:
                self.assertEqual(h.stop_upstream(server, worker), [])


class ResponseOracleTests(unittest.TestCase):
    def test_success_status_and_raw_bytes_are_not_normalized(self):
        check = getattr(h, 'check_response', lambda *_: [])
        raw = b'{ "choices": [{"message":{"content":"fixture-ok"}}] }\n'
        response = {'status': 200, 'body_b64': base64.b64encode(raw).decode()}
        self.assertEqual(check(response, raw), [])
        self.assertTrue(check(dict(response, status=201), raw), 'wrong HTTP status was accepted')
        self.assertTrue(check(dict(response, body_b64=base64.b64encode(raw.strip()).decode()), raw),
                        'whitespace-changing response was accepted')


class ManifestOracleTests(unittest.TestCase):
    def test_missing_duplicate_and_unrequested_results_cannot_pass(self):
        check = getattr(h, 'check_manifest', lambda *_: [])
        wanted = ['false:idle-no-probes', 'true:idle-no-probes']
        good = [{'id': value} for value in wanted]
        self.assertEqual(check(wanted, good), [])
        for bad in (good[:1], good + good[:1], good + [{'id': 'false:unknown'}],
                    [good[0], good[0]]):
            with self.subTest(results=bad):
                self.assertTrue(check(wanted, bad), 'incomplete/duplicate manifest was accepted')


class NamespaceCLITests(unittest.TestCase):
    def test_namespace_refusal_precedes_any_bootstrap_io(self):
        self.assertTrue(callable(getattr(h, 'main', None)), 'namespace-first CLI is missing')
        with mock.patch('socket.if_nameindex', return_value=[(1, 'lo'), (2, 'eth0')]), \
                mock.patch('pathlib.Path.read_bytes', side_effect=AssertionError('binary read')) as read, \
                mock.patch('tempfile.mkdtemp', side_effect=AssertionError('directory created')) as mkdir, \
                redirect_stderr(io.StringIO()) as err, self.assertRaises(SystemExit) as caught:
            h.main(['/not-a-binary', '--cache', 'false'])
        self.assertEqual(caught.exception.code, 2)
        self.assertIn('loopback-only', err.getvalue())
        read.assert_not_called()
        mkdir.assert_not_called()

    def test_listing_is_side_effect_free_and_default_matrix_is_literal(self):
        self.assertTrue(callable(getattr(h, 'main', None)), 'case listing is missing')
        with mock.patch('tempfile.mkdtemp', side_effect=AssertionError('directory created')), \
                redirect_stdout(io.StringIO()) as out:
            self.assertEqual(h.main(['--list-cases']), 0)
        self.assertEqual(json.loads(out.getvalue())['cases'], [
            'idle-no-probes', 'feedback-save-reload', 'startup-two-level-ranking',
            'expired-scores-neutral', 'priority-over-score', 'strict-pair-cooldown',
            'cooldown-expiry-passive'])


class BootstrapIOTests(unittest.TestCase):
    def test_missing_binary_is_error2_and_rolls_back_scratch(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            with mock.patch.dict(os.environ, TMPDIR=str(root)), redirect_stdout(io.StringIO()) as out:
                result = h.main([str(root / 'absent'), '--cache', 'false'])
            self.assertEqual(result, 2, 'missing binary masqueraded as a behavioral result')
            self.assertEqual(json.loads(out.getvalue())['status'], 'ERROR')
            self.assertEqual(list(root.iterdir()), [])

    def test_binary_read_copy_and_directory_failures_are_error2(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            source = root / 'source'
            source.write_bytes(b'fixture executable bytes')
            for target in ('pathlib.Path.read_bytes', 'shutil.copy2', 'tempfile.mkdtemp'):
                with self.subTest(stage=target), mock.patch.dict(os.environ, TMPDIR=str(root)), \
                        mock.patch(target, side_effect=OSError('injected fixture I/O failure')) as fault, \
                        redirect_stdout(io.StringIO()) as out:
                    result = h.main([str(source), '--cache', 'false'])
                    self.assertEqual(result, 2, 'bootstrap I/O masqueraded as behavioral RED')
                    self.assertEqual(json.loads(out.getvalue())['status'], 'ERROR')
                    fault.assert_called()
                self.assertEqual(list(root.iterdir()), [source])


class MatrixContractTests(unittest.TestCase):
    def test_nonfinite_score_evidence_is_recorded_without_masking_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'rows.jsonl'
            value = {'score': float('inf'), 'nested': [float('nan')]}
            error = None
            try:
                h.append_jsonl(path, value)
            except Exception as exc:
                error = type(exc).__name__
            self.assertIsNone(error, 'invalid score evidence became an evidence-writer fixture error')
            saved = json.loads(path.read_text())
            self.assertEqual(saved, {'score': {'nonfinite_float': 'inf'},
                                     'nested': [{'nonfinite_float': 'nan'}]})
            self.assertEqual(value['score'], float('inf'), 'evidence encoder mutated the oracle input')

    def test_noop_case_cannot_claim_complete_behavioral_coverage(self):
        with tempfile.TemporaryDirectory() as directory:
            upstream = h.SimpleNamespace(lock=h.threading.Lock(), calls=[], errors=[], snapshot=lambda: [])
            state = h.SimpleNamespace(base=pathlib.Path(directory), upstream=upstream)
            with mock.patch.dict(h.CASE_HANDLERS, {'feedback-save-reload': lambda *_: None}):
                row = h.run_case(state, 'false', 'feedback-save-reload')
            self.assertEqual(row['status'], 'FAIL', 'a no-op case passed with zero of its declared requests')
            self.assertEqual(row['fixture_errors'], [])

    def test_malformed_persisted_time_is_fail_not_fixture_error(self):
        with tempfile.TemporaryDirectory() as directory:
            row = {'scores': []}
            state = h.SimpleNamespace(base=pathlib.Path(directory))
            ctx = h.Case(state, 'false', 'fixture', row)
            ctx.root.mkdir()
            ctx.gateway = h.SimpleNamespace(ensure_live=lambda: None)
            malformed = score_snapshot([score_row(observed_at=None)])
            with mock.patch.object(h, 'read_scores', return_value=malformed):
                try:
                    ctx.wait_scores('invalid-timestamp', [h.model_key('score-a', 1)],
                                    [h.model_key('score-a', 1)], {'started_ns': 1, 'settled_ns': 2})
                except h.BehaviorFailure:
                    outcome = 'FAIL'
                except Exception as exc:
                    outcome = 'ERROR: ' + type(exc).__name__
                else:
                    outcome = 'PASS'
            self.assertEqual(outcome, 'FAIL', 'invalid persisted evidence was not a behavioral assertion')

    def test_every_literal_case_has_a_distinct_runtime_handler(self):
        self.assertEqual(set(getattr(h, 'CASE_HANDLERS', {})), set(h.CASES),
                         'literal matrix has missing runtime cases')
        self.assertEqual(len(set(h.CASE_HANDLERS.values())), len(h.CASES))

    def test_pass0_fail1_error2_and_per_case_journal_before_next_case(self):
        for failures, errors, wanted in (([], [], 0), (['behavior'], [], 1), ([], ['fixture'], 2)):
            with self.subTest(exit=wanted), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                source = root / 'source'
                source.write_bytes(b'fixture executable bytes')
                states, ids = [], []
                def case(state, cache, name, negative_control=False):
                    journal = state.base / 'cases.jsonl'
                    prior = [json.loads(line) for line in journal.read_text().splitlines()] if journal.exists() else []
                    self.assertEqual([row['id'] for row in prior], ids, 'previous case was not journaled')
                    states.append(state)
                    ids.append(cache + ':' + name)
                    return {'id': ids[-1], 'failures': failures, 'fixture_errors': errors, 'calls': [], 'cleanup': []}
                with mock.patch.dict(os.environ, TMPDIR=str(root)), \
                        mock.patch.object(h, 'run_case', side_effect=case, create=True), \
                        redirect_stdout(io.StringIO()) as out:
                    actual = h.main([str(source), '--cases', 'idle-no-probes'])
                self.assertEqual(actual, wanted, 'PASS/FAIL/ERROR exit contract was not honored')
                self.assertEqual(ids, ['false:idle-no-probes', 'true:idle-no-probes'])
                report = json.loads((states[0].base / 'results.json').read_text())
                self.assertEqual(report['expected_ids'], ids)
                self.assertEqual(report['expected_cases'], len(ids))
                self.assertTrue(report['stub_stopped'])
                self.assertEqual(len(report['results']), len(ids))
                self.assertFalse(states[0].worker.is_alive())

    def test_uncaught_fixture_assertion_is_error_not_behavioral_red(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            source = root / 'source'
            source.write_bytes(b'fixture executable bytes')
            with mock.patch.dict(os.environ, TMPDIR=str(root)), \
                    mock.patch.object(h, 'run_case', side_effect=AssertionError('fixture setup'), create=True), \
                    redirect_stdout(io.StringIO()):
                self.assertEqual(h.main([str(source), '--cache', 'false', '--cases', 'idle-no-probes']), 2)


class GatewayCleanupTests(unittest.TestCase):
    def test_spawn_failure_closes_log(self):
        self.assertTrue(hasattr(h, 'Gateway'), 'gateway process owner is missing')
        with tempfile.TemporaryDirectory() as directory:
            row = {'cleanup': [], 'fixture_errors': []}
            gateway = h.Gateway(h.load_fixture(), pathlib.Path('/absent'), pathlib.Path(directory), 1, 'false', row)
            log = io.BytesIO()
            with mock.patch('pathlib.Path.open', return_value=log), \
                    mock.patch.object(h, 'subprocess', h.SimpleNamespace(
                        Popen=mock.Mock(side_effect=OSError('injected spawn failure')),
                        DEVNULL=h.subprocess.DEVNULL, STDOUT=h.subprocess.STDOUT)):
                with self.assertRaisesRegex(OSError, 'injected spawn failure'):
                    gateway.start()
            self.assertTrue(log.closed, 'process log leaked when spawn failed')
            gateway.stop()
            self.assertTrue(all(item['log_closed'] and item['reaped'] for item in row['cleanup']))

    def test_startup_network_error_reaps_process_and_closes_log(self):
        self.assertTrue(hasattr(h, 'Gateway'), 'gateway process owner is missing')
        with tempfile.TemporaryDirectory() as directory:
            row = {'cleanup': [], 'fixture_errors': []}
            gateway = h.Gateway(h.load_fixture(), pathlib.Path('/not-executed'), pathlib.Path(directory), 1, 'false', row)
            original = h.subprocess.Popen
            acquired = []
            def spawn(_command, **kwargs):
                process = original([h.sys.executable, '-B', '-c', 'import time; time.sleep(60)'], **kwargs)
                acquired.append(process)
                return process
            with mock.patch.object(h, 'subprocess', h.SimpleNamespace(
                    Popen=spawn, DEVNULL=h.subprocess.DEVNULL, STDOUT=h.subprocess.STDOUT)), \
                    mock.patch.object(h, 'wait_ready', side_effect=OSError('injected readiness network failure')):
                with self.assertRaisesRegex(OSError, 'injected readiness network failure'):
                    gateway.start()
            self.assertIsNotNone(acquired[0].poll(), 'gateway process was not reaped')
            self.assertTrue(row['cleanup'][0]['log_closed'])
            gateway.stop()
            self.assertEqual(len(row['cleanup']), 1, 'cleanup was not idempotent')


class BootstrapResourceTests(unittest.TestCase):
    def test_thread_start_bootstrap_failure_is_error2_with_rollback(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            source = root / 'source'
            source.write_bytes(b'fixture executable bytes')
            with mock.patch.dict(os.environ, TMPDIR=str(root)), \
                    mock.patch('threading.Thread.start', side_effect=RuntimeError('injected thread failure')) as fault, \
                    redirect_stdout(io.StringIO()) as out:
                self.assertEqual(h.main([str(source)]), 2, 'thread-start failure was not ERROR/2')
                self.assertEqual(json.loads(out.getvalue())['status'], 'ERROR')
                fault.assert_called_once()
            self.assertEqual(list(root.iterdir()), [source])

    def test_successful_bootstrap_hands_off_owned_resources(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            source = root / 'source'
            source.write_bytes(b'fixture executable bytes')
            with mock.patch.dict(os.environ, TMPDIR=str(root)):
                state = h.bootstrap(source)
            self.assertTrue(hasattr(state, 'upstream'), 'bootstrap did not acquire the request observer')
            try:
                self.assertTrue(state.worker.is_alive())
                self.assertEqual(state.binary.read_bytes(), source.read_bytes())
                self.assertEqual(state.fixture.__file__, str(pathlib.Path(__file__).with_name('test-auto-sse-errors.py')))
            finally:
                self.assertEqual(h.stop_upstream(state.upstream, state.worker), [])
            self.assertTrue(state.base.exists(), 'successful evidence directory was removed')


class ParserBoundaryTests(unittest.TestCase):
    def test_parser431_negative_control_reaches_rejection_and_fails_case_idle(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            server, worker = h.start_upstream(root)
            row = {'windows': []}
            ctx = h.Case(h.SimpleNamespace(base=root, upstream=server), 'false', 'idle-control', row)
            ctx.root.mkdir()
            ctx.gateway = h.SimpleNamespace(ensure_live=lambda: None)
            try:
                with self.assertRaisesRegex(h.BehaviorFailure, 'unexpected upstream request during idle'):
                    ctx.idle('deliberate-parser431', seconds=0.1, inject='parser431')
                self.assertEqual(row['negative_control_response']['status'], 431,
                                 'parser431 negative control did not reach HTTP header rejection')
                self.assertEqual(len(server.snapshot()), 1)
                self.assertEqual(row['windows'][0]['after_count'] - row['windows'][0]['before_count'], 1)
                self.assertEqual(server.errors, [])
            finally:
                self.assertEqual(h.stop_upstream(server, worker), [])

    def test_parser_rejections_are_counted_once_and_fail_the_idle_oracle(self):
        rejected = {
            'oversized-header-431': (b'GET /unsolicited-header HTTP/1.1\r\nHost: loopback\r\nX-Large: '
                                     + b'x' * 65537 + b'\r\n\r\n', 'GET', '/unsolicited-header', 431),
            'too-many-headers-431': (b'HEAD /unsolicited-many HTTP/1.1\r\n'
                                     + b'X-Small: x\r\n' * 101 + b'\r\n', 'HEAD', '/unsolicited-many', 431),
            'malformed-version-400': (b'FIXTURE /unsolicited-version HTTP/not-a-version\r\n\r\n',
                                      'FIXTURE', '/unsolicited-version', 400),
            'malformed-request-400': (b'PATCH /unsolicited-syntax extra HTTP/1.1\r\n\r\n',
                                      'PATCH', '/unsolicited-syntax', 400),
            'oversized-request-414': (b'GET /' + b'x' * 65537 + b' HTTP/1.1\r\n\r\n',
                                      'GET', None, 414),
        }
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            server, worker = h.start_upstream(root)
            try:
                for name, (wire, method, path, status) in rejected.items():
                    with self.subTest(request=name):
                        before = server.snapshot()
                        with h.socket.create_connection(('127.0.0.1', server.server_port), timeout=3) as conn:
                            conn.sendall(wire)
                            conn.shutdown(h.socket.SHUT_WR)
                            raw = bytearray()
                            while True:
                                try:
                                    chunk = conn.recv(65536)
                                except ConnectionResetError:
                                    break
                                if not chunk:
                                    break
                                raw.extend(chunk)
                        # Handler completion precedes server_close; wait for the recorded verdict.
                        deadline = h.time.monotonic() + 1
                        after = server.snapshot()
                        while len(after) <= len(before) and h.time.monotonic() < deadline:
                            h.time.sleep(0.01)
                            after = server.snapshot()
                        self.assertIn(str(status).encode(), raw, 'request did not reach the intended parser rejection')
                        self.assertEqual(h.check_idle(before, after), ['unexpected upstream request during idle'],
                                         'parser-rejected unsolicited request escaped the actual idle oracle')
                        self.assertEqual(len(after) - len(before), 1, 'one rejected request must count exactly once')
                        self.assertEqual(after[-1]['method'], method)
                        if path is not None:
                            self.assertEqual(after[-1]['path'], path)
                        self.assertEqual(after[-1]['status'], status)
                        self.assertTrue(after[-1]['parser_rejected'])
                        observed_line = base64.b64decode(after[-1]['request_line_b64'])
                        self.assertEqual(observed_line, wire.split(b'\n', 1)[0] + b'\n'
                                         if status != 414 else wire[:65537])
                self.assertEqual(server.errors, [], 'HTTP parser rejections are traffic, not fixture errors')
                self.assertTrue((root / 'upstream-requests.jsonl').is_file(),
                                'parser requests left no durable journal')
                received = [json.loads(line) for line in (root / 'upstream-requests.jsonl').read_text().splitlines()
                            if json.loads(line)['event'] == 'received']
                self.assertEqual(len(received), len(rejected), 'request journal double-counted parser rejections')
            finally:
                self.assertEqual(h.stop_upstream(server, worker), [])

    def test_nonempty_unparseable_line_is_evidence_even_without_an_http_response(self):
        with tempfile.TemporaryDirectory() as directory:
            server, worker = h.start_upstream(pathlib.Path(directory))
            try:
                before = server.snapshot()
                with h.socket.create_connection(('127.0.0.1', server.server_port), timeout=3) as conn:
                    conn.sendall(b'\r\n')
                    conn.shutdown(h.socket.SHUT_WR)
                    self.assertEqual(conn.recv(1024), b'')
                after = server.snapshot()
                self.assertEqual(h.check_idle(before, after), ['unexpected upstream request during idle'],
                                 'nonempty parser input was silently discarded')
                self.assertEqual(len(after), 1)
                self.assertEqual(base64.b64decode(after[0]['request_line_b64']), b'\r\n')
            finally:
                self.assertEqual(h.stop_upstream(server, worker), [])


class UpstreamLifecycleTests(unittest.TestCase):
    def test_unsolicited_get_and_every_http_method_are_observable(self):
        self.assertTrue(callable(getattr(h, 'start_upstream', None)), 'complete request observer is missing')
        with tempfile.TemporaryDirectory() as directory:
            server, worker = h.start_upstream(pathlib.Path(directory))
            try:
                before = server.snapshot()
                methods = ('GET', 'HEAD', 'PUT', 'DELETE', 'OPTIONS', 'PATCH', 'TRACE', 'CONNECT', 'FIXTURE')
                start = h.time.time_ns()
                for method in methods:
                    conn = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=3)
                    try:
                        conn.request(method, '/channel-1/unsolicited')
                        response = conn.getresponse()
                        response.read()
                    finally:
                        conn.close()
                end = h.time.time_ns()
                after = server.snapshot()
                self.assertEqual(len(after), len(methods))
                self.assertEqual([row['method'] for row in after], list(methods))
                self.assertTrue(all(start <= row['at_ns'] <= end for row in after))
                self.assertTrue(all(row['path'] == '/channel-1/unsolicited' for row in after))
                self.assertEqual(h.check_idle(before, after), ['unexpected upstream request during idle'])
                self.assertEqual(before, [], 'log snapshots mutated after they were captured')
                self.assertEqual(server.errors, [])
            finally:
                self.assertEqual(h.stop_upstream(server, worker), [])
            self.assertFalse(worker.is_alive())
            self.assertEqual(server.socket.fileno(), -1)

    def test_explicit_synthetic_post_preserves_reply_and_raw_request(self):
        self.assertTrue(callable(getattr(h, 'start_upstream', None)), 'literal fixture server is missing')
        with tempfile.TemporaryDirectory() as directory:
            server, worker = h.start_upstream(pathlib.Path(directory))
            try:
                raw = b'{"model":"score-a","stream":false}'
                conn = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=3)
                try:
                    conn.request('POST', '/channel-1/v1/chat/completions', raw,
                                 {'Content-Type': 'application/json'})
                    response = conn.getresponse()
                    body = response.read()
                    self.assertEqual(response.status, 200)
                    self.assertEqual(body, h.REPLIES['score-a'])
                    self.assertEqual(json.loads(body)['choices'][0]['message']['content'], 'fixture-ok:score-a')
                finally:
                    conn.close()
                calls = server.snapshot()
                self.assertEqual(h.check_calls(calls, [('POST', '/channel-1/v1/chat/completions', 1, 'score-a')]), [])
                self.assertEqual(base64.b64decode(calls[0]['body_b64']), raw)
                self.assertEqual(server.errors, [])
            finally:
                self.assertEqual(h.stop_upstream(server, worker), [])

    def test_thread_start_failure_closes_the_already_bound_server(self):
        self.assertTrue(callable(getattr(h, 'start_upstream', None)), 'staged upstream acquisition is missing')
        acquired = []
        original = h.Upstream
        def make(root):
            server = original(root)
            acquired.append(server)
            return server
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(h, 'Upstream', side_effect=make), \
                mock.patch('threading.Thread.start', side_effect=RuntimeError('injected thread failure')):
            with self.assertRaisesRegex(RuntimeError, 'injected thread failure'):
                h.start_upstream(pathlib.Path(directory))
        self.assertEqual(len(acquired), 1)
        self.assertEqual(acquired[0].socket.fileno(), -1, 'bound socket leaked on thread-start failure')


if __name__ == '__main__':
    unittest.main()
