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
        self.assertTrue(all(check["passed"] for check in artifact["checks"].values()))
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

    def test_invalid_production_is_distinguished_from_candidate_regression(self):
        gate = self.load_gate()
        baseline = response("lightpanda")
        del baseline["data"]["metadata"]["renderedWith"]
        replies = iter([baseline, response("lightpanda")])
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
        self.assertFalse(artifact["checks"]["production"]["passed"])
        self.assertTrue(artifact["checks"]["candidate"]["passed"])
        self.assertFalse(artifact["checks"]["comparison"]["passed"])
        result = artifact["cases"][0]
        self.assertEqual(result["comparison"]["markdown_size_ratio"], 1.0)
        self.assertFalse(result["checks"]["comparison"]["baseline_valid"])
        self.assertIn("no valid baseline", artifact["checks"]["comparison"]["reasons"][0])

    def test_relative_content_loss_fails_even_when_both_semantic_checks_pass(self):
        gate = self.load_gate()
        replies = iter([response("lightpanda"), response("lightpanda", "Example Domain " * 10)])
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
        self.assertTrue(artifact["checks"]["production"]["passed"])
        self.assertTrue(artifact["checks"]["candidate"]["passed"])
        self.assertFalse(artifact["checks"]["comparison"]["passed"])
        result = artifact["cases"][0]
        self.assertTrue(result["checks"]["comparison"]["baseline_valid"])
        self.assertEqual(result["comparison"]["markdown_size_ratio"], 0.5)

    def test_failed_request_keeps_candidate_evidence_and_sanitizes_error(self):
        gate = self.load_gate()

        def requester(endpoint, *args):
            if endpoint == "http://production.invalid":
                raise TimeoutError("must-not-record secret-key")
            return response("lightpanda")

        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(
                production_url="http://production.invalid",
                candidate_url="http://candidate.invalid",
                api_key="secret-key",
                output=pathlib.Path(directory) / "result.json",
                matrix=self.matrix(),
                requester=requester,
            )

        self.assertFalse(artifact["passed"])
        self.assertTrue(artifact["checks"]["candidate"]["passed"])
        self.assertFalse(artifact["checks"]["comparison"]["passed"])
        self.assertIn("comparison unavailable", "\n".join(artifact["reasons"]))
        self.assertNotIn("secret-key", json.dumps(artifact))

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

    def test_informational_case_is_reported_without_failing_the_gate(self):
        gate = self.load_gate()
        matrix = self.matrix()
        informational = dict(matrix["cases"][0], name="hostile-site", informational=True)
        matrix["cases"].append(informational)
        broken = response("lightpanda")
        del broken["data"]["metadata"]["renderedWith"]
        replies = iter([
            response("lightpanda"), response("lightpanda"),
            broken, response("lightpanda", "Example Domain " * 2),
        ])
        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(
                production_url="http://production.invalid",
                candidate_url="http://candidate.invalid",
                api_key="",
                output=pathlib.Path(directory) / "result.json",
                matrix=matrix,
                requester=lambda *args: next(replies),
            )

        self.assertTrue(artifact["passed"], artifact["reasons"])
        self.assertEqual(artifact["reasons"], [])
        self.assertTrue(all(check["passed"] for check in artifact["checks"].values()))
        result = artifact["cases"][1]
        self.assertTrue(result["informational"])
        self.assertFalse(result["checks"]["production"]["passed"])
        self.assertFalse(result["checks"]["candidate"]["passed"])
        notes = "\n".join(artifact["informational"])
        self.assertIn("hostile-site: production used no reported renderer", notes)
        self.assertIn("hostile-site: candidate markdown is too small", notes)
        self.assertFalse(artifact["cases"][0]["informational"])

    def test_informational_flag_must_be_boolean(self):
        gate = self.load_gate()
        invalid = self.matrix()
        invalid["cases"][0]["informational"] = "yes"
        with self.assertRaisesRegex(ValueError, "informational"):
            gate._validate_matrix(invalid)

    def test_checked_in_matrix_covers_both_browsers_and_key_paths(self):
        gate = self.load_gate()
        matrix = json.loads(gate.DEFAULT_MATRIX.read_text())
        gate._validate_matrix(matrix)
        names = {case["name"] for case in matrix["cases"]}
        renderers = {case["renderer"] for case in matrix["cases"]}
        self.assertEqual(renderers, {"lightpanda", "camofox"})
        self.assertTrue(any("javascript" in name for name in names))
        self.assertTrue(any("redirect" in name for name in names))
        decisive_real_world = [
            case for case in matrix["cases"]
            if "real-world" in case["name"] and not case.get("informational", False)
        ]
        self.assertTrue(decisive_real_world, "a non-informational real-world case must decide")

    def test_reddit_case_is_informational(self):
        # Reddit actively restricts automated and anonymous access, so its
        # outcome is evidence, not a verdict on the candidate.
        gate = self.load_gate()
        matrix = json.loads(gate.DEFAULT_MATRIX.read_text())
        reddit = [case for case in matrix["cases"] if "reddit.com" in case["url"]]
        self.assertTrue(reddit)
        self.assertTrue(all(case.get("informational") is True for case in reddit))

    def test_javascript_markers_describe_rendered_main_content(self):
        gate = self.load_gate()
        matrix = json.loads(gate.DEFAULT_MATRIX.read_text())
        main_content = "Albert Einstein\nJ.K. Rowling\n" + "Rendered quote text. " * 30
        for case in matrix["cases"]:
            if "javascript" not in case["name"]:
                continue
            with self.subTest(renderer=case["renderer"]):
                self.assertEqual(case["requiredText"], ["Albert Einstein", "J.K. Rowling"])
                summary, markdown = gate._summary(response(case["renderer"], main_content), 0)
                self.assertEqual(gate._semantic_reasons(case, "candidate", summary, markdown), [])

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
