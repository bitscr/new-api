"""Transport and safety controls for the synthetic effort receiver."""

import contextlib
import copy
import http.client
import importlib.util
import io
import json
import os
import pathlib
import sys
import tempfile
import threading
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "effort_fixture", pathlib.Path(__file__).with_name("test-auto-effort-routing.py")
)
assert spec is not None and spec.loader is not None
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class EffortFixtureTests(unittest.TestCase):
    def test_receiver_accepts_both_http_body_framings(self):
        for chunked in (False, True):
            with self.subTest(chunked=chunked):
                server = fixture.FixtureServer()
                server.case = fixture.CASES["chat.max.qwen3"]
                thread = threading.Thread(
                    target=server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True
                )
                thread.start()
                client = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=3)
                body = b'{ "model":"qwen3", "reasoning_effort":"max", "flag":false, "zero":0 }'
                try:
                    with mock.patch.object(server, "handle_error"):
                        try:
                            client.request(
                                "POST", "/channel-1/v1/chat/completions",
                                body=iter((body[:13], body[13:])) if chunked else body,
                                headers={"Content-Type": "application/json"},
                                encode_chunked=chunked,
                            )
                            response = client.getresponse()
                            status = response.status
                            response.read()
                        except (ConnectionError, http.client.RemoteDisconnected):
                            status = None
                        self.assertEqual(
                            status, 200,
                            "synthetic receiver must decode chunked bodies before JSON parsing",
                        )
                    self.assertEqual(len(server.calls), 1)
                    self.assertEqual(server.calls[0]["body"], json.loads(body))
                    self.assertEqual(server.calls[0]["raw_body"].encode(), body)
                finally:
                    client.close()
                    server.shutdown()
                    server.server_close()
                    thread.join(timeout=3)
                self.assertFalse(thread.is_alive())

    def test_namespace_guard_rejects_real_interfaces(self):
        with mock.patch.object(fixture.socket, "if_nameindex", return_value=[(1, "lo"), (2, "eth0")]):
            with self.assertRaisesRegex(RuntimeError, "isolated network namespace required"):
                fixture.guard_namespace()

    def test_namespace_guard_rejects_host_namespace(self):
        with mock.patch.object(fixture.socket, "if_nameindex", return_value=[(1, "lo")]), \
                mock.patch.object(fixture.os, "readlink", return_value="net:[123]"):
            with self.assertRaisesRegex(RuntimeError, "isolated network namespace required"):
                fixture.guard_namespace()

    def test_manifest_has_unique_identifiers(self):
        cases = fixture.build_cases()
        self.assertEqual(set(cases), {case["case_id"] for case in cases.values()})
        for case in cases.values():
            self.assertEqual(len(case["expected"]), len(case["routes"]))


