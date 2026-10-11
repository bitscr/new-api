"""Negative controls for the policy harness's evidence validator."""
import contextlib
import importlib.util
import io
import json
import os
import pathlib
import sqlite3
import sys
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "policy_fixture", pathlib.Path(__file__).with_name("test-auto-routing-policy.py"))
assert spec and spec.loader
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


def request_stub(server, path, model):
    request = urllib.request.Request(
        f"http://127.0.0.1:{server.server_port}" + path,
        data=json.dumps({"model": model}).encode(),
        headers={"Content-Type": "application/json"})
    try:
        response = urllib.request.urlopen(request, timeout=3)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return {"status": response.status, "body": response.read().decode()}


class PolicyEvidenceTests(unittest.TestCase):
    def run_fixture(self, name="generic503", *, wrong_model=False, lost_after_restart=False, bind_error=False,
                    fallback_model=None, fallback_path=None, followup_path=None):
        case = next(item for item in fixture.cases() if item["name"] == name)
        bad_models = [case["first_model"]] if "first_model" in case else [f"probe-bad-{i:02}" for i in range(case.get("model_count", 3))]
        server = fixture.Server(case, bad_models)
        failed_socket = mock.Mock()
        failed_socket.bind.side_effect = OSError("synthetic bind failure")
        failed_socket.fileno.return_value = -1
        row = {"group": "default", "model": "unrelated-model" if wrong_model else bad_models[0],
               "channel_id": 1, "level": 1, "reason": "synthetic failure"}

        @contextlib.contextmanager
        def gateway(binary, root, port, cache, cleanup, extra_env=None):
            cleanup.append({"reaped": True, "log_closed": True})
            yield

        def ask(root, port, token, requested, index):
            routes = ([(1, bad_models[0])] if not case.get("refuse") else []) + [
                (2, fallback_model or case.get("good_model", "probe-ok"))]
            if index != 1:
                routes = [(1, bad_models[1])]
            result = None
            for channel, model in routes:
                path = f"/channel-{channel}/v1/chat/completions"
                if index == 1 and channel == 2 and fallback_path is not None:
                    path = fallback_path
                if index != 1 and followup_path is not None:
                    path = followup_path
                result = request_stub(server, path, model)
            assert result is not None
            return result

        with tempfile.TemporaryDirectory(prefix="policy-evidence-") as directory, \
                mock.patch.object(fixture, "Server", return_value=server), \
                (mock.patch.object(fixture.socket, "socket", return_value=failed_socket) if bind_error else contextlib.nullcontext()), \
                mock.patch.object(fixture, "gateway", gateway), \
                mock.patch.object(fixture, "seed"), \
                mock.patch.object(fixture, "ask", ask), \
                mock.patch.object(fixture.h, "usage_error_rows", return_value=[]), \
                mock.patch.object(fixture.time, "sleep"), \
                mock.patch.object(fixture.h, "rows", side_effect=[[row], [], []] if lost_after_restart else None,
                                  return_value=[row]) as rows:
            result = fixture.run_case(pathlib.Path("unused-binary"), pathlib.Path(directory) / "case", case, "false")
        return result, rows.call_count

    def test_matching_observed_pair_passes(self):
        result, _ = self.run_fixture()
        self.assertEqual(result["failures"], [])
        self.assertEqual(result["fixture_errors"], [])

    def test_undeclared_fallback_model_is_behavior_failure(self):
        result, _ = self.run_fixture(fallback_model="probe-typo")
        self.assertEqual(result["fixture_errors"], [])
        self.assertTrue(result["failures"], "undeclared fallback route was accepted: " + repr(result))
        self.assertTrue(any("undeclared upstream route/path" in text for text in result["failures"]))
        self.assertNotEqual(result["responses"][0]["status"], 200)

    def test_initial_wrong_path_is_behavior_failure(self):
        result, _ = self.run_fixture(fallback_path="/channel-2/v1/typo")
        self.assertEqual(result["fixture_errors"], [])
        self.assertTrue(any("undeclared upstream route/path" in text for text in result["failures"]),
                        "initial request path was not validated")

    def test_followup_wrong_path_is_behavior_failure(self):
        for name in ("request-only-skip", "restart-pair-only"):
            with self.subTest(case=name):
                result, _ = self.run_fixture(name, followup_path="/channel-1/v1/typo")
                self.assertEqual(result["fixture_errors"], [])
                self.assertTrue(any("undeclared upstream route/path" in text for text in result["failures"]),
                                "follow-up request path was not validated")

    def test_followup_channel_marker_model_disagreement_is_rejected(self):
        result, _ = self.run_fixture("request-only-skip", followup_path="/channel-2/v1/chat/completions")
        self.assertEqual(result["fixture_errors"], [])
        self.assertTrue(any("undeclared upstream route/path" in text for text in result["failures"]),
                        "follow-up channel was inferred from the model")

    def test_main_oracle_mismatch_is_behavior_exit_one(self):
        result, _ = self.run_fixture(fallback_model="probe-typo")
        with tempfile.TemporaryDirectory(prefix="policy-main-") as directory, \
                mock.patch.dict(os.environ, {"TMPDIR": directory}), \
                mock.patch.object(sys, "argv", ["policy-fixture", __file__, "--case", "generic503"]), \
                mock.patch.object(fixture, "run_case", return_value=result), \
                contextlib.redirect_stdout(io.StringIO()) as stdout:
            self.assertEqual(fixture.main(), 1)
            report = json.loads(stdout.getvalue().splitlines()[-1])
            self.assertEqual(report["behavior_failures"], 1)
            self.assertEqual(report["fixture_errors"], 0)

    def test_wrong_model_cooldown_is_rejected(self):
        result, _ = self.run_fixture(wrong_model=True)
        self.assertTrue(result["failures"], "must reject a cooldown attributed to an unobserved model")

    def test_restart_loss_is_rejected(self):
        result, _ = self.run_fixture("restart-pair-only", lost_after_restart=True)
        self.assertTrue(result["failures"], "must reread and validate persistence after restart")

    def test_restart_retained_pair_passes(self):
        result, _ = self.run_fixture("restart-pair-only")
        self.assertEqual(result["failures"], [])
        self.assertEqual(result["fixture_errors"], [])

    def test_refusal_requires_gateway_transport_evidence(self):
        result, _ = self.run_fixture("connection-refused")
        self.assertTrue(result["failures"] or result["fixture_errors"],
                        "a successful fallback alone must not prove connection refusal")

    def test_refusal_bind_failure_is_reported_and_cleaned(self):
        result, _ = self.run_fixture("connection-refused", bind_error=True)
        self.assertTrue(result["fixture_errors"])
        self.assertIn("synthetic bind failure", result["fixture_errors"][0])

    def test_main_missing_binary_is_infrastructure(self):
        with tempfile.TemporaryDirectory(prefix="policy-entry-") as directory, \
                mock.patch.dict(os.environ, {"TMPDIR": directory}), \
                mock.patch.object(sys, "argv", ["policy-fixture", str(pathlib.Path(directory) / "missing")]), \
                contextlib.redirect_stderr(io.StringIO()) as stderr:
            try:
                code = fixture.main()
            except Exception as error:
                self.fail("policy main must report infrastructure failure: " + str(error))
        self.assertEqual(code, 2)
        self.assertIn("Infrastructure error", stderr.getvalue())


