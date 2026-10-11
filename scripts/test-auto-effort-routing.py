#!/usr/bin/env python3
"""Capture auto effort on a real gateway, using only synthetic type-1 channels.

Run in a fresh network namespace (the guard runs before every gateway launch):
  unshare --net -- sh -c 'ip link set dev lo up && exec python3 -B scripts/test-auto-effort-routing.py "$@"' sh /absolute/binary --cache true

The input binary is copied, never rebuilt or installed. All databases, logs and
incremental results.json live in a fresh directory under the required TMPDIR.
A failing wire assertion exits 1; fixture/bootstrap errors exit 2, not a RED.
Use --list-cases to inspect the finite manifest without starting any process.
"""

import argparse
import contextlib
import copy
import hashlib
import http.client
import http.server
import importlib.util
import json
import os
import pathlib
import secrets
import shutil
import socket
import sqlite3
import sys
import tempfile
import threading
import time
from typing import cast
import urllib.error
import urllib.request


spec = importlib.util.spec_from_file_location(
    "auto_sse_helpers", pathlib.Path(__file__).with_name("test-auto-sse-errors.py")
)
assert spec is not None and spec.loader is not None
sse = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sse)

PATHS = {"chat": "/v1/chat/completions", "responses": "/v1/responses"}
EFFORT_PATHS = {"chat": "reasoning_effort", "responses": "reasoning.effort"}
# Expectations are literal contract examples, not a copy of the Go classifier.
# gpt-5.6 accepts max in the baseline's independent official-model validator.
FAMILIES = (
    ("gpt", "gpt-5.6", "[Fast] vendor/GpT-5.6:preview", "max"),
    ("deepseek", "deepseek-v4-flash:0731", "v2/deepseek-ai/DeEpSeEk-v4-flash:0731", "max"),
    ("glm", "glm-5", "[Tenant] Z.AI/GlM-5.1", "max"),
    ("gemini", "gemini-3.1-pro", "router/google/GeMiNi-3.1-pro:preview", "high"),
    ("qwen3", "qwen3-235b-a22b", "[Edge] v2/tenant/QwEn3-235B:2026", "xhigh"),
    ("grok", "grok-4", "tenant/xai/GROK-4.1-fast", "xhigh"),
    ("kimi", "kimi-k2.5", "namespace/moonshotai/KiMi-K2.5", None),
    ("minimax", "MiniMax-M2.7", "[Batch] MiniMax/miniMAX-M2.7", None),
    ("unknown", "widget-2", "qwen3-provider/widget-2", "high"),
)
MODELS = {family: model for family, model, _, _ in FAMILIES}


def effort_state(value):
    return {"present": False} if value is None else {"present": True, "value": value}


def field_state(body, path):
    current = body
    for part in path.split("."):
        if not isinstance(current, dict) or part not in current:
            return {"present": False}
        current = current[part]
    return {"present": True, "value": current}


def set_effort(body, protocol, value):
    parent = body if protocol == "chat" else body.setdefault("reasoning", {})
    key = "reasoning_effort" if protocol == "chat" else "effort"
    if value is None:
        parent.pop(key, None)
    else:
        parent[key] = value


def route(model, effort, *, mapping=None, wire_model=None, passthrough=False):
    return {"selected_model": model, "mapping": mapping or {},
            "wire_model": wire_model or model, "effort": effort_state(effort),
            "passthrough": passthrough}


