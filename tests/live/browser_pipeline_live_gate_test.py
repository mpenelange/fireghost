import copy
import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "browser-pipeline-live-gate.py"
MATRIX = ROOT / "tests" / "fixtures" / "browser-pipeline-live-matrix.json"


class BrowserPipelineLiveGateTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location("browser_pipeline_live_gate", SCRIPT)
        self.gate = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.gate)
        self.matrix = json.loads(MATRIX.read_text())
        self.calls = []

    def response(self, payload):
        profile = payload["profile"]
        markdown = "Readable public material " * 100
        metadata = {
            "pipeline": "browser-v1", "profile": profile, "sourceURL": payload["url"],
            "url": payload["url"], "title": "must-not-record-title", "renderedWith": "camofox",
            "itemsCollected": 0, "reportedTotal": None, "complete": True,
            "stopReason": "snapshot", "rounds": 1, "elapsedMs": 10,
        }
        data = {"markdown": markdown, "metadata": metadata, "warnings": []}
        if profile == "redditThread":
            thread = self.gate.thread_id(payload["url"])
            comments = [
                {"id": f"t1_c{index}", "parentId": "t3_" + thread if index == 0 else "t1_c0",
                 "depth": 0 if index == 0 else 1, "permalink": None,
                 "author": "must-not-record-author", "markdown": "Readable public comment"}
                for index in range(10)
            ]
            data["json"] = {"post": {"markdown": "Requested post body"}, "comments": comments}
            metadata.update(itemsCollected=10, reportedTotal=200, complete=False, stopReason="noControls")
            data["warnings"] = ["must-not-record-warning-text"]
        return {"success": True, "data": data}

    def requester(self, endpoint, key, method, path, payload, timeout, max_bytes):
        self.calls.append((endpoint, key, method, path, payload, timeout, max_bytes))
        self.assertEqual(method, "POST")
        self.assertEqual(key, "must-not-record-key")
        if path == "/mcp":
            self.assertEqual(payload["method"], "tools/call")
            self.assertEqual(payload["params"]["name"], "browser_scrape")
            reply = {"jsonrpc": "2.0", "id": payload["id"], "result": {
                "structuredContent": self.response(payload["params"]["arguments"]),
                "content": [{"type": "text", "text": "must-not-record-MCP-text"}],
            }}
        else:
            self.assertEqual(path, "/v2/browser/scrape")
            reply = self.response(payload)
        return 200, json.dumps(reply).encode()

    def run_gate(self, requester=None, matrix=None, include_mcp=False):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "pipeline.json"
            artifact = self.gate.run_gate(
                endpoint="http://candidate.invalid:8080", api_key="must-not-record-key",
                output=output, matrix=matrix or self.matrix, include_mcp=include_mcp,
                requester=requester or self.requester, repo_revision="test-revision",
            )
            self.assertEqual(json.loads(output.read_text()), artifact)
            return artifact

    def mutate(self, change):
        def requester(*args):
            status, raw = self.requester(*args)
            reply = json.loads(raw)
            payload = args[4]
            if args[3] == "/v2/browser/scrape":
                change(reply, payload)
            return status, json.dumps(reply).encode()
        return requester

    def test_fixture_cases_run_and_artifact_omits_untrusted_content(self):
        artifact = self.run_gate()
        self.assertTrue(artifact["passed"], artifact["reasons"])
        self.assertEqual(len(artifact["cases"]), 4)
        self.assertEqual(len(self.calls), 4)
        self.assertTrue(all(call[6] == 1048576 for call in self.calls))
        self.assertTrue(all(call[5] == 65 for call in self.calls))
        serialized = json.dumps(artifact)
        for private in ["must-not-record", "Readable public", "Requested post body"]:
            self.assertNotIn(private, serialized)
        self.assertEqual(artifact["repo_revision"], "test-revision")
        for result in artifact["cases"]:
            self.assertTrue(result["passed"])
            self.assertGreaterEqual(result["summary"]["markdown_bytes"], 100)

    def test_optional_actual_mcp_requires_structured_response_without_is_error(self):
        artifact = self.run_gate(include_mcp=True)
        self.assertTrue(artifact["passed"], artifact["reasons"])
        self.assertEqual(len(artifact["cases"]), 5)
        self.assertEqual(artifact["cases"][-1]["transport"], "mcp")
        self.assertEqual(self.calls[-1][3], "/mcp")
        for invalid in [{"isError": True}, {"structuredContent": None}]:
            def requester(*args):
                status, raw = self.requester(*args)
                reply = json.loads(raw)
                if args[3] == "/mcp":
                    reply["result"].update(invalid)
                return status, json.dumps(reply).encode()
            self.assertFalse(self.run_gate(requester, include_mcp=True)["passed"])

    def test_non_200_false_success_wrong_pipeline_and_wrong_renderer_fail(self):
        for change in [
            lambda reply, _: reply.update(success=False, error="must-not-record-private-error"),
            lambda reply, _: reply["data"]["metadata"].update(pipeline="must-not-record-private-pipeline"),
            lambda reply, _: reply["data"]["metadata"].update(renderedWith="must-not-record-private-engine"),
        ]:
            artifact = self.run_gate(self.mutate(change))
            self.assertFalse(artifact["passed"])
            self.assertNotIn("must-not-record", json.dumps(artifact))
        def requester(*args):
            _, raw = self.requester(*args)
            return 500, raw
        self.assertFalse(self.run_gate(requester)["passed"])

    def test_reddit_never_claims_complete_and_counts_match_actual_comments(self):
        for change in [
            lambda data: data["metadata"].update(complete=True),
            lambda data: data["metadata"].update(itemsCollected=99),
            lambda data: data["json"].update(comments=[]),
        ]:
            def mutate(reply, payload):
                if payload["profile"] == "redditThread":
                    change(reply["data"])
            artifact = self.run_gate(self.mutate(mutate))
            self.assertFalse(artifact["passed"])
            self.assertTrue(artifact["cases"][0]["passed"], "other case evidence must be retained")

    def test_unique_ids_parent_links_and_partial_deleted_markers(self):
        def duplicate(reply, payload):
            if payload["profile"] == "redditThread":
                reply["data"]["json"]["comments"][1]["id"] = "t1_c0"
        self.assertFalse(self.run_gate(self.mutate(duplicate))["passed"])
        def wrong_parent(reply, payload):
            if payload["profile"] == "redditThread":
                reply["data"]["json"]["comments"][0]["parentId"] = "t3_wrongthread"
        self.assertFalse(self.run_gate(self.mutate(wrong_parent))["passed"])
        def deleted(reply, payload):
            if payload["profile"] == "redditThread":
                comment = reply["data"]["json"]["comments"][1]
                comment.update(id=None, parentId=None, author=None, markdown="[deleted]")
        self.assertTrue(self.run_gate(self.mutate(deleted))["passed"])
        def unknown_readable(reply, payload):
            if payload["profile"] == "redditThread":
                reply["data"]["json"]["comments"][1].update(id=None, parentId=None)
        self.assertFalse(self.run_gate(self.mutate(unknown_readable))["passed"])

    def test_parent_cycles_fail_while_unloaded_parent_links_remain_valid_partial_evidence(self):
        def cycle(reply, payload):
            if payload["profile"] == "redditThread":
                comments = reply["data"]["json"]["comments"]
                comments[0].update(parentId="t1_c1", depth=2)
        self.assertFalse(self.run_gate(self.mutate(cycle))["passed"])
        def unloaded_parent(reply, payload):
            if payload["profile"] == "redditThread":
                reply["data"]["json"]["comments"][1]["parentId"] = "t1_unloaded"
        self.assertTrue(self.run_gate(self.mutate(unloaded_parent))["passed"])

    def test_redirect_within_thread_is_allowed_but_other_threads_and_secret_urls_fail(self):
        def same_thread(reply, payload):
            if payload["profile"] == "redditThread":
                reply["data"]["metadata"]["url"] = payload["url"] + "?token=must-not-record-query"
        artifact = self.run_gate(self.mutate(same_thread))
        self.assertTrue(artifact["passed"], artifact["reasons"])
        self.assertNotIn("must-not-record-query", json.dumps(artifact))
        def other_thread(reply, payload):
            reply["data"]["metadata"]["url"] = "https://www.reddit.com/r/selfhosted/comments/wrongthread/must-not-record-secret/"
        artifact = self.run_gate(self.mutate(other_thread))
        self.assertFalse(artifact["passed"])
        self.assertNotIn("must-not-record", json.dumps(artifact))

    def test_round_item_byte_and_elapsed_budgets_are_enforced(self):
        for field, value in [("rounds", 101), ("itemsCollected", 1001), ("elapsedMs", 60001)]:
            artifact = self.run_gate(self.mutate(lambda reply, _: reply["data"]["metadata"].update({field: value})))
            self.assertFalse(artifact["passed"])
        matrix = copy.deepcopy(self.matrix)
        matrix["cases"] = [matrix["cases"][1]]
        matrix["cases"][0]["request"]["maxBytes"] = 3000
        artifact = self.run_gate(matrix=matrix)
        self.assertFalse(artifact["passed"], "markdown plus structured JSON must fit Reddit maxBytes")

    def test_utf8_markdown_floor_and_safe_unknown_metadata(self):
        def small(reply, _):
            reply["data"]["markdown"] = "x"
            reply["data"]["metadata"].update(title="must-not-record-private-title", stopReason="must-not-record-private-reason")
        artifact = self.run_gate(self.mutate(small))
        self.assertFalse(artifact["passed"])
        self.assertNotIn("must-not-record", json.dumps(artifact))

    def test_transport_and_invalid_json_fail_without_copying_exception_text(self):
        for failure in [TimeoutError("must-not-record-private-exception"), ValueError("must-not-record-private-body")]:
            def requester(*args):
                raise failure
            artifact = self.run_gate(requester)
            self.assertFalse(artifact["passed"])
            self.assertEqual(len(artifact["cases"]), 4)
            self.assertNotIn("must-not-record", json.dumps(artifact))
        self.assertFalse(self.run_gate(lambda *args: (200, b"not JSON"))["passed"])

    def test_existing_output_and_invalid_matrix_fail_before_requests(self):
        for field, value in [("schemaVersion", 2), ("maxResponseBytes", 1000000000)]:
            matrix = copy.deepcopy(self.matrix)
            matrix[field] = value
            with self.assertRaises(ValueError):
                self.run_gate(matrix=matrix)
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "old.json"
            output.write_text("retained")
            with self.assertRaises(FileExistsError):
                self.gate.run_gate(endpoint="http://candidate.invalid", api_key="", output=output, matrix=self.matrix, requester=self.requester)
            self.assertEqual(output.read_text(), "retained")
        self.assertEqual(self.calls, [])

    def test_cli_returns_nonzero_for_live_failures(self):
        with mock.patch.object(self.gate, "run_gate", return_value={"passed": False, "reasons": ["safe failure"]}):
            with mock.patch("builtins.print"):
                result = self.gate.main(["--router-url", "http://candidate.invalid", "--output", "unused.json"])
        self.assertEqual(result, 1)


if __name__ == "__main__":
    unittest.main()