class StubRouteTests(unittest.TestCase):
    def setUp(self):
        case = next(item for item in fixture.cases() if item["name"] == "generic503")
        self.server = fixture.Server(case, ["probe-bad-00", "probe-bad-01"])
        self.worker = threading.Thread(target=self.server.serve_forever,
                                       kwargs={"poll_interval": 0.05}, daemon=True)
        self.worker.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.worker.join(timeout=3)
        self.assertFalse(self.worker.is_alive())
        self.assertEqual(self.server.fileno(), -1)
        self.assertEqual(self.server.fixture_errors, [])

    def test_channel_marker_model_disagreement_cannot_relabel_channel(self):
        for channel, model in ((1, "probe-ok"), (2, "probe-bad-00")):
            with self.subTest(channel=channel, model=model):
                response = request_stub(self.server, f"/channel-{channel}/v1/chat/completions", model)
                self.assertEqual(self.server.calls[-1]["channel"], channel)
                self.assertNotEqual(response["status"], 200)

    def test_declared_routes_preserve_error_and_healthy_responses(self):
        failed = request_stub(self.server, "/channel-1/v1/chat/completions", "probe-bad-00")
        self.assertEqual(failed["status"], 503)
        self.assertEqual(json.loads(failed["body"])["error"]["code"], "server_error")
        healthy = request_stub(self.server, "/channel-2/v1/chat/completions", "probe-ok")
        self.assertEqual(healthy["status"], 200)
        self.assertIn("fixture-ok:probe-ok", healthy["body"])

    def test_recovered_channel_still_serves_only_declared_models(self):
        with self.server.lock:
            self.server.recovered = True
        healthy = request_stub(self.server, "/channel-1/v1/chat/completions", "probe-bad-01")
        self.assertEqual(healthy["status"], 200)
        invalid = request_stub(self.server, "/channel-1/v1/chat/completions", "probe-typo")
        self.assertEqual(invalid["status"], 400)

    def test_custom_good_model_is_not_hardcoded_to_probe_ok(self):
        with self.server.lock:
            self.server.spec["good_model"] = "probe-custom-ok"
        healthy = request_stub(self.server, "/channel-2/v1/chat/completions", "probe-custom-ok")
        self.assertEqual(healthy["status"], 200)
        self.assertIn("fixture-ok:probe-custom-ok", healthy["body"])
        invalid = request_stub(self.server, "/channel-2/v1/chat/completions", "probe-ok")
        self.assertEqual(invalid["status"], 400)

    def test_shared_model_uses_endpoint_identity(self):
        with self.server.lock:
            self.server.bad_models.add("probe-ok")
        for channel, status in ((1, 503), (2, 200)):
            with self.subTest(channel=channel):
                response = request_stub(self.server, f"/channel-{channel}/v1/chat/completions", "probe-ok")
                self.assertEqual(self.server.calls[-1]["channel"], channel)
                self.assertEqual(response["status"], status)

    def test_unknown_routes_never_get_a_healthy_reply(self):
        routes = [
            ("/channel-2/v1/chat/completions", "probe-typo"),
            ("/channel-1/v1/chat/completions", "probe-typo"),
            ("/channel-9/v1/chat/completions", "probe-ok"),
            ("/v1/chat/completions", "probe-ok"),
            ("/channel-2/v1/typo", "probe-ok"),
            ("/channel-2/v1/chat/completions?unexpected=1", "probe-ok"),
        ]
        for path, model in routes:
            with self.subTest(path=path, model=model):
                response = request_stub(self.server, path, model)
                self.assertNotEqual(response["status"], 200)