def make_case(case_id, protocol, routes, *, effort="max", named=False, convert=False):
    payload = {"model": routes[0]["selected_model"] if named else "auto",
               "stream": False, "store": False, "parallel_tool_calls": False}
    preserve = ["store", "parallel_tool_calls"]
    if protocol == "chat":
        payload.update({"messages": [{"role": "user", "content": "Synthetic effort fixture."}],
                        "max_tokens": 32})
    else:
        payload.update({"input": [{"role": "user", "content": "Synthetic effort fixture."}],
                        "max_output_tokens": 32, "background": False})
        preserve.append("background")
    if not convert:
        payload.update({"enable_thinking": True, "reasoning": {"summary": "detailed"}})
        preserve.extend(["enable_thinking", "reasoning.summary"])
        if protocol == "chat":
            payload.update({"thinking": {"type": "enabled", "budget_tokens": 0},
                            "chat_template_kwargs": {"enable_thinking": False}, "reasoning_split": True})
            preserve.extend(["thinking", "chat_template_kwargs", "reasoning_split"])
    set_effort(payload, protocol, effort)
    if any(item["passthrough"] for item in routes):
        payload["fixture_opaque"] = {"zero": 0, "disabled": False, "spacing": " a b "}
        preserve.append("fixture_opaque")
    wire_protocol = "responses" if convert else protocol
    expected = [{"channel": index, "model": item["wire_model"], "effort": item["effort"],
                 "path": f"/channel-{index}" + PATHS[wire_protocol],
                 "status": 503 if len(routes) > 1 and index == 1 else 200}
                for index, item in enumerate(routes, 1)]
    return {"case_id": case_id, "protocol": protocol, "wire_protocol": wire_protocol,
            "auto": not named, "convert": convert, "routes": routes, "payload": payload,
            "preserve": {key: field_state(payload, key) for key in preserve}, "expected": expected}


def build_cases():
    cases = []
    for protocol in PATHS:
        def add(name, routes, **kwargs):
            cases.append(make_case(protocol + "." + name, protocol, routes, **kwargs))
        for family, model, prefixed, want in FAMILIES:
            add("max." + family, [route(model, want)])
            add("prefixed." + family, [route(prefixed, want)])
            add("named." + family, [route(model, "max")], named=True)
        for family, value, want in (
            ("qwen3", "minimal", "minimal"), ("grok", "low", "low"),
            ("deepseek", "medium", "medium"), ("glm", "high", "high"),
            ("unknown", "xhigh", "xhigh"), ("gpt", "medium", "medium"),
            ("gemini", "low", "low"), ("kimi", "medium", None), ("minimax", "high", None),
        ):
            add("nonmax." + family, [route(MODELS[family], want)], effort=value)
        for family in ("qwen3", "kimi", "unknown"):
            add("absent." + family, [route(MODELS[family], None)], effort=None)
        for label, model in (("tagged", "[tenant/kimi]gemini-3.1-pro"),
                             ("namespaced", "vendor/[tier/gpt]unknown-model")):
            for supplied, wanted in (("max", "high"), ("medium", "medium")):
                add("prefix-tags." + label + "." + supplied, [route(model, wanted)], effort=supplied)
        target = "[Edge] v2/tenant/QwEn3-235B:2026"
        add("alias.chain", [route("gpt-fixture-alias", "xhigh", wire_model=target,
                                  mapping={"gpt-fixture-alias": "middle-alias", "middle-alias": target})])
        target = "deepseek-ai/DeEpSeEk-v4-flash:0731"
        add("alias.upstream-priority", [route("qwen3-fixture-alias", "max", wire_model=target,
                                             mapping={"qwen3-fixture-alias": target})])
        add("alias.unknown-target", [route("gpt-fixture-alias", "high", wire_model="vendor/opaque-2",
                                          mapping={"gpt-fixture-alias": "vendor/opaque-2"})])
        for family, want in (("qwen3", "xhigh"), ("kimi", None)):
            model = MODELS[family]
            # Passthrough bypasses aliases: classify the actual wire model, not this GPT target.
            add("passthrough." + family, [route(model, want, passthrough=True,
                                               mapping={model: "gpt-5.6"})])
        add("passthrough.named", [route(MODELS["qwen3"], "max", passthrough=True)], named=True)
        add("retry.gemini-gpt", [route(MODELS["gemini"], "high"), route(MODELS["gpt"], "max")])
        add("retry.kimi-deepseek", [route(MODELS["kimi"], None), route(MODELS["deepseek"], "max")])
    for named in (False, True):
        cases.append(make_case("chat.conversion." + ("named" if named else "auto"), "chat",
                               [route(MODELS["qwen3"], "max" if named else "xhigh")],
                               named=named, convert=True))
    identifiers = [case["case_id"] for case in cases]
    if len(identifiers) != len(set(identifiers)):
        raise RuntimeError("duplicate case identifiers")
    return {case["case_id"]: case for case in cases}


