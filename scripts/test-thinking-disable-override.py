#!/usr/bin/env python3
"""真机验证:渠道级 param_override 是否真的把「关闭思考」送达上游。

背景:用户报告 agentrouter.org 报
  status_code=400, The content[].thinking in the thinking mode must be passed back to the API
根因(见插件 README 的对照实验):请求带 tools 且历史里有"带工具调用但没有非空思考"
的 assistant 回合时,严格 thinking-mode 端点必然拒绝。诚实解法是显式关闭思考,而不是
伪造思考。

本次要证明的链路:
  channels.param_override(带 model 条件)
    -> SetupContextForSelectedChannel 读入
    -> ApplyParamOverrideWithRelayInfo 应用
    -> 出站 body 真的带上了 thinking.type=disabled 与 reasoning_effort=none

同时必须证明**不误伤**:另一个模型名(gpt-6-astra)不得被注入关闭参数,因为
上游明确拒绝 none(用户日志:
  model "gpt-6-astra" does not support reasoning_effort="none")。

隔离:整个网关与假上游都在 unshare --net 内,外部网络不可达。
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
import time
import urllib.request

SPEC = importlib.util.spec_from_file_location(
    'auto_sse_helpers', pathlib.Path(__file__).with_name('test-auto-sse-errors.py'))
assert SPEC is not None and SPEC.loader is not None
h = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(h)

OVERRIDE = json.dumps({
    "operations": [
        {"mode": "set", "path": "thinking", "value": {"type": "disabled"},
         "conditions": [{"path": "model", "mode": "prefix", "value": "deepseek-v4"}]},
        {"mode": "set", "path": "reasoning_effort", "value": "none",
         "conditions": [{"path": "model", "mode": "prefix", "value": "deepseek-v4"}]},
    ]
})


class CapturingUpstream:
    """假上游:记录收到的出站 body,并回一个正常完成。"""

    def __init__(self):
        self.captured = []
        self.server = None
        self.thread = None
        self.port = h.free_port()

    def __enter__(self):
        import http.server
        import threading
        captured = self.captured

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'

            def do_POST(self):  # noqa: N802
                length = int(self.headers.get('content-length') or 0)
                raw = self.rfile.read(length).decode('utf-8', 'replace')
                try:
                    captured.append(json.loads(raw))
                except Exception:  # noqa: BLE001
                    captured.append({'__unparsed__': raw[:2000]})
                body = json.dumps({
                    "id": "fixture", "object": "chat.completion", "created": 1,
                    "model": "probe", "choices": [{
                        "index": 0, "finish_reason": "stop",
                        "message": {"role": "assistant", "content": "fixture-ok"},
                    }],
                    "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                }).encode()
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):  # noqa: A003
                pass

        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', self.port), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        return self

    def __exit__(self, *exc):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        return False


def run(binary, root, cache):
    """一个 cache 模式下跑两个模型:deepseek 必须被注入,gpt-6-astra 必须不被注入。"""
    failures, errors = [], []
    root.mkdir(parents=True, exist_ok=True)
    port, token, admin_token = h.free_port(), secrets.token_hex(24), secrets.token_hex(16)

    with CapturingUpstream() as upstream:
        bootstrap, blog, _ = h.start(binary, root, port, cache)
        h.stop(bootstrap, blog)
        # 复用既有 seed,再改写渠道 1 的 param_override 与模型。
        h.seed(root, token, f'http://127.0.0.1:{upstream.port}', 'normal', admin_token)
        with sqlite3.connect(root / 'one-api.db') as conn:
            conn.execute(
                'UPDATE channels SET param_override=?, models=? WHERE id=1',
                (OVERRIDE, 'deepseek-v4-flash,gpt-6-astra'))
            conn.execute('DELETE FROM abilities')
            now = int(time.time())
            for model in ('deepseek-v4-flash', 'gpt-6-astra'):
                conn.execute(
                    'INSERT INTO abilities ("group",model,channel_id,enabled,priority,weight,tag)'
                    ' VALUES (\'default\',?,1,1,100,0,\'\')', (model,))
            conn.execute(
                "UPDATE options SET value=? WHERE key='ModelRatio'",
                (json.dumps({'deepseek-v4-flash': 1, 'gpt-6-astra': 1}),))

        process, log, _ = h.start(binary, root, port, cache)
        try:
            for model, should_disable in (('deepseek-v4-flash', True), ('gpt-6-astra', False)):
                before = len(upstream.captured)
                payload = json.dumps({
                    "model": model, "stream": False,
                    "messages": [
                        {"role": "user", "content": "hi"},
                        {"role": "assistant", "content": None,
                         "tool_calls": [{"id": "c1", "type": "function",
                                         "function": {"name": "f", "arguments": "{}"}}]},
                        {"role": "tool", "tool_call_id": "c1", "content": "ok"},
                    ],
                    "tools": [{"type": "function",
                               "function": {"name": "f",
                                            "parameters": {"type": "object", "properties": {}}}}],
                }).encode()
                request = urllib.request.Request(
                    f'http://127.0.0.1:{port}/v1/chat/completions', data=payload,
                    headers={'Authorization': 'Bearer ' + token,
                             'Content-Type': 'application/json'})
                try:
                    with urllib.request.urlopen(request, timeout=20) as response:
                        response.read()
                except Exception as exc:  # noqa: BLE001
                    failures.append(f'{model}: request failed {exc!r}')
                    continue
                if len(upstream.captured) <= before:
                    failures.append(f'{model}: upstream never received a request')
                    continue
                sent = upstream.captured[-1]
                thinking = sent.get('thinking')
                effort = sent.get('reasoning_effort')
                disabled = (thinking == {'type': 'disabled'}) and (effort == 'none')
                if disabled != should_disable:
                    failures.append(
                        f'{model}: thinking={thinking!r} effort={effort!r}, '
                        f'expected disabled={should_disable}')
                # 历史与工具必须原样保留。只在 Chat 形态下检查 messages:
                # 走 Chat->Responses 转换的模型(这里 gpt-6-astra)出站是 input[],
                # 那是协议转换不是改写,断言 messages 会误报。
                if disabled:
                    if len(sent.get('messages') or []) != 3:
                        failures.append(f'{model}: messages mutated: {sent.get("messages")!r}')
                    if len(sent.get('tools') or []) != 1:
                        failures.append(f'{model}: tools mutated')
        finally:
            h.stop(process, log)
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
    root = pathlib.Path(tempfile.mkdtemp(prefix='thinking-disable-'))
    try:
        failures, errors = run(binary, root, args.cache)
    finally:
        shutil.rmtree(root, ignore_errors=True)
    print(json.dumps({'cache': args.cache, 'failures': failures, 'fixture_errors': errors},
                     ensure_ascii=False))
    return 1 if failures else 2 if errors else 0


if __name__ == '__main__':
    sys.exit(main())