class SeedRouteTests(unittest.TestCase):
    def seeded_channels(self, refuse=False):
        case = next(item for item in fixture.cases() if item["name"] == "generic503")
        case.update(good_model="probe-custom-ok", refuse=refuse)
        upstream = "http://127.0.0.1:43000"
        with tempfile.TemporaryDirectory(prefix="policy-seed-") as directory:
            root = pathlib.Path(directory)
            with contextlib.closing(sqlite3.connect(root / "one-api.db")) as db, db:
                db.executescript('''
                    CREATE TABLE channels (id INTEGER PRIMARY KEY, models TEXT, base_url TEXT);
                    CREATE TABLE abilities ("group" TEXT, model TEXT, channel_id INTEGER,
                                            enabled INTEGER, priority INTEGER, weight INTEGER, tag TEXT);
                    CREATE TABLE options (key TEXT PRIMARY KEY, value TEXT);
                ''')
                db.executemany("INSERT INTO channels VALUES (?, '', ?)", [(1, upstream), (2, upstream)])
                db.executemany("INSERT INTO options VALUES (?, '')",
                               [(key,) for key in ("ModelRatio", "RetryTimes", "AutomaticRetryStatusCodes")])
            with mock.patch.object(fixture.h, "seed"):
                fixture.seed(root, "fixture-token", upstream, case, ["probe-bad-00"], refusal_port=43001)
            with contextlib.closing(sqlite3.connect(root / "one-api.db")) as db:
                return db.execute("SELECT id,base_url,models FROM channels ORDER BY id").fetchall()

    def test_seed_closes_its_database(self):
        class TrackedConnection(sqlite3.Connection):
            closed = False

            def close(self):
                self.closed = True
                super().close()

        original_connect, opened = sqlite3.connect, []

        def connect(*args, **kwargs):
            connection = original_connect(*args, **kwargs, factory=TrackedConnection)
            opened.append(connection)
            return connection

        try:
            with mock.patch.object(fixture.sqlite3, "connect", side_effect=connect):
                self.seeded_channels()
            self.assertTrue(opened)
            self.assertTrue(all(connection.closed for connection in opened),
                            "seeding must close SQLite handles, not only commit the transaction")
        finally:
            for connection in opened:
                connection.close()

    def test_normal_channels_have_independent_endpoint_prefixes(self):
        self.assertEqual(self.seeded_channels(), [
            (1, "http://127.0.0.1:43000/channel-1", "probe-bad-00"),
            (2, "http://127.0.0.1:43000/channel-2", "probe-custom-ok"),
        ])

    def test_refusal_endpoint_is_not_replaced_by_listening_stub(self):
        self.assertEqual(self.seeded_channels(refuse=True), [
            (1, "http://127.0.0.1:43001", "probe-bad-00"),
            (2, "http://127.0.0.1:43000/channel-2", "probe-custom-ok"),
        ])


class CaseManifestTests(unittest.TestCase):
    def test_reviewed_classifier_negative_controls_are_literal(self):
        expected = [
            {"name": "model-only-account-qualifier", "status": 429, "code": "model_rate_limit_exceeded",
             "message": "Your account rate limit is exceeded only for model 'gpt-4o'; other models remain available.",
             "channels": [1, 1, 1, 2], "client_status": 200, "codes": "429"},
            {"name": "negated-model-compat", "status": 400, "code": "invalid_request",
             "message": "No model is unsupported. The request JSON is malformed.",
             "channels": [1], "client_status": 400, "codes": "429"},
            {"name": "negated-model-overload", "status": 503, "code": "upstream_error",
             "message": "No model is overloaded; upstream maintenance is in progress.",
             "channels": [1, 2], "client_status": 200, "codes": "503"},
        ]
        manifest = {case["name"]: case for case in fixture.cases()}
        for wanted in expected:
            with self.subTest(case=wanted["name"]):
                self.assertIn(wanted["name"], manifest)
                self.assertEqual({key: manifest[wanted["name"]].get(key) for key in wanted}, wanted)


if __name__ == "__main__":
    unittest.main()