CASES = build_cases()


def guard_namespace():
    return sse.fixture_namespace.require_namespace()


@contextlib.contextmanager
def gateway(binary, root, port, cache, cleanup):
    guard_namespace()
    process, log, version = sse.start(binary, root, port, cache)
    try:
        yield version
    finally:
        sse.stop(process, log)
        cleanup.append({"pid": process.pid, "returncode": process.returncode,
                        "reaped": process.poll() is not None, "log_closed": log.closed})


def seed_case(root, token, upstream, case):
    sse.seed(root, token, upstream, "normal", secrets.token_hex(16))
    routes = list(case["routes"])
    if len(routes) == 1:
        routes.append(route("fixture-decoy", "high"))
    ratios = {"auto": 1}
    with sqlite3.connect(root / "one-api.db") as conn:
        for index, item in enumerate(routes, 1):
            model = item["selected_model"]
            settings = {"show_error_details": True, "proxy": "",
                        "pass_through_body_enabled": item["passthrough"]}
            priority = 100 if index == 1 else 0
            conn.execute('UPDATE channels SET name=?,models=?,base_url=?,model_mapping=?,setting=?,settings=?,param_override=?,priority=?,retry_times=NULL WHERE id=?',
                         ("effort-fixture-" + str(index), model, upstream + f"/channel-{index}",
                          json.dumps(item["mapping"]), json.dumps(settings), "{}", "{}", priority, index))
            conn.execute('UPDATE abilities SET model=?,priority=? WHERE channel_id=?', (model, priority, index))
            ratios.update({name: 1 for name in [model, item["wire_model"], *item["mapping"], *item["mapping"].values()]})
        options = {"ModelRatio": json.dumps(ratios), "CompletionRatio": json.dumps(ratios),
                   "RetryTimes": "1", "LogConsumeEnabled": "true",
                   "global.pass_through_request_enabled": "false",
                   "global.chat_completions_to_responses_policy": json.dumps(
                       {"enabled": case["convert"], "all_channels": True, "model_patterns": [".*"]})}
        for key, value in options.items():
            conn.execute('INSERT INTO options (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value',
                         (key, value))
        # Read the exact synthetic targets back before launching the gateway.
        configured = list(conn.execute('SELECT id,type,models,base_url,priority FROM channels ORDER BY id'))
        wanted = [(i, 1, r["selected_model"], upstream + f"/channel-{i}", 100 if i == 1 else 0)
                  for i, r in enumerate(routes, 1)]
        if configured != wanted:
            raise RuntimeError("synthetic channel seed readback mismatch")
    return configured


class FixtureServer(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self):
        guard_namespace()
        self.state_lock = threading.Lock()
        self.case = None
        self.calls = []
        super().__init__(("127.0.0.1", 0), Stub)


