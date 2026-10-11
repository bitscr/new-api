"""Fail-closed, read-only network guard for the synthetic upstream harnesses.

Only lo may be active/addressed. The two kernel IPv6 fallback devices are allowed
only in their observed inert default configuration, never by name alone. No
interface, route, sysctl or module is changed here. Call before bootstrap and each
server/process acquisition; do not cache this predicate or add a bypass option.
Two equal snapshots and thread/namespace bracketing detect changes during reads;
they are not a lock against a privileged peer reconfiguring the namespace later.
The runner must exclusively own its fresh namespace for the entire fixture run.
"""
import base64
import ipaddress
import json
import os
import re
import shutil
import socket
import subprocess


class NamespaceError(RuntimeError):
    def __init__(self, reason, evidence):
        self.evidence = evidence
        super().__init__('isolated network namespace required: verified loopback-only '
                         'unshare --net required before bootstrap; ' + reason
                         + '; network_state=' + json.dumps(evidence, sort_keys=True, default=repr))


IP_QUERIES = (
    ('links', ('-j', '-d', 'link', 'show')),
    ('addresses', ('-j', '-d', 'address', 'show')),
    ('routes4', ('-j', '-4', 'route', 'show', 'table', 'all')),
    ('routes6', ('-j', '-6', 'route', 'show', 'table', 'all')),
)
# Capability/queue-size fields are reported by ip -d; unknown configuration is
# deliberately rejected rather than ignored when a kernel/ip version changes.
LINK_NUMBERS = {'min_mtu', 'max_mtu', 'num_tx_queues', 'num_rx_queues',
                'gso_max_size', 'gso_max_segs', 'tso_max_size', 'tso_max_segs',
                'gro_max_size', 'gso_ipv4_max_size', 'gro_ipv4_max_size'}
LINK_KEYS = LINK_NUMBERS | {'ifindex', 'ifname', 'flags', 'mtu', 'qdisc', 'operstate',
    'linkmode', 'group', 'txqlen', 'link_type', 'address', 'broadcast', 'promiscuity',
    'allmulti', 'inet6_addr_gen_mode', 'link', 'linkinfo', 'permaddr'}
TUNNEL_DATA = {'remote': 'any', 'local': 'any', 'ttl': 0, 'encap_limit': 0,
               'tclass': '0x00', 'flowlabel': '0x00000'}
FALLBACKS = {'ip6tnl0': ('ip6tnl', 'tunnel6', 1452),
             'ip6gre0': ('ip6gre', 'gre6', 1448)}


def need(condition, reason):
    if not condition:
        raise ValueError(reason)


def exact(actual, expected):
    # JSON equality distinguishes bool/int and validates nested default settings.
    return json.dumps(actual, sort_keys=True, allow_nan=False) == json.dumps(expected, sort_keys=True)


