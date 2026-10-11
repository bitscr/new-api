"""Strict isolated-network policy regression for the fake-upstream harnesses."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import socket
import unittest
from unittest import mock


def load_script(name):
    spec = importlib.util.spec_from_file_location(name.replace('-', '_'), Path(__file__).with_name(name))
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class RealNamespaceRegressionTests(unittest.TestCase):
    def test_real_fresh_namespace_accepts_inert_kernel_defaults(self):
        self.assertNotEqual(os.readlink('/proc/self/ns/net'), os.readlink('/proc/1/ns/net'),
                            'run this suite inside unshare --net with lo up')
        harness = load_script('test-auto-score-persistence.py')
        error = None
        try:
            isolation = harness.require_namespace()
        except RuntimeError as exc:
            error = str(exc)
        self.assertIsNone(error, 'inert kernel fallback devices wrongly refused: ' + str(error))
        self.assertEqual([list(item) for item in isolation['interfaces']],
                         [list(item) for item in socket.if_nameindex()])


def real_fixture():
    # Exact ip -j/-d output from a fresh namespace on the regression kernel.
    return json.loads((Path(__file__).parent / 'fixtures/netns-inert-defaults.json').read_text())


def loopback_fixture():
    state = real_fixture()
    state['interfaces'] = [[1, 'lo']]
    state['links'] = state['links'][:1]
    state['addresses'] = state['addresses'][:1]
    return state


def validate(state):
    return load_script('fixture_namespace.py').validate_namespace(state)


class NamespacePolicyTests(unittest.TestCase):
    def test_exact_real_inert_defaults_are_accepted_without_filtering(self):
        state = real_fixture()
        original = copy.deepcopy(state)
        error = None
        try:
            validate(state)
        except RuntimeError as exc:
            error = str(exc)
        self.assertIsNone(error, 'verified inert-default fixture was rejected: ' + str(error))
        self.assertEqual(state, original, 'validation filtered or mutated network evidence')

    def test_only_active_addressed_loopback_is_the_positive_control(self):
        validate(loopback_fixture())

    def test_host_namespace_and_unknown_devices_are_rejected(self):
        host = loopback_fixture()
        host['netns'] = host['pid1_netns']
        unknown = real_fixture()
        unknown['interfaces'].append([4, 'eth0'])
        unknown['links'].append(dict(unknown['links'][0], ifindex=4, ifname='eth0', link_type='ether'))
        unknown['addresses'].append(dict(unknown['addresses'][0], ifindex=4, ifname='eth0'))
        for state in (host, unknown):
            with self.subTest(interfaces=state['interfaces']), self.assertRaises(RuntimeError):
                validate(state)

    def test_default_names_cannot_hide_up_configured_or_other_kind_devices(self):
        for index in (1, 2):
            for changes in ({'flags': ['NOARP', 'UP']}, {'operstate': 'UP'}, {'link': 'eth0'},
                            {'link_type': 'ether'}, {'mtu': 1280}, {'master': 'vrf0'},
                            {'promiscuity': 1}, {'address': '2001:db8::1'}):
                with self.subTest(index=index, changes=changes):
                    state = real_fixture()
                    for collection in ('links', 'addresses'):
                        state[collection][index].update(changes)
                    with self.assertRaises(RuntimeError):
                        validate(state)
            for field, value in (('remote', '2001:db8::2'), ('local', '2001:db8::1'),
                                 ('ttl', 64), ('encap_limit', 4), ('ikey', 42),
                                 ('flowlabel', '0x00001'), ('tclass', '0xff')):
                with self.subTest(index=index, field=field):
                    state = real_fixture()
                    for collection in ('links', 'addresses'):
                        state[collection][index]['linkinfo']['info_data'][field] = value
                    with self.assertRaises(RuntimeError):
                        validate(state)
            state = real_fixture()
            for collection in ('links', 'addresses'):
                state[collection][index]['linkinfo']['info_kind'] = 'dummy'
            with self.subTest(index=index, kind='dummy'), self.assertRaises(RuntimeError):
                validate(state)

    def test_external_addresses_are_rejected_on_any_device(self):
        for index in (0, 1, 2):
            for family, address, prefix in (('inet', '192.0.2.1', 24), ('inet6', '2001:db8::1', 64)):
                state = loopback_fixture() if index == 0 else real_fixture()
                state['addresses'][index]['addr_info'].append(
                    {'family': family, 'local': address, 'prefixlen': prefix, 'scope': 'global'})
                with self.subTest(index=index, family=family), self.assertRaises(RuntimeError):
                    validate(state)
        state = real_fixture()
        state['addresses'][1]['addr_info'] = copy.deepcopy(state['addresses'][0]['addr_info'])
        with self.subTest(tunnel_has_loopback_address=True), self.assertRaises(RuntimeError):
            validate(state)

    def test_both_families_all_tables_reject_non_loopback_or_indirect_routes(self):
        for family, destination in (('routes4', '192.0.2.0/24'), ('routes6', '2001:db8::/64')):
            for changes in ({'dst': 'default', 'dev': 'lo'},
                            {'dst': destination, 'dev': 'lo'},
                            {'dst': destination, 'dev': 'eth0', 'table': 123},
                            {'dst': destination, 'dev': 'ip6tnl0'},
                            {'gateway': '192.0.2.1'}, {'via': {'family': 'inet6', 'addr': '2001:db8::1'}},
                            {'nexthops': [{'dev': 'eth0'}]}, {'nhid': 7},
                            {'prefsrc': '192.0.2.1'}, {'encap': {'type': 'seg6'}}):
                state = loopback_fixture()
                state[family].append(dict(state[family][0], **changes))
                with self.subTest(family=family, changes=changes), self.assertRaises(RuntimeError):
                    validate(state)


    def test_malformed_or_missing_state_fails_closed(self):
        states = [None, [], {}]
        for field in ('netns', 'pid1_netns', 'interfaces', 'links', 'addresses', 'routes4', 'routes6'):
            missing = real_fixture()
            del missing[field]
            states.append(missing)
            for invalid in (None, {}, 'invalid', 0, True):
                state = real_fixture()
                state[field] = invalid
                states.append(state)
        for collection in ('links', 'addresses'):
            for field in ('ifindex', 'ifname', 'flags', 'mtu', 'link_type', 'linkinfo', 'operstate', 'link'):
                state = real_fixture()
                del state[collection][1][field]
                states.append(state)
            state = real_fixture()
            state[collection].append(copy.deepcopy(state[collection][0]))
            states.append(state)
        state = real_fixture()
        state['links'][1]['linkinfo']['info_data']['ttl'] = False
        state['addresses'][1]['linkinfo']['info_data']['ttl'] = False
        states.append(state)
        for index, state in enumerate(states):
            with self.subTest(index=index), self.assertRaises(RuntimeError):
                validate(state)


    def test_numeric_route_addresses_and_fractional_interface_ids_are_malformed(self):
        for field in ('dst', 'prefsrc'):
            state = loopback_fixture()
            state['routes4'][0][field] = 2130706433  # ipaddress accepts this int as 127.0.0.1.
            with self.subTest(field=field), self.assertRaises(RuntimeError):
                validate(state)
        state = real_fixture()
        for collection in ('links', 'addresses'):
            state[collection][1]['ifindex'] = 2.0
        with self.subTest(fractional_ifindex=True), self.assertRaises(RuntimeError):
            validate(state)


class InspectionTests(unittest.TestCase):
    def setUp(self):
        self.guard = load_script('fixture_namespace.py')
        self.state = real_fixture()
        self.names = self.enterContext(mock.patch.object(self.guard.socket, 'if_nameindex',
                                      return_value=[tuple(x) for x in self.state['interfaces']]))
        self.links = self.enterContext(mock.patch.object(self.guard.os, 'readlink', side_effect=lambda path:
            self.state['pid1_netns'] if path == '/proc/1/ns/net' else self.state['netns']))
        self.enterContext(mock.patch.object(self.guard.shutil, 'which', return_value='/usr/sbin/ip'))
        self.run = self.enterContext(mock.patch.object(self.guard.subprocess, 'run', side_effect=self.result))

    def result(self, command, **kwargs):
        raw = next(row['stdout'] for row in self.state['inspection'] if row['command'][1:] == command[1:])
        if not kwargs.get('text'):
            raw = raw.encode()
        return self.guard.subprocess.CompletedProcess(command, 0, raw, '' if kwargs.get('text') else b'')

    def test_inspection_preserves_every_device_and_both_all_table_route_queries(self):
        result = self.guard.require_namespace()
        for key in ('links', 'addresses', 'routes4', 'routes6'):
            self.assertEqual(result[key], self.state[key])
        self.assertEqual([row['command'][1:] for row in result['inspection']],
                         [row['command'][1:] for row in self.state['inspection']])
        self.assertEqual([row['stdout'] for row in result['inspection']],
                         [row['stdout'] for row in self.state['inspection']])

    def assert_duplicate_route_inspection_rejected(self, family):
        original = self.result
        expected = copy.deepcopy(self.state[family])
        expected.append(copy.deepcopy(expected[0]))
        def duplicated(command, **kwargs):
            row = original(command, **kwargs)
            if ('-4' if family == 'routes4' else '-6') in command:
                row.stdout = json.dumps(expected).encode()
            return row
        self.run.side_effect = duplicated
        with self.assertRaisesRegex(self.guard.NamespaceError, 'duplicate.*route',
                                    msg='duplicated valid all-table route rows were accepted') as caught:
            self.guard.require_namespace()
        evidence = caught.exception.evidence
        self.assertEqual(evidence[family], expected, 'rejection lost or filtered the duplicate route')
        for key in ('interfaces', 'links', 'addresses'):
            self.assertEqual(evidence[key], self.state[key], 'rejection lost complete namespace evidence')
        entry = next(item for item in evidence['inspection']
                     if ('-4' if family == 'routes4' else '-6') in item['command'])
        self.assertEqual(json.loads(entry['stdout']), expected, 'raw inspector route output was not retained')

    def test_raw_inspector_rejects_duplicated_ipv4_route_array(self):
        self.assert_duplicate_route_inspection_rejected('routes4')

    def test_raw_inspector_rejects_duplicated_ipv6_route_array(self):
        self.assert_duplicate_route_inspection_rejected('routes6')

    def test_second_snapshot_rejects_late_route_without_an_interface_change(self):
        late = copy.deepcopy(self.state)
        late['routes6'].append({'dst': 'default', 'dev': 'ip6gre0', 'table': 123})
        with mock.patch.object(self.guard, 'inspect_namespace', side_effect=[self.state, late]), \
                self.assertRaises(RuntimeError):
            self.guard.require_namespace()

    def test_two_valid_snapshots_cannot_switch_network_namespaces(self):
        switched = dict(self.state, netns='net:[999999]')
        with mock.patch.object(self.guard, 'inspect_namespace', side_effect=[self.state, switched]), \
                self.assertRaises(RuntimeError):
            self.guard.require_namespace()

    def test_thread_namespace_cannot_differ_from_process_namespace(self):
        self.links.side_effect = lambda path: ('net:[999999]' if path == '/proc/thread-self/ns/net' else
            self.state['pid1_netns'] if path == '/proc/1/ns/net' else self.state['netns'])
        with self.assertRaises(RuntimeError):
            self.guard.require_namespace()

    def test_inspection_detects_namespace_change_before_returning(self):
        count = 0
        def changing(path):
            nonlocal count
            if path == '/proc/self/ns/net':
                count += 1
                return self.state['netns'] if count == 1 else 'net:[999999]'
            return self.state['pid1_netns'] if path == '/proc/1/ns/net' else self.state['netns']
        self.links.side_effect = changing
        with self.assertRaises(RuntimeError):
            self.guard.inspect_namespace()

    def test_inspection_detects_interface_change_before_returning(self):
        self.names.side_effect = [self.names.return_value, [(1, 'lo')]]
        with self.assertRaises(RuntimeError):
            self.guard.inspect_namespace()

    def test_each_failed_or_malformed_inspection_fails_closed(self):
        original = self.result
        for position in range(4):
            for fault in ('exit', 'stderr', 'invalid-json', 'duplicate-key', 'missing-list', 'nonfinite'):
                def faulty(command, **kwargs):
                    row = original(command, **kwargs)
                    slot = [x['command'][1:] for x in self.state['inspection']].index(command[1:])
                    if slot == position:
                        text = row.stdout if isinstance(row.stdout, str) else row.stdout.decode()
                        if fault == 'exit':
                            row.returncode = 1
                        elif fault == 'stderr':
                            row.stderr = 'ip failure' if kwargs.get('text') else b'ip failure'
                        elif fault == 'duplicate-key':
                            # Duplicate equal values must not be silently accepted by json.loads.
                            first = next(iter(json.loads(text)[0]))
                            literal = json.dumps(first) + ':' + json.dumps(json.loads(text)[0][first])
                            text = text.replace(literal, literal + ',' + literal, 1)
                        elif fault == 'nonfinite':
                            text = text.replace('}]', ',"bad":NaN}]', 1)
                        else:
                            text = '{' if fault == 'invalid-json' else '{}'
                        row.stdout = text if kwargs.get('text') else text.encode()
                    return row
                self.run.side_effect = faulty
                with self.subTest(position=position, fault=fault), self.assertRaises(RuntimeError):
                    self.guard.require_namespace()

    def test_missing_ip_failed_enumeration_and_unreadable_namespace_fail_closed(self):
        with mock.patch.object(self.guard.shutil, 'which', return_value=None), self.assertRaises(RuntimeError):
            self.guard.require_namespace()
        for target, field in ((self.guard.os, 'readlink'), (self.guard.socket, 'if_nameindex')):
            with mock.patch.object(target, field, side_effect=OSError('inspection unavailable')), \
                    self.assertRaises(RuntimeError):
                self.guard.require_namespace()
        for exc in (OSError('ip unavailable'), self.guard.subprocess.TimeoutExpired('ip', 5)):
            self.run.side_effect = exc
            with self.subTest(error=type(exc).__name__), self.assertRaises(RuntimeError):
                self.guard.require_namespace()

    def test_failed_query_retains_raw_stdout_stderr_and_command(self):
        import base64
        self.run.side_effect = self.guard.subprocess.TimeoutExpired('ip', 5, output=b'partial\xff', stderr=b'failure\xfe')
        with self.assertRaises(RuntimeError) as caught:
            self.guard.require_namespace()
        evidence = caught.exception.evidence
        self.assertTrue(evidence['inspection'], 'the failed command was lost from evidence')
        self.assertEqual(base64.b64decode(evidence['inspection'][-1]['stdout_b64']), b'partial\xff')
        self.assertEqual(base64.b64decode(evidence['inspection'][-1]['stderr_b64']), b'failure\xfe')
        self.assertEqual(evidence['inspection'][-1]['command'], ['/usr/sbin/ip', '-j', '-d', 'link', 'show'])


class GuardIntegrationTests(unittest.TestCase):
    @staticmethod
    def scripts():
        return [load_script('test-auto-' + name + '.py') for name in
                ('score-persistence', 'sse-errors', 'effort-routing', 'routing-policy', 'credit-errors')]

    def test_every_cli_rejects_before_binary_or_directory_bootstrap(self):
        import contextlib
        import io
        import sys
        import tempfile
        for module in self.scripts():
            with self.subTest(script=module.__file__), \
                    mock.patch.object(socket, 'if_nameindex', return_value=[(1, 'lo'), (2, 'eth0')]), \
                    mock.patch.object(Path, 'resolve', side_effect=AssertionError('binary resolved')) as resolve, \
                    mock.patch.object(Path, 'read_bytes', side_effect=AssertionError('binary read')) as read, \
                    mock.patch.object(tempfile, 'mkdtemp', side_effect=AssertionError('directory created')) as mkdir, \
                    mock.patch.object(sys, 'argv', [module.__file__, '/not-a-binary']), \
                    contextlib.redirect_stderr(io.StringIO()) as err, \
                    contextlib.redirect_stdout(io.StringIO()) as out:
                caught = None
                try:
                    code = module.main()
                except SystemExit as exc:
                    code = exc.code
                except Exception as exc:
                    code, caught = None, repr(exc)
                self.assertEqual(code, 2, 'namespace guard did not own CLI refusal: ' + str(caught))
                self.assertIn('network namespace', out.getvalue() + err.getvalue())
                resolve.assert_not_called()
                read.assert_not_called()
                mkdir.assert_not_called()

    def test_every_server_refuses_external_interfaces_before_creating_a_socket(self):
        score, sse, effort, policy, credit = self.scripts()
        constructors = [lambda: score.Upstream(Path('/unused')), lambda: sse.FixtureServer(('127.0.0.1', 0)),
                        effort.FixtureServer, lambda: policy.Server(policy.cases()[0], ['probe-bad-00']), credit.Upstream]
        for make in constructors:
            with self.subTest(constructor=make), \
                    mock.patch.object(socket, 'if_nameindex', return_value=[(1, 'lo'), (2, 'eth0')]), \
                    mock.patch.object(socket, 'socket', side_effect=AssertionError('socket created')) as bind:
                caught = None
                try:
                    make()
                except Exception as exc:
                    caught = exc
                self.assertIsInstance(caught, RuntimeError, 'namespace refusal must precede socket construction')
                self.assertIn('network namespace', str(caught))
                bind.assert_not_called()

    def test_sse_namespace_recheck_failures_stay_error2(self):
        import contextlib
        import io
        import sys
        import tempfile
        for stage in ('stub', 'gateway'):
            module = load_script('test-auto-sse-errors.py')
            safe = real_fixture()
            unsafe = real_fixture()
            unsafe['routes4'].append({'dst': 'default', 'dev': 'ip6tnl0', 'table': 123})
            samples = [copy.deepcopy(safe) for _ in range(2 if stage == 'stub' else 4)] + [unsafe]
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as directory, \
                    mock.patch.dict(os.environ, TMPDIR=directory), \
                    mock.patch.object(sys, 'argv', [module.__file__, __file__, '--cache', 'false', '--cases', 'normal']), \
                    mock.patch.object(module.fixture_namespace, 'inspect_namespace', side_effect=samples), \
                    contextlib.redirect_stderr(io.StringIO()) as err, \
                    contextlib.redirect_stdout(io.StringIO()):
                caught = None
                try:
                    code = module.main()
                except SystemExit as exc:
                    code = exc.code
                except Exception as exc:
                    code, caught = None, type(exc).__name__
                self.assertEqual(code, 2, 'late namespace failure escaped as CLI exit1: ' + str(caught))
                self.assertIn('network namespace', err.getvalue())

    def test_direct_and_spec_imports_find_only_their_sibling_owner(self):
        import subprocess
        import sys
        code = ('import importlib.util,pathlib; p=pathlib.Path(__import__("sys").argv[1]); '
                's=importlib.util.spec_from_file_location("outside_fixture",p); '
                'm=importlib.util.module_from_spec(s); s.loader.exec_module(m); '
                'h=getattr(m,"sse",getattr(m,"h",getattr(m,"fixture",m))); '
                'n=getattr(h,"fixture_namespace",None); '
                'assert n is not None, "common sibling guard missing"; '
                'assert pathlib.Path(n.__file__)==p.with_name("fixture_namespace.py"); '
                'print(n.__file__)')
        for module in self.scripts():
            for command in ([sys.executable, '-I', '-B', module.__file__, '--help'],
                            [sys.executable, '-I', '-B', '-c', code, module.__file__]):
                with self.subTest(command=command):
                    result = subprocess.run(command, capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == '__main__':
    unittest.main()