class MainEntryTests(unittest.TestCase):
    def setUp(self):
        directory = self.enterContext(tempfile.TemporaryDirectory(prefix="effort-main-test-"))
        self.root = pathlib.Path(directory)
        self.enterContext(mock.patch.dict(os.environ, {"TMPDIR": directory}))
        self.enterContext(mock.patch.object(fixture.urllib.request, "_opener"))
        fixture.guard_namespace()  # Keep bootstrap controls in a real isolated namespace.
        self.args = [str(pathlib.Path(__file__)), "--cases", "chat.max.qwen3"]

    def invoke(self, args=None):
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch.object(sys, "argv", ["test-auto-effort-routing.py", *(self.args if args is None else args)]), \
                contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            try:
                code = fixture.main()
            except Exception as error:
                self.fail(f"main leaked {type(error).__name__}: {error}; infrastructure must return 2")
        return code, stdout.getvalue(), stderr.getvalue()

    def result(self, effort="xhigh"):
        # Use the real wire evaluator, not a pre-labelled FAIL result. Only the
        # external gateway run is replaced in these entrypoint unit controls.
        case = fixture.CASES["chat.max.qwen3"]
        body = copy.deepcopy(case["payload"])
        body.update(model=case["expected"][0]["model"], reasoning_effort=effort)
        raw = json.dumps(body)
        calls = [{"model": body["model"], "path": case["expected"][0]["path"],
                  "body": body, "raw_body": raw, "upstream_status": 200,
                  "effort": fixture.field_state(body, "reasoning_effort")}]
        response = {"status": 200, "body": "fixture-ok"}
        logs = [{"channel_id": 1, "other": {"reasoning_effort": effort}}]
        failures = fixture.check_case(case, calls, response, logs, raw)
        return {"id": "true:chat.max.qwen3", "calls": calls, "failures": failures,
                "status": "FAIL" if failures else "PASS",
                "cleanup": [{"pid": index, "reaped": True, "log_closed": True} for index in (1, 2)]}

    def saved_report(self):
        reports = list(self.root.glob("auto-reasoning-effort-*/results.json"))
        self.assertEqual(len(reports), 1)
        return json.loads(reports[0].read_text())

    def assert_infrastructure(self, result, detail):
        code, _, stderr = result
        self.assertEqual(code, 2)
        self.assertIn("infrastructure error", stderr.lower())
        self.assertIn(detail, stderr)
        self.assertNotIn("Traceback", stderr)

    def test_main_namespace_failure_is_infrastructure(self):
        with mock.patch.object(fixture.socket, "if_nameindex", return_value=[(1, "lo"), (2, "eth0")]), \
                mock.patch.object(fixture, "FixtureServer") as server:
            self.assert_infrastructure(self.invoke(), "isolated network namespace required")
        server.assert_not_called()
        self.assertEqual(list(self.root.iterdir()), [])

    def test_main_missing_binary_is_infrastructure(self):
        with mock.patch.object(fixture, "FixtureServer") as server:
            result = self.invoke([str(self.root / "missing-binary")])
        self.assert_infrastructure(result, "FileNotFoundError")
        server.assert_not_called()
        self.assertEqual(list(self.root.iterdir()), [])

    def test_main_incomplete_evidence_is_infrastructure(self):
        row = self.result()
        row["id"] = "true:wrong-case"
        with mock.patch.object(fixture, "run_case", return_value=row):
            self.assert_infrastructure(self.invoke(), "incomplete/duplicate results")
        saved = self.saved_report()
        self.assertTrue(saved["stub_stopped"])
        self.assertEqual(saved["results"][0]["id"], "true:wrong-case")

    def test_main_wire_mismatch_remains_exit_one(self):
        row = self.result(effort="max")
        self.assertEqual(len(row["failures"]), 1)
        self.assertIn("wire effort", row["failures"][0])
        with mock.patch.object(fixture, "run_case", return_value=row):
            code, _, stderr = self.invoke()
        self.assertEqual(code, 1)
        self.assertEqual(stderr, "")
        saved = self.saved_report()
        self.assertEqual(saved["summary"]["FAIL"], 1)
        self.assertEqual(saved["summary"]["ERROR"], 0)
        self.assertTrue(saved["stub_stopped"])

    def test_main_valid_wire_control_returns_zero(self):
        row = self.result()
        self.assertEqual(row["failures"], [])
        with mock.patch.object(fixture, "run_case", return_value=row):
            code, _, stderr = self.invoke()
        self.assertEqual((code, stderr), (0, ""))
        self.assertEqual(self.saved_report()["summary"]["PASS"], 1)

    def assert_cleanup_failure(self, cleanup):
        row = self.result()
        row["cleanup"] = cleanup
        with mock.patch.object(fixture, "run_case", return_value=row):
            code, _, _ = self.invoke()
        self.assertEqual(code, 2, "gateway cleanup must be checked before PASS")
        self.assertEqual(self.saved_report()["summary"]["ERROR"], 1)

    def test_main_unreaped_gateway_is_infrastructure(self):
        cleanup = self.result()["cleanup"]
        cleanup[-1]["reaped"] = False
        self.assert_cleanup_failure(cleanup)

    def test_main_unclosed_gateway_log_is_infrastructure(self):
        cleanup = self.result()["cleanup"]
        cleanup[-1]["log_closed"] = False
        self.assert_cleanup_failure(cleanup)

    def test_main_missing_gateway_cleanup_is_infrastructure(self):
        self.assert_cleanup_failure([])

    def test_main_argparse_exit_semantics_are_not_swallowed(self):
        for args, code in ((["--help"], 0), ([], 2), (["--unknown"], 2)):
            with self.subTest(args=args), self.assertRaises(SystemExit) as raised:
                self.invoke(args)
            self.assertEqual(raised.exception.code, code)

    def test_main_keyboard_interrupt_is_not_infrastructure(self):
        with mock.patch.object(fixture, "guard_namespace", side_effect=KeyboardInterrupt), \
                self.assertRaises(KeyboardInterrupt):
            self.invoke()


if __name__ == "__main__":
    unittest.main()