def validate_namespace(state):
    """Pure validation of the complete, unfiltered inspector result."""
    try:
        for key in ('netns', 'pid1_netns'):
            need(type(state[key]) is str and re.fullmatch(r'net:\[[0-9]+\]', state[key]),
                 'invalid namespace identity')
        need(state['netns'] != state['pid1_netns'], 'PID 1 network namespace is not isolated')
        interfaces = state['interfaces']
        need(type(interfaces) is list and interfaces, 'missing interface inventory')
        for pair in interfaces:
            need(type(pair) in (list, tuple) and len(pair) == 2 and type(pair[0]) is int
                 and pair[0] > 0 and type(pair[1]) is str, 'malformed interface inventory')
        ids = [item[0] for item in interfaces]
        names = [item[1] for item in interfaces]
        need(len(ids) == len(set(ids)) and len(names) == len(set(names)), 'duplicate interfaces')
        need(set(names) <= {'lo', *FALLBACKS} and (1, 'lo') in [tuple(x) for x in interfaces],
             'unknown interfaces or missing loopback')
        inventory = sorted(tuple(x) for x in interfaces)
        for collection in ('links', 'addresses'):
            entries = state[collection]
            need(type(entries) is list and all(type(x) is dict for x in entries),
                 'malformed ' + collection)
            need(all(type(x['ifindex']) is int and type(x['ifname']) is str for x in entries),
                 'malformed link identity')
            need(sorted((x['ifindex'], x['ifname']) for x in entries) == inventory,
                 collection + ' inventory disagrees with interfaces')
        links = {link['ifname']: link for link in state['links']}
        for collection in ('links', 'addresses'):
            for link in state[collection]:
                name = link['ifname']
                allowed = LINK_KEYS | ({'addr_info'} if collection == 'addresses' else set())
                need(set(link) <= allowed, 'unknown link configuration: ' + name)
                expected = {'promiscuity': 0, 'allmulti': 0, 'group': 'default', 'txqlen': 1000}
                if name == 'lo':
                    expected.update(ifindex=1, link_type='loopback', mtu=65536, qdisc='noqueue',
                                    operstate='UNKNOWN', flags=['LOOPBACK', 'UP', 'LOWER_UP'],
                                    address='00:00:00:00:00:00', broadcast='00:00:00:00:00:00')
                    need(not {'link', 'linkinfo', 'permaddr'} & set(link), 'configured loopback link')
                else:
                    kind, link_type, mtu = FALLBACKS[name]
                    data = dict(TUNNEL_DATA, **({'proto': 'ip6ip6'} if kind == 'ip6tnl' else {}))
                    expected.update(link=None, link_type=link_type, mtu=mtu, qdisc='noop',
                                    operstate='DOWN', flags=['NOARP'], address='::', broadcast='::',
                                    linkinfo={'info_kind': kind, 'info_data': data})
                    if 'permaddr' in link:
                        need(type(link['permaddr']) is str, 'malformed permanent tunnel address')
                        ipaddress.IPv6Address(link['permaddr'])
                if collection == 'links':
                    expected['linkmode'] = 'DEFAULT'
                need(all(key in link and exact(link[key], value) for key, value in expected.items()),
                     'nondefault or active link: ' + name)
                need('inet6_addr_gen_mode' not in link or link['inet6_addr_gen_mode'] == 'eui64',
                     'configured address generation: ' + name)
                for key in LINK_NUMBERS & set(link):
                    need(type(link[key]) is int and link[key] >= 0, 'malformed link capability')
                if collection == 'addresses':
                    need(all(exact(value, links[name][key]) for key, value in link.items()
                             if key != 'addr_info'), 'link changed during address inspection')
                    addresses = link['addr_info']
                    need(type(addresses) is list, 'missing address inventory')
                    if name != 'lo':
                        need(addresses == [], 'addressed fallback device: ' + name)
                        continue
                    seen = []
                    for address in addresses:
                        need(type(address) is dict and set(address) <= {
                            'family', 'local', 'prefixlen', 'scope', 'label', 'protocol',
                            'valid_life_time', 'preferred_life_time'}, 'unknown address configuration')
                        key = (address['family'], address['local'], address['prefixlen'])
                        need(type(key[2]) is int and key in (('inet', '127.0.0.1', 8), ('inet6', '::1', 128))
                             and address['scope'] == 'host', 'non-loopback address or prefix')
                        need(address.get('label', 'lo') == 'lo'
                             and address.get('protocol', 'kernel_lo') == 'kernel_lo', 'configured loopback address')
                        for lifetime in ('valid_life_time', 'preferred_life_time'):
                            need(type(address[lifetime]) is int and address[lifetime] == 4294967295,
                                 'nondefault address lifetime')
                        seen.append(key)
                    need(('inet', '127.0.0.1', 8) in seen and len(seen) == len(set(seen)),
                         'missing or duplicate loopback address')
        for key, network in (('routes4', '127.0.0.0/8'), ('routes6', '::1/128')):
            routes = state[key]
            need(type(routes) is list, 'missing all-table route inventory: ' + key)
            seen_routes = set()
            for route in routes:
                need(type(route) is dict and set(route) <= {'type', 'dst', 'dev', 'table',
                    'protocol', 'scope', 'prefsrc', 'flags', 'metric', 'pref'}, 'unknown/indirect route')
                need(route['dev'] == 'lo' and route['table'] == 'local' and route['protocol'] == 'kernel'
                     and route['flags'] == [] and route['type'] in ('local', 'broadcast'), 'non-loopback route')
                need(type(route['dst']) is str, 'malformed route destination')
                destination = ipaddress.ip_network(route['dst'], strict=False)
                need(destination.subnet_of(ipaddress.ip_network(network)), 'non-loopback route destination')
                if 'prefsrc' in route:
                    need(type(route['prefsrc']) is str, 'malformed route source')
                    source = ipaddress.ip_address(route['prefsrc'])
                    need(source.version == destination.version and source.is_loopback, 'non-loopback route source')
                need(route.get('scope', 'host') in ('host', 'link')
                     and route.get('pref', 'medium') == 'medium', 'configured loopback route')
                if 'metric' in route:
                    need(type(route['metric']) is int and route['metric'] == 0, 'nondefault route metric')
                identity = (route['type'], str(destination), route['dev'], route['table'], route.get('metric', 0))
                need(identity not in seen_routes, 'duplicate ' + key + ' route identity')
                seen_routes.add(identity)
    except (KeyError, TypeError, ValueError, AttributeError) as exc:
        raise NamespaceError(str(exc), state) from exc
    return state


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        need(key not in result, 'duplicate JSON inspection key: ' + key)
        result[key] = value
    return result


