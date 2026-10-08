#!/usr/bin/env python3
"""Exercise auto feedback using a real binary and synthetic loopback upstreams.

Never reads the production DB or keys. Each case gets a fresh SQLite database.
The binary binds all interfaces: isolate its bootstrap setup API in a netns.
Usage (from the repository root, with permission to create a network namespace):
    unshare --net -- sh -c 'ip link set dev lo up && exec python3 scripts/test-auto-sse-errors.py "$@"' sh /absolute/binary ...
"""

import argparse
import hashlib
import http.server
import json
import os
import pathlib
import re
import secrets
import socket
import sqlite3
import subprocess
import tempfile
import threading
import time
from typing import cast
import urllib.error
import urllib.parse
import urllib.request


CASES = (
    "normal", "direct_banner", "error_envelope", "hidden_error",
    "formatted_error", "thinking_error", "tool_calls", "reasoning", "null_error",
    "http_error", "unknown_error", "escaped_error",
)
BAD_CASES = {"direct_banner", "error_envelope", "hidden_error", "formatted_error", "thinking_error", "http_error", "escaped_error"}
ERROR_CASES = BAD_CASES - {"direct_banner", "http_error"}
WARNING = "\n\n[Gateway Warning: 当前渠道负"
ERROR = {"error": {
    "message": 'All candidates failed. Last upstream error (200): data: {"choices":[{"delta":{"content":"\\n\\n[Gateway Warning:',
    "type": "upstream_error", "code": "upstream_error",
}}


def fixture_chunk(case, model):
    if model == "probe-target" and case in ERROR_CASES:
        return ERROR
    delta = {"role": "assistant", "content": "fixture-ok:" + model}
    finish = "stop"
    if model == "probe-target":
        if case == "direct_banner":
            delta["content"] = WARNING
        elif case == "tool_calls":
            delta = {"tool_calls": [{"index": 0, "id": "call_fixture", "type": "function",
                                      "function": {"name": "fixture", "arguments": "{}"}}]}
            finish = "tool_calls"
        elif case == "reasoning":
            delta = {"reasoning_content": "Synthetic reasoning control."}
    result = {"id": "fixture-completion", "object": "chat.completion.chunk", "created": 1,
              "model": model, "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
    if model == "probe-target" and case == "null_error":
        result["error"] = None
    elif model == "probe-target" and case == "unknown_error":
        result["error"] = {"metadata": "informational fixture"}
    return result


class FixtureServer(http.server.ThreadingHTTPServer):
    def __init__(self, address):
        self.state_lock = threading.Lock()
        self.case = ""
        self.calls = []
        super().__init__(address, Stub)


class Stub(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        state = cast(FixtureServer, self.server)
        with state.state_lock:
            case = state.case
            model = request.get("model")
            state.calls.append({"case": case, "model": model, "path": self.path})
        if case == "http_error" and model == "probe-target":
            body = json.dumps({"error": {"message": "Synthetic HTTP 400 upstream rejection",
                                          "type": "invalid_request_error",
                                          "code": "fixture_rejected"}}).encode()
            status, content_type = 400, "application/json"
        else:
            body = ("data: " + json.dumps(fixture_chunk(case, model), ensure_ascii=False)
                    + "\n\ndata: [DONE]\n\n").encode()
            if case == "escaped_error" and model == "probe-target":
                body = body.replace(b'"error":', br'"\u0065rror":', 1)
            status, content_type = 200, "text/event-stream"
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        self.wfile.flush()


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def stop(process, log):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=8)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)
    log.close()


def start(binary, root, port, cache):
    if {name for _, name in socket.if_nameindex()} != {"lo"}:
        raise RuntimeError(
            "Unsafe bootstrap: an isolated network namespace containing only lo is required; "
            "run with unshare --net and 'ip link set dev lo up' (see module docstring)."
        )
    env = {key: os.environ[key] for key in ("PATH", "HOME", "LANG", "TMPDIR") if key in os.environ}
    env.update({"SQLITE_PATH": str(root / "one-api.db"), "GIN_MODE": "release",
                "MEMORY_CACHE_ENABLED": cache, "SESSION_SECRET": secrets.token_hex(32),
                "ERROR_LOG_ENABLED": "true"})
    log = (root / ("process-" + str(time.time_ns()) + ".log")).open("wb")
    try:
        process = subprocess.Popen([str(binary), "--port", str(port), "--log-dir", str(root / "logs")],
                                   cwd=root, env=env, stdout=log, stderr=subprocess.STDOUT)
    except BaseException:
        log.close()
        raise
    deadline = time.monotonic() + 15
    try:
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError("isolated process exited: " + str(root))
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}/api/status", timeout=1) as response:
                    status = json.load(response)
                if status.get("success"):
                    return process, log, status["data"]["version"]
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                pass
            time.sleep(0.1)
        raise RuntimeError("isolated startup deadline exceeded: " + str(root))
    except BaseException:
        stop(process, log)
        raise


