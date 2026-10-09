"""Fixture contracts for the independent credit-error wire regression."""
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from contextlib import redirect_stdout
from unittest import mock

spec = importlib.util.spec_from_file_location('credit_wire', Path(__file__).with_name('test-auto-credit-errors.py'))
assert spec is not None and spec.loader is not None
credit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(credit)


def frames(body):
    return [json.loads(line[6:]) for line in body.decode().splitlines() if line.startswith('data: {')]


class CreditFixtureTests(unittest.TestCase):
    def test_literal_banner_stays_content_not_a_fabricated_error(self):
        payloads = frames(credit.fixture_body('stream-banner', credit.TARGET, True))
        self.assertFalse(any('error' in frame for frame in payloads))
        self.assertEqual(credit.BANNER, ''.join(choice['delta'].get('content', '')
                         for frame in payloads for choice in frame['choices']))
        self.assertTrue(all(frame['model'] == credit.RESPONSE_MODEL for frame in payloads))

    def test_split_banner_really_crosses_delta_boundaries(self):
        payloads = frames(credit.fixture_body('stream-split', credit.TARGET, True))
        fragments = [choice['delta']['content'] for frame in payloads for choice in frame['choices']
                     if 'content' in choice['delta']]
        self.assertGreater(len(fragments), 3)
        self.assertTrue(all(len(fragment) == 1 for fragment in fragments))
        self.assertEqual(credit.BANNER, ''.join(fragments))

    def test_named_nonstream_replays_the_same_literal(self):
        body = json.loads(credit.fixture_body('nonstream-banner-named', credit.TARGET, False))
        self.assertEqual(credit.BANNER, body['choices'][0]['message']['content'])
        self.assertNotIn('error', body)

    def test_choice_fragments_have_independent_indices(self):
        payloads = frames(credit.fixture_body('stream-independent-choices', credit.TARGET, True))
        choices = [choice for frame in payloads for choice in frame['choices'] if choice['delta'].get('content')]
        self.assertEqual({0, 1}, {choice['index'] for choice in choices})
        self.assertEqual(credit.BANNER, ''.join(choice['delta']['content'] for choice in choices))
        self.assertTrue(all(choice['delta']['content'] != credit.BANNER for choice in choices))

    def test_normal_target_and_backup_both_contain_real_answers(self):
        for model in (credit.TARGET, credit.HEALTHY):
            body = credit.fixture_body('normal', model, True).decode()
            self.assertEqual('fixture-ok', credit.answer_text({'body': body}, True))

    def test_unsafe_namespace_stops_before_bootstrap(self):
        with mock.patch.object(credit.socket, 'if_nameindex', return_value=[(1, 'lo'), (2, 'eth0')]), \
             mock.patch.object(credit.tempfile, 'mkdtemp') as create, \
             mock.patch.object(credit.fixture, 'start') as start, \
             mock.patch('sys.argv', ['test-auto-credit-errors.py', '/unused/binary']):
            with self.assertRaises(SystemExit) as stopped:
                credit.main()
            self.assertEqual(2, stopped.exception.code)
            create.assert_not_called()
            start.assert_not_called()


class CreditBootstrapTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='credit-contract-', dir=os.environ.get('TMPDIR'))
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.runs = self.root / 'runs'
        self.runs.mkdir()
        self.binary = self.root / 'not-a-gateway'
        self.binary.write_bytes(b'non-executable input for bootstrap contracts only\n')
        for patcher in (
            mock.patch.object(credit.socket, 'if_nameindex', return_value=[(1, 'lo')]),
            mock.patch.dict(os.environ, {'TMPDIR': str(self.runs)}),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        patcher = mock.patch.object(credit, 'run_case', side_effect=AssertionError('must not run a case during failed bootstrap'))
        self.run_case = patcher.start()
        self.addCleanup(patcher.stop)
        patcher = mock.patch.object(credit.fixture, 'start', side_effect=AssertionError('contract tests must never launch a gateway'))
        self.start = patcher.start()
        self.addCleanup(patcher.stop)
        self.addCleanup(self.start.assert_not_called)

    def main_code(self, binary=None, extra=()):
        output = io.StringIO()
        argv = ['test-auto-credit-errors.py', str(binary or self.binary),
                '--cache', 'false', '--cases', 'normal', *extra]
        with mock.patch('sys.argv', argv), redirect_stdout(output):
            try:
                code = credit.main()
            except Exception as exc:
                self.fail(f'bootstrap fixture failures must return exit 2, not escape as {exc!r} (CLI exit 1)')
        self.output = output.getvalue()
        return code

    def assert_bootstrap_error(self, kind):
        diagnostic = json.loads(self.output.strip())
        self.assertEqual('ERROR', diagnostic['status'])
        self.assertIn(kind, ' '.join(diagnostic['fixture_errors']))
        self.run_case.assert_not_called()
        self.assertEqual([], list(self.runs.iterdir()), 'failed bootstrap must clean up its private directory')

    def test_missing_binary_returns_fixture_error(self):
        self.assertEqual(2, self.main_code(binary=self.root / 'missing-binary'))
        self.assert_bootstrap_error('FileNotFoundError')

    def test_unreadable_binary_returns_fixture_error(self):
        with mock.patch.object(credit.Path, 'read_bytes', side_effect=PermissionError('unreadable binary')):
            self.assertEqual(2, self.main_code())
        self.assert_bootstrap_error('PermissionError')

    def test_malformed_baseline_returns_fixture_error(self):
        baseline = self.root / 'malformed-baseline.json'
        baseline.write_text('{')
        self.assertEqual(2, self.main_code(extra=('--baseline-results', str(baseline))))
        self.assert_bootstrap_error('JSONDecodeError')

    def test_bootstrap_directory_failure_returns_fixture_error(self):
        with mock.patch.object(credit.tempfile, 'mkdtemp', side_effect=PermissionError('bootstrap directory denied')):
            self.assertEqual(2, self.main_code())
        self.assert_bootstrap_error('PermissionError')

    def test_thread_start_failure_closes_bootstrap_server(self):
        servers = []
        upstream_type = credit.Upstream

        def create_upstream():
            server = upstream_type()
            servers.append(server)
            self.addCleanup(server.server_close)
            return server

        with mock.patch.object(credit, 'Upstream', side_effect=create_upstream), \
             mock.patch.object(credit.threading.Thread, 'start', side_effect=RuntimeError('bootstrap worker refused')):
            self.assertEqual(2, self.main_code())
        self.assertEqual(1, len(servers))
        self.assertEqual(-1, servers[0].socket.fileno(), 'failed bootstrap leaked the acquired listening socket')
        self.assert_bootstrap_error('RuntimeError')

    def test_per_case_exit_codes_and_cleanup_stay_unchanged(self):
        for status, failures, fixture_errors, expected in (
            ('PASS', [], [], 0),
            ('FAIL', ['behavioral mismatch'], [], 1),
            ('ERROR', [], ['fixture failed'], 2),
            ('ERROR', ['behavioral mismatch'], ['fixture failed'], 2),
        ):
            with self.subTest(status=status, failures=failures, fixture_errors=fixture_errors):
                row = {'id': 'false:normal', 'status': status,
                       'failures': failures, 'fixture_errors': fixture_errors}
                self.run_case.side_effect = None
                self.run_case.return_value = row
                self.run_case.reset_mock()
                self.assertEqual(expected, self.main_code())
                self.run_case.assert_called_once()
                summary = json.loads(self.output.splitlines()[-1])
                report = json.loads(Path(summary['results']).read_text())
                self.assertEqual([row], report['results'])
                self.assertTrue(report['stub_stopped'])


if __name__ == '__main__':
    unittest.main()