class Stub(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, format, *args):
        pass

    def read_request_body(self):
        # ReaderOnly passthrough deliberately has no Len(): Go sends chunked HTTP.
        self.connection.settimeout(5)
        limit = 1024 * 1024  # Synthetic fixtures only, never production payloads.
        transfer = self.headers.get("Transfer-Encoding", "").strip().lower()
        if not transfer:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 <= length <= limit:
                raise ValueError("invalid synthetic request length")
            body = self.rfile.read(length)
            if len(body) != length:
                raise ValueError("truncated synthetic request")
            return body
        if transfer != "chunked":
            raise ValueError("unsupported synthetic transfer encoding")
        chunks, total = [], 0
        while True:
            line = self.rfile.readline(8193)
            if len(line) > 8192 or not line.endswith(b"\r\n"):
                raise ValueError("invalid chunk header")
            length = int(line.split(b";", 1)[0], 16)
            if length < 0 or total + length > limit:
                raise ValueError("invalid synthetic chunk length")
            if length == 0:
                http.client.parse_headers(self.rfile)  # Consume optional trailers.
                return b"".join(chunks)
            chunk = self.rfile.read(length)
            if len(chunk) != length or self.rfile.read(2) != b"\r\n":
                raise ValueError("truncated synthetic chunk")
            chunks.append(chunk)
            total += length

    def do_POST(self):
        raw = self.read_request_body()
        body = json.loads(raw)
        state = cast(FixtureServer, self.server)
        with state.state_lock:
            case = state.case
            assert case is not None, "no active synthetic case"
            index = len(state.calls) + 1
            status = 503 if len(case["expected"]) > 1 and index == 1 else 200
            protocol = "responses" if self.path.endswith("/responses") else "chat"
            state.calls.append({"case_id": case["case_id"], "attempt": index, "path": self.path,
                                "model": body.get("model"), "upstream_status": status,
                                "effort": field_state(body, EFFORT_PATHS[protocol]),
                                "reasoning_effort": field_state(body, "reasoning_effort"),
                                "reasoning.effort": field_state(body, "reasoning.effort"),
                                "body": body, "raw_body": raw.decode(),
                                "framing": {"transfer_encoding": self.headers.get("Transfer-Encoding", ""),
                                            "content_length": self.headers.get("Content-Length", "")}})
        if status == 503:
            response = {"error": {"message": "Synthetic first-attempt 503", "type": "upstream_error",
                                  "code": "fixture_retry"}}
        elif protocol == "responses":
            response = {"id": "resp_fixture", "object": "response", "created_at": 1, "status": "completed",
                        "model": body.get("model"), "output": [{"id": "msg_fixture", "type": "message",
                        "role": "assistant", "status": "completed", "content": [{"type": "output_text",
                        "text": "fixture-ok", "annotations": []}]}],
                        "usage": {"input_tokens": 8, "output_tokens": 2, "total_tokens": 10}}
        else:
            response = {"id": "chat_fixture", "object": "chat.completion", "created": 1,
                        "model": body.get("model"), "choices": [{"index": 0, "message": {
                        "role": "assistant", "content": "fixture-ok"}, "finish_reason": "stop"}],
                        "usage": {"prompt_tokens": 8, "completion_tokens": 2, "total_tokens": 10}}
        encoded = json.dumps(response).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(encoded)
        self.wfile.flush()
        self.close_connection = True


def request_gateway(port, token, case, raw):
    request = urllib.request.Request(f"http://127.0.0.1:{port}" + PATHS[case["protocol"]], data=raw,
                                     headers={"Authorization": "Bearer sk-" + token,
                                              "Content-Type": "application/json"})
    try:
        response = urllib.request.urlopen(request, timeout=20)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return {"status": response.status, "body": response.read().decode(errors="replace"),
                "request_id": response.headers.get("X-Oneapi-Request-Id", "")}


def consume_logs(root, request_id):
    deadline = time.monotonic() + 3
    while True:
        with sqlite3.connect("file:" + str(root / "one-api.db") + "?mode=ro", uri=True) as conn:
            conn.row_factory = sqlite3.Row
            rows = [dict(row) for row in conn.execute(
                'SELECT model_name,channel_id,request_id,other FROM logs WHERE type=2 AND request_id=? ORDER BY id',
                (request_id,))]
        if rows or time.monotonic() >= deadline:
            for row in rows:
                row["other"] = json.loads(row["other"])
            return rows
        time.sleep(0.05)