def seed(root, token, upstream, case, admin_token):
    now = int(time.time())
    with sqlite3.connect(root / "one-api.db") as conn:
        conn.execute('INSERT INTO users (id,username,password,display_name,role,status,quota,used_quota,request_count,"group",aff_code,aff_count,aff_quota) VALUES (1,\'fixture\',\'unusable-password-hash\',\'fixture\',100,1,100000000,0,0,\'default\',\'fixture\',0,0)')
        conn.execute('UPDATE users SET access_token=? WHERE id=1', (admin_token,))
        conn.execute('INSERT INTO tokens (id,user_id,key,status,name,created_time,accessed_time,expired_time,remain_quota,unlimited_quota,model_limits_enabled,model_limits,"group",groups,cross_group_retry) VALUES (1,1,?,1,\'fixture\',?,?,-1,100000000,1,0,\'\',\'default\',\'default\',0)', (token, now, now))
        for channel, model, priority in [(1, "probe-target", 100), (2, "probe-ok", 0)]:
            settings = {"show_error_details": case != "hidden_error"}
            if channel == 1 and case == "formatted_error":
                settings["force_format"] = True
            if channel == 1 and case == "thinking_error":
                settings["thinking_to_content"] = True
            conn.execute('INSERT INTO channels (id,type,key,status,name,weight,created_time,base_url,models,"group",used_quota,priority,auto_ban,setting) VALUES (?,1,\'fixture-only\',1,?,0,?,?,?,\'default\',0,?,0,?)',
                         (channel, model, now, upstream, model, priority, json.dumps(settings)))
            conn.execute('INSERT INTO abilities ("group",model,channel_id,enabled,priority,weight,tag) VALUES (\'default\',?,?,1,?,0,\'\')', (model, channel, priority))
        options = {"ModelRatio": json.dumps({"probe-target": 1, "probe-ok": 1}), "RetryTimes": "3",
                   "AutomaticRetryStatusCodes": "401-403,413,429,500-599",
                   "performance_setting.monitor_enabled": "false"}
        for key, value in options.items():
            conn.execute("INSERT INTO options (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", (key, value))


def ask(root, port, token, index):
    payload = {"model": "auto", "messages": [{"role": "user", "content": "Synthetic routing fixture."}],
               "max_tokens": 32, "stream": True}
    request = urllib.request.Request(f"http://127.0.0.1:{port}/v1/chat/completions",
                                     data=json.dumps(payload).encode(),
                                     headers={"Authorization": "Bearer sk-" + token, "Content-Type": "application/json"})
    try:
        response = urllib.request.urlopen(request, timeout=15)
    except urllib.error.HTTPError as exc:
        response = exc
    with response:
        body = response.read()
        (root / f"response-{index}.sse").write_bytes(body)
        result = {"status": response.status, "body": body.decode(errors="replace"),
                  "request_id": response.headers.get("X-Oneapi-Request-Id", "")}
    # The handler flushes deferred feedback just after its final SSE frame.
    time.sleep(0.15)
    return result


def rows(root):
    with sqlite3.connect("file:" + str(root / "one-api.db") + "?mode=ro", uri=True) as conn:
        conn.row_factory = sqlite3.Row
        return [dict(row) for row in conn.execute('SELECT "group",model,channel_id,level,reason FROM auto_model_cooldowns ORDER BY id')]


def usage_error_rows(root):
    with sqlite3.connect("file:" + str(root / "one-api.db") + "?mode=ro", uri=True) as conn:
        conn.row_factory = sqlite3.Row
        return [dict(row) for row in conn.execute(
            'SELECT id,type,user_id,token_id,token_name,model_name,channel_id AS channel,'
            '"group",is_stream,quota,prompt_tokens,completion_tokens,request_id,content,other '
            'FROM logs WHERE type=5 ORDER BY id')]


