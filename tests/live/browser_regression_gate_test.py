import importlib.util
import json
import pathlib
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "browser_regression_gate", ROOT / "scripts" / "browser-regression-gate.py"
)


def response(renderer, markdown="Example Domain " * 20, title="Example Domain"):
    return {
        "success": True,
        "data": {
            "markdown": markdown,
            "metadata": {
                "title": title,
                "statusCode": 200,
                "renderedWith": renderer,
                "sourceURL": "https://example.com/",
            },
        },
    }


class BrowserRegressionGateTests(unittest.TestCase):
    def load_gate(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        return gate

    def matrix(self):
        return {
            "schemaVersion": 1,
            "cases": [
                {
                    "name": "static-lightpanda",
                    "renderer": "lightpanda",
                    "url": "https://example.com/?token=must-not-record",
                    "minimumMarkdownChars": 100,
                    "minimumCandidateRatio": 0.9,
                    "requiredText": ["Example Domain"],
                    "forbiddenText": ["Just a moment"],
                }
            ],
        }

    def test_gate_forces_renderer_compares_output_and_writes_safe_artifact(self):
        gate = self.load_gate()
        calls = []

        def requester(endpoint, api_key, payload, timeout):
            calls.append((endpoint, api_key, payload, timeout))
            return response(payload["renderer"])

        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "result.json"
            artifact = gate.run_gate(
                production_url="http://production.invalid",
                candidate_url="http://candidate.invalid",
                api_key="secret-key",
                output=output,
                matrix=self.matrix(),
                requester=requester,
                repo_revision="abc123",
            )
            stored = json.loads(output.read_text())

        self.assertTrue(artifact["passed"], artifact["reasons"])
        self.assertEqual(len(calls), 2)
        self.assertTrue(all(call[2]["renderer"] == "lightpanda" for call in calls))
        self.assertTrue(all(call[2]["formats"] == ["markdown"] for call in calls))
        serialized = json.dumps(stored)
        self.assertNotIn("secret-key", serialized)
        self.assertNotIn("must-not-record", serialized)
        self.assertNotIn("Example Domain Example Domain", serialized)
        result = stored["cases"][0]
        self.assertEqual(result["candidate"]["rendered_with"], "lightpanda")
        self.assertGreaterEqual(result["comparison"]["markdown_size_ratio"], 0.9)

    def test_regressions_accumulate_actionable_reasons(self):
        gate = self.load_gate()
        replies = iter([
            response("camofox", "Expected marker " * 20),
            response("lightpanda", "Just a moment", title="Blocked"),
        ])
        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(
                production_url="http://production.invalid",
                candidate_url="http://candidate.invalid",
                api_key="",
                output=pathlib.Path(directory) / "result.json",
                matrix=self.matrix(),
                requester=lambda *args: next(replies),
            )

        self.assertFalse(artifact["passed"])
        reasons = "\n".join(artifact["reasons"])
        self.assertIn("production used camofox; expected lightpanda", reasons)
        self.assertIn("candidate markdown is too small", reasons)
        self.assertIn("candidate is missing required text", reasons)
        self.assertIn("candidate contains forbidden text", reasons)

    def test_invalid_matrix_fails_before_any_request(self):
        gate = self.load_gate()
        invalid = self.matrix()
        invalid["cases"][0]["renderer"] = "auto"
        calls = []
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, "renderer"):
                gate.run_gate(
                    production_url="http://production.invalid",
                    candidate_url="http://candidate.invalid",
                    api_key="",
                    output=pathlib.Path(directory) / "result.json",
                    matrix=invalid,
                    requester=lambda *args: calls.append(args),
                )
        self.assertEqual(calls, [])

    def test_checked_in_matrix_covers_both_browsers_and_key_paths(self):
        gate = self.load_gate()
        matrix = json.loads(gate.DEFAULT_MATRIX.read_text())
        gate._validate_matrix(matrix)
        names = {case["name"] for case in matrix["cases"]}
        renderers = {case["renderer"] for case in matrix["cases"]}
        self.assertEqual(renderers, {"lightpanda", "camofox"})
        self.assertTrue(any("javascript" in name for name in names))
        self.assertTrue(any("redirect" in name for name in names))
        self.assertTrue(any("real-world" in name for name in names))

    def test_artifact_is_never_overwritten(self):
        gate = self.load_gate()
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "result.json"
            output.write_text("preserve me")
            with self.assertRaises(FileExistsError):
                gate.run_gate(
                    production_url="http://production.invalid",
                    candidate_url="http://candidate.invalid",
                    api_key="",
                    output=output,
                    matrix=self.matrix(),
                    requester=lambda *args: response("lightpanda"),
                )
            self.assertEqual(output.read_text(), "preserve me")


if __name__ == "__main__":
    unittest.main()
