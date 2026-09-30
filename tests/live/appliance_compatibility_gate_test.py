import importlib.util
import json
import pathlib
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]


class ApplianceCompatibilityGateTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location(
            "appliance_compatibility", ROOT / "scripts/appliance-compatibility-gate.py")
        self.gate = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.gate)
        self.matrix = {"schemaVersion": 1, "cases": [{
            "name": "article", "path": "/v2/scrape", "kind": "scrape",
            "body": {"url": "https://example.com/?private=hidden"},
            "minimumContentChars": 10, "minimumCandidateRatio": 0.9,
            "requiredText": ["Example Domain"], "timeoutSeconds": 60,
        }]}

    def request(self, endpoint, key, path, body, timeout):
        if path == "/metrics":
            return 200, 'web_retrieval_cloud_attempts_total{endpoint="search"} 0\nweb_retrieval_cloud_attempts_total{endpoint="scrape"} 0\n'
        return 200, {"success": True, "data": {
            "markdown": "Example Domain readable content", "metadata": {"statusCode": 200},
            "futureField": "tolerate unknown response fields"}}

    def run_gate(self, request=None, matrix=None):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "result.json"
            result = self.gate.run_gate(
                baseline_url="http://baseline:8080", candidate_url="http://candidate:8080",
                api_key="never-retain-key", output=output, matrix=matrix or self.matrix,
                requester=request or self.request, repo_revision="test-revision")
            self.assertEqual(json.loads(output.read_text()), result)
            return result

    def test_legacy_response_without_renderer_provenance_and_future_fields(self):
        result = self.run_gate()
        self.assertTrue(result["passed"], result["reasons"])
        self.assertEqual({row["phase"] for row in result["cases"]}, {"cold", "warm"})
        self.assertTrue(all(row["comparison"]["baselineValid"] for row in result["cases"]))
        serialized = json.dumps(result)
        for private in ["never-retain-key", "private=hidden", "readable content", "futureField"]:
            self.assertNotIn(private, serialized)

    def test_failed_control_does_not_establish_candidate_regression(self):
        def request(endpoint, *args):
            if endpoint.startswith("http://baseline") and args[1] != "/metrics":
                return 200, {"success": False}
            return self.request(endpoint, *args)
        result = self.run_gate(request)
        self.assertFalse(result["passed"])
        self.assertTrue(all(row["candidate"]["passed"] for row in result["cases"]))
        self.assertTrue(all(not row["comparison"]["baselineValid"] for row in result["cases"]))

    def test_candidate_content_loss_fails_valid_comparison(self):
        def request(endpoint, *args):
            if endpoint.startswith("http://candidate") and args[1] != "/metrics":
                return 200, {"success": True, "data": {"markdown": "Example Domain", "metadata": {"statusCode": 200}}}
            return self.request(endpoint, *args)
        result = self.run_gate(request)
        self.assertFalse(result["passed"])
        self.assertTrue(all(row["comparison"]["baselineValid"] for row in result["cases"]))
        self.assertTrue(any("ratio" in reason for reason in result["reasons"]))

    def test_origin_error_is_not_successful_scrape(self):
        def request(endpoint, *args):
            status, body = self.request(endpoint, *args)
            if isinstance(body, dict):
                body["data"]["metadata"]["statusCode"] = 404
            return status, body
        self.assertFalse(self.run_gate(request)["passed"])

    def test_untrusted_origin_status_is_not_retained_in_artifact(self):
        for unsafe in ["private-upstream-token", {"credential": "private-upstream-token"}, True]:
            def request(endpoint, *args):
                status, body = self.request(endpoint, *args)
                if isinstance(body, dict):
                    body["data"]["metadata"]["statusCode"] = unsafe
                return status, body
            result = self.run_gate(request)
            self.assertFalse(result["passed"])
            self.assertNotIn("private-upstream-token", json.dumps(result))
            self.assertTrue(all(row["baseline"]["originStatus"] is None for row in result["cases"]))

    def test_search_requires_valid_rows_and_title_overlap(self):
        matrix = {"schemaVersion": 1, "cases": [{
            "name": "search", "path": "/v2/search", "kind": "search",
            "body": {"query": "public query", "limit": 5}, "minimumResults": 1,
            "minimumTitleOverlap": 0.5, "timeoutSeconds": 60, "concurrent": True}]}
        def request(endpoint, key, path, body, timeout):
            if path == "/metrics":
                return self.request(endpoint, key, path, body, timeout)
            title = "same title" if endpoint.startswith("http://baseline") else "different title"
            return 200, {"success": True, "data": {"web": [{"title": title, "url": "https://example.com/"}]}}
        result = self.run_gate(request, matrix)
        self.assertFalse(result["passed"])
        self.assertEqual({row["phase"] for row in result["cases"]}, {"cold", "warm", "concurrent"})

    def test_missing_cloud_metrics_and_cloud_attempts_fail_closed(self):
        for payload in ["", 'web_retrieval_cloud_attempts_total{endpoint="search"} 1\nweb_retrieval_cloud_attempts_total{endpoint="scrape"} 0\n']:
            def request(endpoint, key, path, body, timeout):
                if path == "/metrics":
                    return 200, payload
                return self.request(endpoint, key, path, body, timeout)
            self.assertFalse(self.run_gate(request)["passed"])


if __name__ == "__main__":
    unittest.main()
