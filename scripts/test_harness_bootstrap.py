"""Bootstrap/CLI ownership contracts; all execution requires a private netns.

Faults are injected at I/O/thread boundaries, never as pre-labelled case verdicts.
Real sockets are kept referenced until teardown assertions, so GC cannot hide leaks.
"""
import contextlib
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.request
from unittest import mock


def load_script(filename):
    spec = importlib.util.spec_from_file_location(filename.replace('-', '_'), Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class BootstrapContracts(unittest.TestCase):
    def setUp(self):
        root = self.enterContext(tempfile.TemporaryDirectory(prefix='bootstrap-contract-', dir=os.environ['TMPDIR']))
        self.root = Path(root)
        self.source = self.root / 'source'
        self.source.write_bytes(b'not executed: synthetic binary input')
        self.enterContext(mock.patch.dict(os.environ, TMPDIR=root))
        self.enterContext(mock.patch.object(urllib.request, '_opener', None))

    def invoke(self, module, args):
        with mock.patch.object(sys, 'argv', [module.__file__, *map(str, args)]), \
                contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()) as err:
            escaped = None
            try:
                code = module.main()
            except SystemExit as exc:
                code = exc.code
            except Exception as exc:
                code, escaped = None, repr(exc)
        return code, out.getvalue(), err.getvalue(), escaped

    def assert_error2(self, outcome, detail):
        code, out, err, escaped = outcome
        self.assertEqual(code, 2, 'bootstrap escaped ERROR/2 boundary: ' + str(escaped))
        self.assertIn(detail, out + err)
        self.assertNotIn('Traceback', out + err)
        self.assertIsNone(escaped)

    def assert_no_bootstrap_dirs(self):
        self.assertEqual([path.name for path in self.root.iterdir() if path.is_dir()], [],
                         'failed bootstrap left an acquired scratch directory behind')


class SSEBootstrapIOTests(BootstrapContracts):
    def setUp(self):
        super().setUp()
        self.module = load_script('test-auto-sse-errors.py')
        self.args = [self.source, '--cache', 'false', '--cases', 'normal']

    def test_missing_binary_is_error2_before_server_creation(self):
        with mock.patch.object(self.module, 'FixtureServer') as server:
            self.assert_error2(self.invoke(self.module, [self.root / 'missing']), 'FileNotFoundError')
            server.assert_not_called()
        self.assert_no_bootstrap_dirs()

    def test_unreadable_binary_is_error2_and_rolls_back_directory(self):
        original = Path.open
        def deny(path, *args, **kwargs):
            if path == self.source:
                raise PermissionError('injected unreadable binary')
            return original(path, *args, **kwargs)
        with mock.patch.object(Path, 'open', deny), mock.patch.object(self.module, 'FixtureServer') as server:
            self.assert_error2(self.invoke(self.module, self.args), 'injected unreadable binary')
            server.assert_not_called()
        self.assert_no_bootstrap_dirs()

    def test_malformed_baseline_is_error2_and_rolls_back_directory(self):
        prior = self.root / 'bad-baseline.json'
        prior.write_text('{not-json')
        with mock.patch.object(self.module, 'FixtureServer') as server:
            self.assert_error2(self.invoke(self.module, [*self.args, '--baseline-results', prior]), 'JSONDecodeError')
            server.assert_not_called()
        self.assert_no_bootstrap_dirs()

    def test_directory_creation_failure_is_error2(self):
        with mock.patch.object(self.module.tempfile, 'mkdtemp', side_effect=OSError('injected directory failure')), \
                mock.patch.object(self.module, 'FixtureServer') as server:
            self.assert_error2(self.invoke(self.module, self.args), 'injected directory failure')
            server.assert_not_called()
        self.assert_no_bootstrap_dirs()


SCRIPTS = {
    'sse': ('test-auto-sse-errors.py', 'FixtureServer', ['--cache', 'false', '--cases', 'normal']),
    'effort': ('test-auto-effort-routing.py', 'FixtureServer', ['--cache', 'false', '--cases', 'chat.max.qwen3']),
    'policy': ('test-auto-routing-policy.py', 'Server', ['--cache', 'false', '--case', 'generic503']),
    'score': ('test-auto-score-persistence.py', 'Upstream', ['--cache', 'false', '--cases', 'idle-no-probes']),
    'credit': ('test-auto-credit-errors.py', 'Upstream', ['--cache', 'false']),
}


