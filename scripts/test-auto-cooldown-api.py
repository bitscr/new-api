#!/usr/bin/env python3
"""Real-binary check of the auto cooldown admin API (list + clear).

Proves the panel's actual wire contract: the frontend sends the row identity as
QUERY params on DELETE (axios `params`), so a handler reading a JSON body or path
params would look correct in unit tests and silently fail in the browser.

Run from a loopback-only network namespace:
  unshare --net -- sh -c 'ip link set dev lo up && exec python3 -B scripts/test-auto-cooldown-api.py "$@"' sh /absolute/binary --cache false
Fresh DB under TMPDIR, never production. Behavior mismatch exits 1, fixture fault 2.
"""
import argparse
import contextlib
import importlib.util
import json
import os
import pathlib
import secrets
import shutil
import sqlite3
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

SPEC = importlib.util.spec_from_file_location(
    'auto_sse_helpers', pathlib.Path(__file__).with_name('test-auto-sse-errors.py'))
assert SPEC is not None and SPEC.loader is not None
h = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(h)

NOW = int(time.time())


def admin_get(port, admin_token, path):
    request = urllib.request.Request(
        'http://127.0.0.1:' + str(port) + path,
        headers={'Authorization': 'Bearer ' + admin_token, 'New-Api-User': '1'})
    with urllib.request.urlopen(request, timeout=5) as response:
        return json.load(response)


def admin_delete(port, admin_token, query):
    request = urllib.request.Request(
        'http://127.0.0.1:' + str(port) + '/api/option/auto_model_cooldowns?' + urllib.parse.urlencode(query),
        headers={'Authorization': 'Bearer ' + admin_token, 'New-Api-User': '1'}, method='DELETE')
    with urllib.request.urlopen(request, timeout=5) as response:
        return json.load(response)


def cooldown_rows(root):
    with sqlite3.connect('file:' + str(root / 'one-api.db') + '?mode=ro', uri=True) as conn:
        conn.row_factory = sqlite3.Row
        return [dict(row) for row in conn.execute(
            'SELECT "group",model,channel_id,level,reason,permanent,until FROM auto_model_cooldowns ORDER BY id')]


def seed(root, token, admin_token, upstream):
    h.seed(root, token, upstream, 'normal', admin_token)
    now = int(time.time())
    with sqlite3.connect(root / 'one-api.db') as conn, conn:
        # 一条永久级、一条抖动级:列表必须如实区分两者,而不是只看行数。
        conn.execute('INSERT INTO auto_model_cooldowns ("group",model,channel_id,until,level,reason,permanent,updated_at) '
                     "VALUES ('default','ghost-model',3,?,'1','model_not_found(HTTP 404)',1,?)", (now + 24 * 3600, now))
        conn.execute('INSERT INTO auto_model_cooldowns ("group",model,channel_id,until,level,reason,permanent,updated_at) '
                     "VALUES ('default','busy-model',2,?,'2','超时/失败(30000ms)',0,?)", (now + 900, now))


def run(binary, root, cache):
    failures, errors = [], []
    cleanup = []
    try:
        root.mkdir(parents=True, exist_ok=True)
        port, token, admin_token = h.free_port(), secrets.token_hex(24), secrets.token_hex(16)
        upstream = 'http://127.0.0.1:1'
        # 第一次启动只为了让程序建表:真实部署也是先起服务再有数据。
        bootstrap_process, bootstrap_log, _ = h.start(binary, root, port, cache)
        h.stop(bootstrap_process, bootstrap_log)
        seed(root, token, admin_token, upstream)
        process, log, _ = h.start(binary, root, port, cache)
        try:
            listed = admin_get(port, admin_token, '/api/option/auto_model_cooldowns')
            if listed.get('success') is not True:
                failures.append('cooldown list API rejected fixture admin credentials')
                return failures, errors
            rows = listed.get('data') or []
            if len(rows) != 2:
                failures.append(f'listed {len(rows)} cooldowns, expected the 2 seeded rows')
            by_model = {row.get('model'): row for row in rows}
            permanent = by_model.get('ghost-model') or {}
            if permanent.get('permanent') is not True:
                failures.append('list did not mark the deterministic failure as permanent')
            if not permanent.get('group') == 'default' or permanent.get('channel_id') != 3:
                failures.append('list lost the (group, model, channel) identity')
            if not permanent.get('reason'):
                failures.append('list dropped the cooldown reason')
            jitter = by_model.get('busy-model') or {}
            if jitter.get('permanent') is not False:
                failures.append('list marked a jitter cooldown as permanent')

            # 前端实际发的请求形态:identity 走 query params。
            cleared = admin_delete(port, admin_token,
                                   {'group': 'default', 'model': 'ghost-model', 'channel_id': 3})
            if cleared.get('success') is not True:
                failures.append('clear API rejected a query-param request the frontend actually sends')
            remaining = cooldown_rows(root)
            if {row['model'] for row in remaining} != {'busy-model'}:
                failures.append(f'clear removed the wrong rows: {[r["model"] for r in remaining]}')

            # 参数不全必须拒绝,否则一次误点会清掉整片冷却。
            for bad in ({'model': 'busy-model', 'channel_id': 2}, {'group': 'default', 'channel_id': 2},
                        {'group': 'default', 'model': 'busy-model'}, {'group': 'default', 'model': 'busy-model', 'channel_id': 'abc'}):
                if admin_delete(port, admin_token, bad).get('success') is not False:
                    failures.append(f'incomplete identity {bad} was not rejected')
            if len(cooldown_rows(root)) != 1:
                failures.append('a rejected clear still deleted rows')

            # 清除必须让 auto 重新可选,而不只是少了一行。
            after = admin_get(port, admin_token, '/api/option/auto_model_cooldowns')
            if {row.get('model') for row in (after.get('data') or [])} != {'busy-model'}:
                failures.append('list did not reflect the clear')
        finally:
            h.stop(process, log)
            cleanup.append({'pid': process.pid, 'reaped': process.poll() is not None, 'log_closed': log.closed})
    except Exception as exc:  # noqa: BLE001 - fixture boundary
        errors.append(type(exc).__name__ + ': ' + str(exc))
    finally:
        if any(not x['reaped'] or not x['log_closed'] for x in cleanup):
            errors.append('fixture cleanup incomplete')
    return failures, errors


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('binary')
    parser.add_argument('--cache', choices=['false', 'true'], default='false')
    args = parser.parse_args()
    # start() 直接把它当环境变量值,必须是字符串而不是 bool。
    cache = args.cache
    binary = pathlib.Path(args.binary).resolve()
    if not os.access(binary, os.X_OK):
        print(json.dumps({'error': 'binary not executable: ' + str(binary)}))
        return 2
    root = pathlib.Path(tempfile.mkdtemp(prefix='auto-cooldown-api-'))
    try:
        failures, errors = run(binary, root, cache)
    finally:
        shutil.rmtree(root, ignore_errors=True)
    print(json.dumps({'cache': args.cache, 'failures': failures, 'fixture_errors': errors}))
    return 1 if failures else 2 if errors else 0


if __name__ == '__main__':
    sys.exit(main())