def usage_log_views(port, admin_token, token, request_id):
    def get(path, headers):
        request = urllib.request.Request(f"http://127.0.0.1:{port}" + path, headers=headers)
        with urllib.request.urlopen(request, timeout=3) as response:
            payload = json.load(response)
        if payload.get("success") is not True:
            raise RuntimeError("usage-log API rejected fixture credentials or request")
        return payload["data"]

    headers = {"Authorization": "Bearer " + admin_token, "New-Api-User": "1"}
    query = urllib.parse.urlencode({"type": 5, "request_id": request_id})
    missing = urllib.parse.urlencode({"type": 5, "request_id": "missing-" + request_id})
    return {"root": get("/api/log/?" + query, headers),
            "missing": get("/api/log/?" + missing, headers),
            "token": get("/api/log/token", {"Authorization": "Bearer sk-" + token})}


def check_usage_logs(case, responses, persisted, views):
    failures = []
    wanted = 1 if case in BAD_CASES else 0
    request_id = responses[0].get("request_id", "")
    if not request_id or any(not response.get("request_id") for response in responses):
        failures.append("missing request id for usage error log correlation")
    if len(persisted) != wanted:
        failures.append(f"wrong persisted usage error log count: {len(persisted)} != {wanted}")
    expected = {"type": 5, "user_id": 1, "token_id": 1, "token_name": "fixture",
                "model_name": "probe-target", "channel": 1, "group": "default", "is_stream": True,
                "quota": 0, "prompt_tokens": 0, "completion_tokens": 0, "request_id": request_id}
    for row in persisted:
        if any(row.get(key) != value for key, value in expected.items()):
            failures.append("usage error log lost request/model/channel context or changed billing")
        other = json.loads(row["other"])
        status = 400 if case == "http_error" else 200
        if not row["content"] or other.get("status_code") != status or not other.get("error_code"):
            failures.append("usage error log lost its diagnostic reason or original status")
    root = views["root"]
    if root["total"] != wanted or len(root["items"]) != wanted:
        failures.append("type/request filtered usage error logs disagree with persistence")
    fields = ("request_id", "content", "model_name", "channel", "other")
    if [[row.get(key) for key in fields] for row in root["items"]] != \
            [[row.get(key) for key in fields] for row in persisted]:
        failures.append("root usage error log view changed persisted diagnostic details")
    if views["missing"]["total"] != 0 or views["missing"]["items"]:
        failures.append("usage error log request-id filter returned another request")
    private = [row for row in views["token"] if row["type"] == 5]
    if len(private) != wanted:
        failures.append("token usage error log view has the wrong row count")
    for row in private:
        other = json.loads(row["other"])
        if (set(other) != {"error_code", "error_type", "status_code"}
                or row["content"] != other.get("error_code")
                or row["request_id"] != request_id):
            failures.append("token usage error log view leaked root-only details or lost correlation")
    return failures


def smoke_assets(port):
    base = f"http://127.0.0.1:{port}"
    with urllib.request.urlopen(base + "/", timeout=3) as response:
        page = response.read()
        assert response.status == 200 and b"<html" in page.lower(), "missing embedded index"
    match = re.search(rb'src="(/assets/[^\"]+\.js)"', page)
    assert match, "index has no bundled JS"
    asset = match.group(1).decode()
    with urllib.request.urlopen(base + asset, timeout=3) as response:
        content = response.read()
        assert response.status == 200 and len(content) > 100, "missing embedded JS"
    return {"index_bytes": len(page), "asset": asset, "asset_bytes": len(content)}