class ThreadBootstrapTests(BootstrapContracts):
    def check_thread_fault(self, name, stage):
        filename, server_name, args = SCRIPTS[name]
        module = load_script(filename)
        servers, workers = [], []
        original_server = getattr(module, server_name)
        original_thread = threading.Thread
        def server(*a, **kw):
            value = original_server(*a, **kw)
            servers.append(value)
            return value
        def worker(*a, **kw):
            if stage == 'construct':
                raise RuntimeError('injected thread construction failure')
            value = original_thread(*a, **kw)
            workers.append(value)
            value.start = mock.Mock(side_effect=RuntimeError('injected thread start failure'))
            return value
        try:
            with mock.patch.object(module, server_name, side_effect=server), \
                    mock.patch.object(module.threading, 'Thread', side_effect=worker):
                outcome = self.invoke(module, [self.source, *args])
            self.assertEqual(len(servers), 1, 'thread fault did not follow real socket acquisition')
            self.assertEqual(servers[0].socket.fileno(), -1,
                             name + ' leaked its bound server socket on thread ' + stage + ' failure')
            self.assertTrue(all(not value.is_alive() for value in workers))
            self.assert_error2(outcome, 'injected thread')
            if name != 'policy':
                self.assert_no_bootstrap_dirs()
            else:
                self.assertEqual(list(self.root.glob('auto-policy-*/generic503')), [],
                                 'failed case bootstrap left its owned directory')
        finally:
            for server in servers:
                server.server_close()  # Test emergency cleanup, after inspecting the real ownership result.

    def test_sse_thread_construction_failure_closes_socket(self):
        self.check_thread_fault('sse', 'construct')

    def test_sse_thread_start_failure_closes_socket(self):
        self.check_thread_fault('sse', 'start')

    def test_effort_thread_construction_failure_closes_socket(self):
        self.check_thread_fault('effort', 'construct')

    def test_effort_thread_start_failure_closes_socket(self):
        self.check_thread_fault('effort', 'start')

    def test_policy_thread_construction_failure_closes_socket(self):
        self.check_thread_fault('policy', 'construct')

    def test_policy_thread_start_failure_closes_socket(self):
        self.check_thread_fault('policy', 'start')

    def test_score_thread_construction_failure_control_closes_socket(self):
        self.check_thread_fault('score', 'construct')

    def test_score_thread_start_failure_control_closes_socket(self):
        self.check_thread_fault('score', 'start')

    def test_credit_thread_construction_failure_control_closes_socket(self):
        self.check_thread_fault('credit', 'construct')

    def test_credit_thread_start_failure_control_closes_socket(self):
        self.check_thread_fault('credit', 'start')


class CopyBootstrapIOTests(BootstrapContracts):
    def test_effort_unreadable_copy_rolls_back_scratch(self):
        module = load_script(SCRIPTS['effort'][0])
        with mock.patch.object(module.shutil, 'copy2', side_effect=PermissionError('injected unreadable binary')):
            self.assert_error2(self.invoke(module, [self.source, *SCRIPTS['effort'][2]]), 'injected unreadable binary')
        self.assert_no_bootstrap_dirs()

    def test_policy_unreadable_copy_rolls_back_scratch(self):
        module = load_script(SCRIPTS['policy'][0])
        with mock.patch.object(module.shutil, 'copy2', side_effect=PermissionError('injected unreadable binary')):
            self.assert_error2(self.invoke(module, [self.source, *SCRIPTS['policy'][2]]), 'injected unreadable binary')
        self.assert_no_bootstrap_dirs()


class LateGuardAndInterruptTests(BootstrapContracts):
    def test_late_guard_refusal_releases_owned_servers_and_threads(self):
        for name in ('sse', 'effort', 'policy', 'score'):
            for stage in ('server', 'gateway'):
                with self.subTest(harness=name, stage=stage):
                    filename, server_name, args = SCRIPTS[name]
                    module = load_script(filename)
                    owner = getattr(module, 'sse', getattr(module, 'h', module))
                    guard = owner.fixture_namespace
                    safe = guard.inspect_namespace()
                    unsafe = copy.deepcopy(safe)
                    unsafe['routes4'].append({'dst': 'default', 'dev': 'lo', 'table': 123})
                    safe_count = (4 if name == 'score' else 2) + (2 if stage == 'gateway' else 0)
                    samples = [copy.deepcopy(safe) for _ in range(safe_count)] + [unsafe]
                    servers, workers = [], []
                    original_server, original_thread = getattr(module, server_name), threading.Thread
                    def server(*a, **kw):
                        value = original_server(*a, **kw)
                        servers.append(value)
                        return value
                    def worker(*a, **kw):
                        value = original_thread(*a, **kw)
                        workers.append(value)
                        return value
                    with mock.patch.object(guard, 'inspect_namespace', side_effect=samples), \
                            mock.patch.object(module, server_name, side_effect=server), \
                            mock.patch.object(module.threading, 'Thread', side_effect=worker):
                        outcome = self.invoke(module, [self.source, *args])
                    self.assert_error2(outcome, 'network namespace')
                    self.assertEqual(len(servers), 0 if stage == 'server' else 1)
                    self.assertTrue(all(value.socket.fileno() == -1 for value in servers))
                    self.assertTrue(all(not value.is_alive() for value in workers))
                    self.assertEqual(list(self.root.rglob('process-*.log')), [], 'gateway crossed late guard')

    def test_thread_construction_interrupt_propagates_after_socket_cleanup(self):
        for name in ('sse', 'effort', 'policy', 'score'):
            with self.subTest(harness=name):
                filename, server_name, args = SCRIPTS[name]
                module = load_script(filename)
                servers = []
                original = getattr(module, server_name)
                def server(*a, **kw):
                    value = original(*a, **kw)
                    servers.append(value)
                    return value
                try:
                    with mock.patch.object(module, server_name, side_effect=server), \
                            mock.patch.object(module.threading, 'Thread', side_effect=KeyboardInterrupt), \
                            self.assertRaises(KeyboardInterrupt):
                        self.invoke(module, [self.source, *args])
                    self.assertEqual(len(servers), 1)
                    self.assertEqual(servers[0].socket.fileno(), -1)
                finally:
                    for value in servers:
                        value.server_close()


