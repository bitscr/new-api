#!/usr/bin/env python3
"""Real-binary check that the auto option panel accepts good values and refuses bad ones.

The regression this pins: `UpdateOption` rejects with **HTTP 200 + success:false**, so a
frontend that only looks for transport errors reports "saved" for a value the server threw
away. Both halves matter — a validator that rejects everything is as broken as one that
accepts anything, and the panel is the only place these values can be set.

Run from a loopback-only network namespace:
  unshare --net -- sh -c 'ip link set dev lo up && exec python3 -B scripts/test-auto-option-validation.py "$@"' sh /absolute/binary --cache false
"""
import argparse
import importlib.util
import json
import os
import pathlib
import secrets
import shutil
import sqlite3
import sys
import tempfile
import urllib.parse
import urllib.request

SPEC = importlib.util.spec_from_file_location(
    'auto_sse_helpers', pathlib.Path(__file__).with_name('test-auto-sse-errors.py'))
assert SPEC is not None and SPEC.loader is not None
h = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(h)

# (key, value, should_be_accepted)
CASES = [
    ('AutoModelMaxAttempts', '4', True),
    ('AutoModelMaxAttempts', '1', True),
    ('AutoModelScoreDecayMinutes', '30', True),
    ('AutoModelPermanentCooldownHours', '24', True),
    # 0 / 负数会让 auto 直接不可用。
    ('AutoModelMaxAttempts', '0', False),
    ('AutoModelScoreDecayMinutes', '-5', False),
    ('AutoModelPermanentCooldownHours', 'abc', False),
    # 封顶换算后小于首次窗口,阶梯语义自相矛盾。
    ('AutoModelPermanentCooldownMaxDays', '0', False),
    # 关键词:真实日志样本必须能存进去。
    ('AutoModelPermanentKeywords', 'function calling is not supported', True),
    ('AutoModelPermanentKeywords', '无权访问 超低特价专属 分组', True),
    # 会误伤或注定不生效的条目必须在写库前挡住。
    ('AutoModelPermanentKeywords', 'error', False),
    ('AutoModelPermanentKeywords', 'context_length_exceeded', False),
    ('AutoModelPermanentKeywords', 'x', False),
    # 一行合法 + 一行非法,整体必须拒绝。
    ('AutoModelPermanentKeywords', 'model_not_supported\nerror', False),
    # 候选白名单:两个\"看起来一样、后果相反\"的状态都必须能存。
    ('AutoModelCandidates', '', True),
    ('AutoModelCandidates', '{}', True),
    ('AutoModelCandidates', '{"default": ["gpt-4o"]}', True),
    # 空数组 = 该分组一个模型都不选。这是合法配置,不能当成非法挡掉。
    ('AutoModelCandidates', '{"default": []}', True),
    # 形状不对必须拒绝。
    ('AutoModelCandidates', '["gpt-4o"]', False),
    ('AutoModelCandidates', 'not json', False),
    # 组内权重。
    ('AutoModelWeights', '', True),
    ('AutoModelWeights', '{}', True),
    ('AutoModelWeights', '{"default": {"gpt-4o": 2}}', True),
    # 权重 0 = 该模型退出 auto,是合法配置。
    ('AutoModelWeights', '{"default": {"gpt-4o": 0}}', True),
    # 负数 / 非数字 / null 组必须拒绝。
    ('AutoModelWeights', '{"default": {"gpt-4o": -1}}', False),
    ('AutoModelWeights', '{"default": {"gpt-4o": "high"}}', False),
    ('AutoModelWeights', '{"default": null}', False),
    ('AutoModelWeights', '[]', False),
    # 总开关。面板上它是唯一一个**没有 JSON 校验器**的控件,共享的 validate()
    # 若按字段名无条件查校验器,保存会在前端抛异常被挡下——而 bun build 看不见。
    ('AutoModelEnabled', 'true', True),
    ('AutoModelEnabled', 'false', True),
    ('AutoModelEnabled', 'yes', False),
]


def put_option(port, admin_token, key, value):
    # UpdateOption 解析 JSON body(common.DecodeJson),不是表单。
    body = json.dumps({'key': key, 'value': value}).encode()
    request = urllib.request.Request(
        'http://127.0.0.1:' + str(port) + '/api/option/',
        data=body, method='PUT',
        headers={'Authorization': 'Bearer ' + admin_token, 'New-Api-User': '1',
                 'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=5) as response:
        return response.status, json.load(response)


def stored_option(root, key):
    with sqlite3.connect('file:' + str(root / 'one-api.db') + '?mode=ro', uri=True) as conn:
        row = conn.execute('SELECT value FROM options WHERE key=?', (key,)).fetchone()
    return None if row is None else row[0]


def run(binary, root, cache):
    failures, errors, cleanup = [], [], []
    try:
        root.mkdir(parents=True, exist_ok=True)
        port, token, admin_token = h.free_port(), secrets.token_hex(24), secrets.token_hex(16)
        # 先起一次让程序建表,再灌测试管理员。
        bootstrap, bootstrap_log, _ = h.start(binary, root, port, cache)
        h.stop(bootstrap, bootstrap_log)
        h.seed(root, token, 'http://127.0.0.1:1', 'normal', admin_token)
        process, log, _ = h.start(binary, root, port, cache)
        try:
            for key, value, accepted in CASES:
                before = stored_option(root, key)
                status, payload = put_option(port, admin_token, key, value)
                if status != 200:
                    failures.append(f'{key}={value!r} answered HTTP {status}, expected the 200+success contract')
                    continue
                if payload.get('success') is not accepted:
                    failures.append(f'{key}={value!r} accepted={payload.get("success")}, expected {accepted}'
                                    f' ({payload.get("message")})')
                    continue
                after = stored_option(root, key)
                if accepted:
                    if after is None:
                        failures.append(f'{key}={value!r} reported success but no value reached the DB')
                    elif str(after) != value:
                        failures.append(f'{key} stored {after!r}, expected {value!r}')
                elif after != before:
                    failures.append(f'{key}={value!r} was refused but the stored value still changed')
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
    binary = pathlib.Path(args.binary).resolve()
    if not os.access(binary, os.X_OK):
        print(json.dumps({'error': 'binary not executable: ' + str(binary)}))
        return 2
    root = pathlib.Path(tempfile.mkdtemp(prefix='auto-option-validation-'))
    try:
        failures, errors = run(binary, root, args.cache)
    finally:
        shutil.rmtree(root, ignore_errors=True)
    print(json.dumps({'cache': args.cache, 'cases': len(CASES), 'failures': failures, 'fixture_errors': errors}))
    return 1 if failures else 2 if errors else 0


if __name__ == '__main__':
    sys.exit(main())