def check_case(case, calls, response, logs, raw_request):
    failures = []
    expected = case["expected"]
    actual_models = [call["model"] for call in calls]
    wanted_models = [item["model"] for item in expected]
    if actual_models != wanted_models:
        failures.append(f"wire model sequence {actual_models!r} != {wanted_models!r}")
    if response.get("status") != 200 or "fixture-ok" not in response.get("body", ""):
        failures.append("client did not receive a successful synthetic response")
    if [call["upstream_status"] for call in calls] != [item["status"] for item in expected]:
        failures.append("wrong upstream status sequence (retry must be 503 then 200)")
    for index, (call, want) in enumerate(zip(calls, expected), 1):
        if call["path"] != want["path"]:
            failures.append(f"attempt {index} path {call['path']!r} != {want['path']!r}")
        if call["effort"] != want["effort"]:
            failures.append(f"attempt {index} wire effort {call['effort']!r} != {want['effort']!r}")
        if not want["effort"]["present"]:
            for path in EFFORT_PATHS.values():
                if field_state(call["body"], path)["present"]:
                    failures.append(f"attempt {index} omission requires absent {path}, not null/empty")
        for path, value in case["preserve"].items():
            if field_state(call["body"], path) != value:
                failures.append(f"attempt {index} changed unrelated {path}")
        if case["convert"] and field_state(call["body"], "reasoning.summary") != effort_state("detailed"):
            failures.append("Chat-to-Responses conversion lost reasoning.summary")
        if case["routes"][index - 1]["passthrough"]:
            wanted_body = copy.deepcopy(case["payload"])
            wanted_body["model"] = want["model"]
            set_effort(wanted_body, case["protocol"], want["effort"].get("value"))
            if call["body"] != wanted_body:
                failures.append(f"attempt {index} passthrough changed fields besides model/effort")
            if not case["auto"] and call["raw_body"] != raw_request:
                failures.append("named passthrough changed request bytes")
    if len(logs) != 1:
        failures.append(f"expected one request-correlated consume log, got {len(logs)}")
    elif calls:
        if field_state(logs[0]["other"], "reasoning_effort") != calls[-1]["effort"]:
            failures.append("usage metadata disagrees with actual final wire effort")
        if logs[0]["channel_id"] != expected[-1]["channel"]:
            failures.append("usage metadata recorded the wrong final channel")
    return failures


def run_case(binary, base, stub, cache, case):
    root = base / (cache + "-" + case["case_id"])
    root.mkdir()
    raw = json.dumps(case["payload"], ensure_ascii=False, indent=2).encode()
    result = {"id": cache + ":" + case["case_id"], "case_id": case["case_id"], "memory_cache": cache,
              "root": str(root), "expected": case["expected"], "request": case["payload"],
              "raw_request": raw.decode(), "calls": [], "cleanup": [], "failures": [], "status": "ERROR"}
    with stub.state_lock:
        stub.case, stub.calls = case, []
    stage = "bootstrap"
    try:
        port = sse.free_port()
        with gateway(binary, root, port, cache, result["cleanup"]) as version:
            result["version"], result["assets"] = version, sse.smoke_assets(port)
        stage = "seed"
        token = secrets.token_hex(24)
        result["channels"] = seed_case(root, token, f"http://127.0.0.1:{stub.server_port}", case)
        stage = "dispatch"
        with gateway(binary, root, port, cache, result["cleanup"]):
            result["response"] = request_gateway(port, token, case, raw)
            result["usage_logs"] = consume_logs(root, result["response"]["request_id"])
        with stub.state_lock:
            result["calls"] = copy.deepcopy(stub.calls)
        result["failures"] = check_case(case, result["calls"], result["response"], result["usage_logs"], raw.decode())
        result["status"] = "FAIL" if result["failures"] else "PASS"
        if not result["calls"]:
            result["status"] = "ERROR"
            result["failures"].append("no upstream dispatch: this is not a wire RED")
        if len(case["expected"]) > 1:
            result["source_effort_preservation"] = {
                "client": field_state(case["payload"], EFFORT_PATHS[case["protocol"]]),
                "expected_attempts": [item["effort"] for item in case["expected"]],
                "actual_attempts": [call["effort"] for call in result["calls"]],
                "original_max_reached_second_attempt": len(result["calls"]) == 2
                    and result["calls"][1]["effort"] == effort_state("max")}
    except Exception as error:
        result["failures"].append(f"fixture {stage}: {type(error).__name__}: {error}")
    finally:
        with stub.state_lock:
            result["calls"] = copy.deepcopy(stub.calls)
    return result