def thread_fault_cli():
    """Child-only real entry point fault, with pre-exit socket ownership evidence."""
    import runpy
    import socketserver
    filename, binary, stage, *args = sys.argv[1:]
    original_server = socketserver.TCPServer.__init__
    servers = []
    def acquire(server, *a, **kw):
        original_server(server, *a, **kw)
        servers.append(server)
    target = 'threading.Thread' if stage == 'construct' else 'threading.Thread.start'
    sys.argv = [filename, binary, *args]
    try:
        with mock.patch.object(socketserver.TCPServer, '__init__', acquire), \
                mock.patch(target, side_effect=RuntimeError('injected CLI thread ' + stage + ' failure')):
            runpy.run_path(filename, run_name='__main__')
    finally:
        print('CLI_TEARDOWN ' + json.dumps({'server_count': len(servers),
                                          'socket_fds_before_exit': [value.socket.fileno() for value in servers]}))
        for value in servers:
            value.server_close()


class RealCLITests(BootstrapContracts):
    def command(self, name, binary=None, extra=(), env=None, fault=None):
        filename, _, args = SCRIPTS[name]
        script = Path(__file__).with_name(filename)
        if fault:
            command = [sys.executable, '-B', '-c',
                       'import test_harness_bootstrap as t; t.thread_fault_cli()',
                       str(script), str(binary or self.source), fault, *args]
        else:
            command = [sys.executable, '-B', str(script), str(binary or self.source), *args, *extra]
        result = subprocess.run(command, cwd=script.parent, capture_output=True, text=True,
                                timeout=15, env=env)
        # Raw CLI output stays in the unit run log, including real exit status.
        print('CLI_RESULT ' + json.dumps({'command': command, 'exit': result.returncode,
                                         'stdout': result.stdout, 'stderr': result.stderr}), flush=True)
        return result

    def assert_cli2(self, result, detail):
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn(detail, result.stdout + result.stderr)
        self.assertNotIn('Traceback', result.stdout + result.stderr)

    def test_real_missing_binary_and_directory_failure_exits_are_error2(self):
        for name in ('sse', 'effort', 'policy', 'score'):
            with self.subTest(harness=name, fault='missing-binary'):
                self.assert_cli2(self.command(name, binary=self.root / 'missing'), 'FileNotFoundError')
            with self.subTest(harness=name, fault='directory'):
                env = dict(os.environ, TMPDIR=str(self.root / 'missing-dir'))
                self.assert_cli2(self.command(name, env=env), 'directory' if name == 'score' else 'FileNotFoundError')
        self.assert_no_bootstrap_dirs()

    def test_real_unreadable_binary_and_malformed_baseline_exit_error2(self):
        for name in ('sse', 'effort', 'policy', 'score'):
            with self.subTest(harness=name):
                self.assert_cli2(self.command(name, binary=self.root), 'IsADirectoryError')
        baseline = self.root / 'invalid.json'
        baseline.write_text('{not-json')
        self.assert_cli2(self.command('sse', extra=('--baseline-results', str(baseline))), 'JSONDecodeError')
        self.assert_no_bootstrap_dirs()

    def test_real_thread_faults_exit2_and_close_sockets_before_process_exit(self):
        for name in ('sse', 'effort', 'policy', 'score'):
            for stage in ('construct', 'start'):
                with self.subTest(harness=name, fault=stage):
                    result = self.command(name, fault=stage)
                    self.assert_cli2(result, 'injected CLI thread')
                    teardown = [json.loads(line.removeprefix('CLI_TEARDOWN '))
                                for line in result.stdout.splitlines() if line.startswith('CLI_TEARDOWN ')]
                    self.assertEqual(teardown, [{'server_count': 1, 'socket_fds_before_exit': [-1]}])

    def test_real_argparse_help_and_invalid_options_keep_original_exits(self):
        for name in ('sse', 'effort', 'policy', 'score'):
            with self.subTest(harness=name):
                result = self.command(name, extra=('--help',))
                self.assertEqual(result.returncode, 0)
                self.assertIn('usage:', result.stdout)
                self.assert_cli2(self.command(name, extra=('--not-a-real-option',)), 'unrecognized arguments')


if __name__ == '__main__':
    unittest.main()