def run_case(binary, base, stub, cache, case, baseline):
    root = base / (cache + "-" + case)
    root.mkdir()
    port = free_port()
    process, log, version = start(binary, root, port, cache)
    try:
        assets = smoke_assets(port)
    finally:
        stop(process, log)
    token = secrets.token_hex(24)
    admin_token = secrets.token_hex(16)
    seed(root, token, f"http://127.0.0.1:{stub.server_port}", case, admin_token)
    with stub.state_lock:
        stub.case = case
        begin = len(stub.calls)
    process, log, _ = start(binary, root, port, cache)
    try:
        responses = [ask(root, port, token, index) for index in (1, 2)]
        before_restart = rows(root)
    finally:
        stop(process, log)
    process, log, _ = start(binary, root, port, cache)
    try:
        responses.append(ask(root, port, token, 3))
        cooldowns = rows(root)
        error_logs = usage_error_rows(root)
        log_views = usage_log_views(port, admin_token, token, responses[0].get("request_id", ""))
    finally:
        stop(process, log)
    with stub.state_lock:
        calls = list(stub.calls[begin:])
    expected = ["probe-target"] + ["probe-ok" if case in BAD_CASES else "probe-target"] * 2
    if case == "http_error":
        expected.insert(1, "probe-ok")  # The first request can retry before any bytes were sent.
    failures = check_usage_logs(case, responses, error_logs, log_views)
    if [item["model"] for item in calls] != expected:
        failures.append("wrong route sequence, expected " + repr(expected))
    if any(response["status"] != 200 for response in responses):
        failures.append("fixture failed outside the HTTP-200 relay path")
    for stage, observed in (("before_restart", before_restart), ("after_restart", cooldowns)):
        keys = [(row["group"], row["model"], row["channel_id"], row["level"]) for row in observed]
        wanted = [("default", "probe-target", 1, 1)] if case in BAD_CASES else []
        if keys != wanted:
            failures.append(stage + " wrong cooldowns: " + repr(keys))
    if case in ERROR_CASES - {"formatted_error", "thinking_error"}:
        frames = [json.loads(line[6:]) for line in responses[0]["body"].splitlines()
                  if line.startswith("data: {")]
        errors = [frame for frame in frames if isinstance(frame.get("error"), dict)]
        if not errors or errors[0]["error"].get("code") != "upstream_error":
            failures.append("client lost the upstream error envelope")
        if case != "hidden_error" and errors and errors[0] != ERROR:
            failures.append("client error envelope was altered")
    if baseline is not None and any(responses[0][key] != baseline["responses"][0][key]
                                    for key in ("status", "body")):
        failures.append("first client response changed from the recorded baseline")
    return {"case": case, "memory_cache": cache, "version": version, "assets": assets,
            "calls": calls, "responses": responses, "before_restart": before_restart,
            "cooldowns": cooldowns, "error_logs": error_logs, "log_views": log_views,
            "failures": failures}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=pathlib.Path)
    parser.add_argument("--cache", choices=("false", "true", "both"), default="both")
    parser.add_argument("--cases", nargs="+", choices=CASES, default=list(CASES))
    parser.add_argument("--baseline-results", type=pathlib.Path,
                        help="also assert unchanged first-response bytes against a prior run")
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    base = pathlib.Path(tempfile.mkdtemp(prefix="auto-sse-errors-", dir=os.environ.get("TMPDIR")))
    modes = ("false", "true") if args.cache == "both" else (args.cache,)
    baseline = {}
    if args.baseline_results:
        prior = json.loads(args.baseline_results.read_text())["results"]
        baseline = {(item["memory_cache"], item["case"]): item for item in prior}
        if any((cache, case) not in baseline for cache in modes for case in args.cases):
            parser.error("baseline does not contain every requested case")
    with binary.open("rb") as source:
        binary_hash = hashlib.file_digest(source, "sha256").hexdigest()
    stub = FixtureServer(("127.0.0.1", 0))
    worker = threading.Thread(target=stub.serve_forever, daemon=True)
    worker.start()
    results = []
    try:
        for cache in modes:
            for case in args.cases:
                result = run_case(binary, base, stub, cache, case, baseline.get((cache, case)))
                results.append(result)
                (base / "results.json").write_text(json.dumps({"binary": str(binary), "sha256": binary_hash,
                    "expected_cases": len(modes) * len(args.cases), "results": results}, ensure_ascii=False, indent=2))
                print(json.dumps({"case": case, "cache": cache, "calls": [item["model"] for item in result["calls"]],
                                  "cooldown_count": len(result["cooldowns"]), "failures": result["failures"]}, ensure_ascii=False), flush=True)
    finally:
        stub.shutdown()
        stub.server_close()
        worker.join(timeout=3)
        print("EVIDENCE_DIR=" + str(base), flush=True)
    failed = sum(bool(result["failures"]) for result in results)
    print(json.dumps({"total": len(results), "failed": failed, "sha256": binary_hash}), flush=True)
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