def file_hash(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def persist(path, report):
    pending = path.with_suffix(".pending")
    pending.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    pending.replace(path)


def run():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", nargs="?", type=pathlib.Path)
    parser.add_argument("--cases", nargs="+", choices=list(CASES), default=list(CASES))
    parser.add_argument("--cache", choices=("false", "true", "both"), default="true")
    parser.add_argument("--list-cases", action="store_true")
    args = parser.parse_args()
    if args.list_cases:
        print(json.dumps({"declared_cases": len(CASES), "cases": list(CASES.values())}, ensure_ascii=False, indent=2))
        return 0
    if args.binary is None:
        parser.error("binary is required unless --list-cases is used")
    if len(args.cases) != len(set(args.cases)):
        parser.error("duplicate --cases selections are not allowed")
    isolation = guard_namespace()
    temp = pathlib.Path(os.environ.get("TMPDIR", ""))
    if not temp.is_absolute() or temp == pathlib.Path("/tmp") or pathlib.Path("/tmp") in temp.parents:
        parser.error("set TMPDIR to an absolute scratch directory outside /tmp")
    with contextlib.ExitStack() as bootstrap:
        source = args.binary.resolve(strict=True)
        base = pathlib.Path(tempfile.mkdtemp(prefix="auto-reasoning-effort-", dir=temp))
        bootstrap.callback(shutil.rmtree, base)
        binary = base / "new-api"
        shutil.copy2(source, binary)
        digest = file_hash(source)
        if file_hash(binary) != digest:
            raise RuntimeError("copied binary SHA256 mismatch")
        os.environ["HOME"] = str(base)
        urllib.request.install_opener(urllib.request.build_opener(urllib.request.ProxyHandler({})))
        modes = ("false", "true") if args.cache == "both" else (args.cache,)
        expected_ids = [cache + ":" + case_id for cache in modes for case_id in args.cases]
        report = {"source_binary": str(source), "executed_binary": str(binary), "sha256": digest,
                  "netns": isolation["netns"], "interfaces": isolation["interfaces"], "isolation": isolation,
                  "declared_cases": len(CASES), "expected_cases": len(expected_ids),
                  "expected_ids": expected_ids, "manifest": [CASES[key] for key in args.cases], "results": []}
        output = base / "results.json"
        persist(output, report)
        stub = FixtureServer()
        bootstrap.callback(stub.server_close)
        worker = threading.Thread(target=stub.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)
        worker.start()
        bootstrap.callback(worker.join, timeout=3)
        bootstrap.callback(stub.shutdown)
        # Keep completed bootstrap evidence; normal matrix cleanup takes ownership.
        bootstrap.pop_all()
    try:
        for cache in modes:
            for case_id in args.cases:
                result = run_case(binary, base, stub, cache, CASES[case_id])
                cleanup = result.get("cleanup", [])
                if len(cleanup) != 2 or any(not item.get("reaped") or not item.get("log_closed") for item in cleanup):
                    result["status"] = "ERROR"
                    result["failures"].append("gateway cleanup incomplete")
                report["results"].append(result)
                persist(output, report)
                print(json.dumps({"id": result["id"], "status": result["status"],
                                  "calls": [{key: call[key] for key in ("model", "effort", "upstream_status")}
                                            for call in result["calls"]], "failures": result["failures"]}), flush=True)
    finally:
        stub.shutdown()
        stub.server_close()
        worker.join(timeout=3)
        report["stub_stopped"] = not worker.is_alive()
        persist(output, report)
        print("EVIDENCE_DIR=" + str(base), flush=True)
    # Aggregate persisted evidence, not an assumed/mental count.
    saved = json.loads(output.read_text())
    ids = [item["id"] for item in saved["results"]]
    if ids != expected_ids or len(ids) != len(set(ids)) or not saved["stub_stopped"]:
        raise RuntimeError("incomplete/duplicate results or unreaped fixture server")
    totals = {status: sum(item["status"] == status for item in saved["results"])
              for status in ("PASS", "FAIL", "ERROR")}
    saved["summary"] = {"declared_cases": len(CASES), "selected_cases": len(args.cases),
                        "expected_cases": len(expected_ids), "collected_cases": len(ids), **totals}
    persist(output, saved)
    print(json.dumps({**saved["summary"], "sha256": digest, "results": str(output)}), flush=True)
    return 2 if totals["ERROR"] else (1 if totals["FAIL"] else 0)


def main():
    try:
        return run()
    except Exception as error:
        print(f"Infrastructure error: {type(error).__name__}: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
