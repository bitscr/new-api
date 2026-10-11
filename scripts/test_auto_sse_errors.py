"""Safety tests for the real-binary SSE fixture (no gateways are started)."""

import importlib.util
import io
import json
import pathlib
import tempfile
import threading
import types
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "auto_sse_errors", pathlib.Path(__file__).with_name("test-auto-sse-errors.py")
)
assert spec is not None and spec.loader is not None
harness = importlib.util.module_from_spec(spec)
spec.loader.exec_module(harness)


class StartIsolationTests(unittest.TestCase):
    def setUp(self):
        harness.fixture_namespace.require_namespace()
        self.interfaces = self.enterContext(mock.patch.object(
            harness.socket, "if_nameindex", return_value=harness.socket.if_nameindex()))
        # Mock gateway spawning, not the independent ip inspection subprocesses.
        self.popen = mock.Mock()
        self.enterContext(mock.patch.object(harness, "subprocess", types.SimpleNamespace(
            Popen=self.popen, STDOUT=harness.subprocess.STDOUT,
            TimeoutExpired=harness.subprocess.TimeoutExpired)))
        self.popen.return_value.poll.return_value = None
        self.open_log = self.enterContext(mock.patch.object(pathlib.Path, "open", mock.mock_open()))
        self.urlopen = self.enterContext(mock.patch.object(harness.urllib.request, "urlopen"))
        self.urlopen.return_value.__enter__.return_value = io.BytesIO(
            b'{"success":true,"data":{"version":"fixture-version"}}'
        )
        self.start_args = (pathlib.Path("/fixture/new-api"), pathlib.Path("/fixture"), 13001, "false")

    def assert_rejected(self, interfaces):
        self.interfaces.return_value = interfaces
        with self.assertRaisesRegex(RuntimeError, "isolated network namespace.*unshare --net"):
            harness.start(*self.start_args)
        self.popen.assert_not_called()
        self.open_log.assert_not_called()

    def test_start_rejects_exposed_interfaces(self):
        self.assert_rejected([(1, "lo"), (2, "eth0")])

    def test_start_rejects_empty_interfaces(self):
        self.assert_rejected([])

    def test_start_enables_error_logging(self):
        harness.start(*self.start_args)
        self.assertEqual(
            self.popen.call_args.kwargs["env"].get("ERROR_LOG_ENABLED"), "true",
            "the real-binary fixture must exercise persisted error logs",
        )

    def test_start_allows_loopback_only(self):
        process, log, version = harness.start(*self.start_args)
        self.popen.assert_called_once()
        self.open_log.assert_called_once_with("wb")
        self.assertIs(process, self.popen.return_value)
        self.assertIs(log, self.open_log.return_value)
        self.assertEqual(version, "fixture-version")


class StubShapeTests(unittest.TestCase):
    def reply(self, case):
        server = harness.FixtureServer(("127.0.0.1", 0))
        server.case = case
        thread = threading.Thread(target=server.serve_forever,
                                  kwargs={"poll_interval": 0.01}, daemon=True)
        thread.start()
        try:
            request = harness.urllib.request.Request(
                f"http://127.0.0.1:{server.server_port}/v1/chat/completions",
                data=json.dumps({"model": "probe-target"}).encode(),
                headers={"Content-Type": "application/json"},
            )
            try:
                response = harness.urllib.request.urlopen(request, timeout=2)
            except harness.urllib.error.HTTPError as error:
                response = error
            with response:
                return response.status, response.read()
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)
            self.assertFalse(thread.is_alive())

    def test_ordinary_upstream_error_is_real_http400(self):
        status, body = self.reply("http_error")
        self.assertEqual(status, 400)
        self.assertEqual(json.loads(body)["error"]["code"], "fixture_rejected")
        self.assertIn("http_error", harness.CASES)
        self.assertIn("http_error", harness.BAD_CASES)
        self.assertNotIn("http_error", harness.ERROR_CASES)


    def test_unknown_error_metadata_is_a_success_control(self):
        status, body = self.reply("unknown_error")
        self.assertEqual(status, 200)
        frame = json.loads(body.split(b"data: ", 1)[1].split(b"\n", 1)[0])
        self.assertEqual(frame.get("error"), {"metadata": "informational fixture"})
        self.assertTrue(frame["choices"][0]["delta"]["content"])
        self.assertIn("unknown_error", harness.CASES)
        self.assertNotIn("unknown_error", harness.BAD_CASES)


    def test_escaped_error_key_remains_valid_json(self):
        status, body = self.reply("escaped_error")
        self.assertEqual(status, 200)
        self.assertIn(br'"\u0065rror":', body)
        frame = json.loads(body.split(b"data: ", 1)[1].split(b"\n", 1)[0])
        self.assertEqual(frame, harness.ERROR)
        self.assertIn("escaped_error", harness.ERROR_CASES)
        self.assertIn("escaped_error", harness.CASES)


class UsageLogEvidenceTests(unittest.TestCase):
    def run_fixture(self, persisted, views):
        with tempfile.TemporaryDirectory(prefix="usage-log-harness-test-") as directory:
            stub = types.SimpleNamespace(server_port=13002, case="", calls=[],
                                         state_lock=threading.Lock())

            def ask(root, port, token, index):
                model = "probe-target" if index == 1 else "probe-ok"
                stub.calls.append({"model": model})
                payload = harness.ERROR if index == 1 else harness.fixture_chunk("normal", model)
                return {"status": 200, "body": "data: " + json.dumps(payload) + "\n\ndata: [DONE]\n\n",
                        "request_id": f"fixture-request-{index}"}

            cooldown = [{"group": "default", "model": "probe-target", "channel_id": 1, "level": 1}]
            with mock.patch.object(harness, "start", return_value=(object(), object(), "fixture")), \
                 mock.patch.object(harness, "stop"), \
                 mock.patch.object(harness, "smoke_assets", return_value={}), \
                 mock.patch.object(harness, "seed"), \
                 mock.patch.object(harness, "ask", side_effect=ask), \
                 mock.patch.object(harness, "rows", return_value=cooldown), \
                 mock.patch.object(harness, "usage_error_rows", return_value=persisted, create=True), \
                 mock.patch.object(harness, "usage_log_views", return_value=views, create=True):
                return harness.run_case(pathlib.Path("/fixture/binary"), pathlib.Path(directory),
                                        stub, "false", "error_envelope", None)

    def test_correct_cooldown_without_usage_error_log_fails(self):
        views = {"root": {"total": 0, "items": []}, "missing": {"total": 0, "items": []},
                 "token": []}
        result = self.run_fixture([], views)
        self.assertTrue(any("error log" in text for text in result["failures"]),
                        "a cooldown alone must not pass usage-log acceptance")

    def test_persisted_error_and_filtered_private_views_pass(self):
        other = {"error_code": "bad_response", "error_type": "new_api_error", "status_code": 200}
        row = {"type": 5, "user_id": 1, "token_id": 1, "token_name": "fixture",
               "model_name": "probe-target", "channel": 1, "group": "default", "is_stream": True,
               "quota": 0, "prompt_tokens": 0, "completion_tokens": 0,
               "request_id": "fixture-request-1", "content": "diagnostic text",
               "other": json.dumps(other)}
        private = dict(row, content="bad_response")
        views = {"root": {"total": 1, "items": [row]},
                 "missing": {"total": 0, "items": []}, "token": [private]}
        result = self.run_fixture([row], views)
        self.assertEqual(result["failures"], [])


if __name__ == "__main__":
    unittest.main()