def inspect_namespace():
    """Read netlink via ip only; command failures retain the raw partial evidence."""
    state = {'inspection': []}
    paths = {'netns': '/proc/self/ns/net', 'thread_netns': '/proc/thread-self/ns/net',
             'pid1_netns': '/proc/1/ns/net'}
    try:
        state.update({key: os.readlink(path) for key, path in paths.items()})
        state['interfaces'] = [list(item) for item in socket.if_nameindex()]
        need(state['netns'] == state['thread_netns'], 'thread and process namespaces differ')
        executable = shutil.which('ip')
        need(executable is not None, 'ip utility is unavailable')
        for key, arguments in IP_QUERIES:
            command = [executable, *arguments]
            entry = {'command': command}
            state['inspection'].append(entry)
            stdout = stderr = b''
            try:
                result = subprocess.run(command, capture_output=True, timeout=5, check=False)
                stdout, stderr = result.stdout, result.stderr
                entry['exit'] = result.returncode
            except Exception as exc:
                stdout, stderr = getattr(exc, 'stdout', None) or b'', getattr(exc, 'stderr', None) or b''
                entry['error'] = repr(exc)
                raise
            finally:
                entry.update(stdout_b64=base64.b64encode(stdout).decode('ascii'),
                             stderr_b64=base64.b64encode(stderr).decode('ascii'))
            entry.update(stdout=stdout.decode('utf-8'), stderr=stderr.decode('utf-8'))
            need(result.returncode == 0 and stderr == b'', 'ip inspection failed: ' + key)
            state[key] = json.loads(entry['stdout'], object_pairs_hook=unique_object,
                                   parse_constant=lambda value: need(False, 'nonfinite JSON: ' + value))
        after = {key: os.readlink(path) for key, path in paths.items()}
        after['interfaces'] = [list(item) for item in socket.if_nameindex()]
        state['after'] = after
        need(all(exact(state[key], value) for key, value in after.items()),
             'namespace or interfaces changed during inspection')
    except Exception as exc:
        raise NamespaceError('inspection failed: ' + repr(exc), state) from exc
    return state


def require_namespace():
    first = validate_namespace(inspect_namespace())
    second = validate_namespace(inspect_namespace())
    if not exact(first, second):
        raise NamespaceError('network state changed between inspections', {'first': first, 'second': second})
    return dict(second, initial_snapshot=first)
