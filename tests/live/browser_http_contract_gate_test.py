import copy
import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "browser-http-contract-gate.py"
FIXTURE = ROOT / "tests" / "fixtures" / "browser-http-contract.json"
IMAGE = "ghcr.io/redf0x1/camofox-browser@sha256:" + "a" * 64


class BrowserHTTPContractGateTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location("browser_http_contract_gate", SCRIPT)
        self.gate = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.gate)
        self.fixture = json.loads(FIXTURE.read_text())
        self.calls = []
        self.tabs = []
        self.created = 0

    def requester(self, endpoint, key, method, path, payload, timeout, max_bytes):
        self.calls.append((method, path, payload, timeout, max_bytes))
        if path == "/health":
            return 200, {"ok": True, "running": True, "engine": "camoufox", "version": "2.4.8"}
        if method == "GET" and path.startswith("/tabs?"):
            return 200, {"running": True, "tabs": [{"tabId": tab} for tab in self.tabs]}
        if method == "POST" and path == "/tabs":
            self.assertNotIn("url", payload, "blank creation must omit about:blank")
            self.created += 1
            tab = f"contract-tab-{self.created}"
            self.tabs.append(tab)
            return 200, {"tabId": tab, "url": "about:blank"}
        if path.endswith("/navigate"):
            self.assertEqual(payload["url"], "https://example.com/")
            return 200, {"ok": True, "url": payload["url"]}
        if path.endswith("/wait"):
            return 200, {"ok": True, "ready": True}
        if path.endswith("/evaluate"):
            result = {"url": "https://example.com/", "title": "Example Domain", "text": "This domain is for use in documentation examples without needing permission. Avoid use in operations."}
            if "JSON.stringify" in payload["expression"]:
                result = json.dumps(result)
            return 200, {"result": result, "truncated": False}
        if method == "DELETE" and path.startswith("/tabs/"):
            self.tabs.remove(path.rsplit("/", 1)[1])
            return 200, {"ok": True}
        if method == "DELETE" and path.startswith("/sessions/"):
            self.tabs.clear()
            return 200, {"ok": True}
        self.fail(f"unexpected request {method} {path}")

    def run_gate(self, requester=None, **kwargs):
        image_reference = kwargs.pop("image_reference", IMAGE)
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "contract.json"
            result = self.gate.run_gate(
                endpoint="http://browser.invalid:9377", api_key="must-not-record-key",
                expected_version="2.4.8", image_reference=image_reference,
                output=output, fixture=self.fixture,
                requester=requester or self.requester, repo_revision="test-revision", **kwargs,
            )
            self.assertEqual(json.loads(output.read_text()), result)
            return result

    def test_actual_shapes_consecutive_reuse_cleanup_and_safe_manifest(self):
        artifact = self.run_gate()
        self.assertTrue(artifact["passed"], artifact["reasons"])
        self.assertEqual(self.created, 2)
        self.assertEqual(self.tabs, [])
        self.assertTrue(artifact["cleanup"]["passed"])
        users = {payload["userId"] for _, _, payload, _, _ in self.calls if payload and "userId" in payload}
        self.assertEqual(len(users), 1)
        self.assertRegex(users.pop(), r"^crw-contract-[a-f0-9]{32}$")
        self.assertEqual({check["encoding"] for check in artifact["evaluations"]}, {"object", "string"})
        self.assertTrue(all(0 < call[3] <= 15 for call in self.calls))
        self.assertTrue(all(call[4] == 262144 for call in self.calls))
        stored = json.dumps(artifact)
        self.assertNotIn("must-not-record-key", stored)
        self.assertNotIn("documentation examples without needing permission", stored)
        self.assertEqual(artifact["runtime_manifest"]["image_reference"], IMAGE)
        self.assertEqual(artifact["runtime_manifest"]["repo_revision"], "test-revision")

    def test_current_example_domain_body_does_not_need_the_title_as_a_heading(self):
        def request(*args):
            status, reply = self.requester(*args)
            if args[3].endswith("/evaluate"):
                encoded = isinstance(reply["result"], str)
                snapshot = json.loads(reply["result"]) if encoded else reply["result"]
                snapshot["title"] = "Example Domain"
                snapshot["text"] = "This domain is for use in documentation examples without needing permission. Avoid use in operations."
                reply["result"] = json.dumps(snapshot) if encoded else snapshot
            return status, reply
        artifact = self.run_gate(request)
        self.assertTrue(artifact["passed"], artifact["reasons"])
        self.assertEqual(len(artifact["evaluations"]), 4, "both encodings must pass in both cycles")
        self.assertTrue(artifact["cleanup"]["passed"])

    def test_wrong_title_is_rejected_even_with_both_old_and_current_body_markers(self):
        for title in ["Unexpected Page", "example domain", "Example Domain - challenge"]:
            with self.subTest(title=title):
                def request(*args):
                    status, reply = self.requester(*args)
                    if args[3].endswith("/evaluate"):
                        encoded = isinstance(reply["result"], str)
                        snapshot = json.loads(reply["result"]) if encoded else reply["result"]
                        snapshot["title"] = title
                        snapshot["text"] = "Example Domain: This domain is for use in documentation examples without needing permission."
                        reply["result"] = json.dumps(snapshot) if encoded else snapshot
                    return status, reply
                artifact = self.run_gate(request)
                self.assertFalse(artifact["passed"], "body markers alone must not validate a different page title")
                self.assertIn("evaluation-title", " ".join(artifact["reasons"]))
                self.assertTrue(artifact["cleanup"]["passed"])

    def test_each_run_uses_a_new_profile(self):
        first = self.run_gate()
        second = self.run_gate()
        self.assertNotEqual(first["scope_id"], second["scope_id"])

    def test_wrong_pinned_version_prevents_creation(self):
        def request(*args):
            status, reply = self.requester(*args)
            if args[3] == "/health":
                reply["version"] = "2.4.9"
            return status, reply
        artifact = self.run_gate(request)
        self.assertFalse(artifact["passed"])
        self.assertIn("health.version", " ".join(artifact["reasons"]))
        self.assertEqual(self.created, 0)

    def test_unknown_health_engine_and_old_version_fail(self):
        for field, value in [("engine", "chromium"), ("version", "2.4.6")]:
            def request(*args):
                status, reply = self.requester(*args)
                if args[3] == "/health":
                    reply[field] = value
                return status, reply
            self.assertFalse(self.run_gate(request)["passed"])

    def test_evaluation_failure_is_truthful_and_always_cleans_up(self):
        for change in [{"truncated": True}, {"result": "not JSON"}, {"result": {"text": "Blocked", "url": "https://example.com/"}}, {"ok": False}]:
            def request(*args):
                status, reply = self.requester(*args)
                if args[3].endswith("/evaluate"):
                    reply.update(change)
                return status, reply
            artifact = self.run_gate(request)
            self.assertFalse(artifact["passed"])
            self.assertTrue(artifact["cleanup"]["passed"])
            self.assertEqual(self.tabs, [])

    def test_blank_create_5xx_resets_only_owned_profile_and_retries_once(self):
        attempts = 0
        def request(*args):
            nonlocal attempts
            if args[2:4] == ("POST", "/tabs"):
                attempts += 1
                if attempts == 1:
                    return 500, {"error": "secret upstream details"}
            return self.requester(*args)
        artifact = self.run_gate(request)
        self.assertTrue(artifact["passed"], artifact["reasons"])
        self.assertEqual(attempts, 3)
        self.assertEqual(artifact["create_recoveries"], 1)
        resets = [path for method, path, _, _, _ in self.calls if method == "DELETE" and path.startswith("/sessions/")]
        self.assertTrue(all(path == "/sessions/" + artifact["scope_id"] for path in resets))
        self.assertNotIn("secret upstream details", json.dumps(artifact))

    def test_repeated_5xx_or_4xx_does_not_loop(self):
        for status, expected_attempts in [(503, 2), (400, 1)]:
            attempts = 0
            def request(*args):
                nonlocal attempts
                if args[2:4] == ("POST", "/tabs"):
                    attempts += 1
                    return status, {"error": "not saved"}
                return self.requester(*args)
            artifact = self.run_gate(request)
            self.assertFalse(artifact["passed"])
            self.assertEqual(attempts, expected_attempts)

    def test_transport_timeout_does_not_retry_ambiguous_create(self):
        attempts = 0
        def request(*args):
            nonlocal attempts
            if args[2:4] == ("POST", "/tabs"):
                attempts += 1
                raise TimeoutError("secret transport URL")
            return self.requester(*args)
        artifact = self.run_gate(request)
        self.assertFalse(artifact["passed"])
        self.assertEqual(attempts, 1)
        self.assertTrue(artifact["cleanup"]["passed"])
        self.assertNotIn("secret transport URL", json.dumps(artifact))

    def test_cleanup_failure_fails_gate_even_after_good_content(self):
        def request(*args):
            if args[2] == "DELETE" and args[3].startswith("/sessions/"):
                return 200, {}
            return self.requester(*args)
        artifact = self.run_gate(request)
        self.assertFalse(artifact["passed"])
        self.assertFalse(artifact["cleanup"]["passed"])

    def test_foreign_tab_list_or_unsafe_tab_id_fails_without_unscoped_delete(self):
        for mode in ["foreign", "unsafe"]:
            def request(*args):
                status, reply = self.requester(*args)
                if mode == "foreign" and args[2] == "GET" and args[3].startswith("/tabs?") and self.created:
                    reply["tabs"].append({"tabId": "foreign-tab"})
                if mode == "unsafe" and args[2:4] == ("POST", "/tabs"):
                    reply["tabId"] = "../../foreign/session"
                return status, reply
            artifact = self.run_gate(request)
            self.assertFalse(artifact["passed"])
            self.assertTrue(all("foreign" not in path for _, path, _, _, _ in self.calls))

    def test_invalid_fixture_and_unpinned_manifest_fail_before_network(self):
        for field, value in [("schemaVersion", 2), ("cycles", 101)]:
            fixture = copy.deepcopy(self.fixture)
            fixture[field] = value
            with self.assertRaises(ValueError):
                with tempfile.TemporaryDirectory() as directory:
                    self.gate.run_gate(endpoint="http://browser.invalid", api_key="", expected_version="2.4.8", image_reference=IMAGE, output=pathlib.Path(directory) / "out.json", fixture=fixture, requester=self.requester)
        with self.assertRaises(ValueError):
            self.run_gate(image_reference="ghcr.io/redf0x1/camofox-browser:latest")
        self.assertEqual(self.calls, [])

    def test_http_transport_bounds_response_and_does_not_follow_redirects(self):
        class Response:
            status = 200
            def getheader(self, name):
                return None
            def read1(self, amount):
                return b"x" * amount
        connection = mock.Mock()
        connection.getresponse.return_value = Response()
        with mock.patch("http.client.HTTPConnection", return_value=connection):
            with self.assertRaises(self.gate.ContractError):
                self.gate._request("http://browser.invalid", "key", "GET", "/health", None, 1, 16)
        connection.close.assert_called_once()
        connection = mock.Mock()
        response = mock.Mock(status=302)
        response.getheader.return_value = "2"
        response.read1.side_effect = [b"{}", b""]
        connection.getresponse.return_value = response
        with mock.patch("http.client.HTTPConnection", return_value=connection):
            status, _ = self.gate._request("http://browser.invalid", "key", "GET", "/health", None, 1, 16)
        self.assertEqual(status, 302)
        self.assertEqual(connection.request.call_count, 1)

    def test_non_json_create_5xx_is_status_evidence_for_scoped_recovery(self):
        connection = mock.Mock()
        response = mock.Mock(status=500)
        response.getheader.return_value = None
        response.read1.side_effect = [b"<html>upstream error details</html>", b""]
        connection.getresponse.return_value = response
        with mock.patch("http.client.HTTPConnection", return_value=connection):
            status, reply = self.gate._request(
                "http://browser.invalid", "key", "POST", "/tabs",
                {"userId": "crw-contract-test", "sessionKey": "contract"}, 1, 64,
            )
        self.assertEqual(status, 500)
        self.assertIsNone(reply, "arbitrary error body must not enter the artifact")
        connection.close.assert_called_once()

    def test_dribbling_response_cannot_renew_the_body_deadline(self):
        import time

        connection = mock.Mock()
        response = mock.Mock(status=200)
        response.getheader.return_value = None
        def dribble(amount):
            time.sleep(0.025)
            return b" "
        response.read1.side_effect = dribble
        connection.getresponse.return_value = response
        with mock.patch("http.client.HTTPConnection", return_value=connection):
            started = time.monotonic()
            with self.assertRaises(TimeoutError):
                self.gate._request("http://browser.invalid", "", "GET", "/health", None, 0.12, 64)
            self.assertLess(time.monotonic() - started, 0.24)
        self.assertGreater(response.read1.call_count, 1, "test must exercise repeated body reads")
        deadline = time.monotonic() + 0.15
        while not connection.close.called and time.monotonic() < deadline:
            time.sleep(0.005)
        connection.close.assert_called_once()


if __name__ == "__main__":
    unittest.main()
