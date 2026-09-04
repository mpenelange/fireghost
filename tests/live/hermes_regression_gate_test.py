import importlib.util
import json
import pathlib
import subprocess
import tempfile
import unittest
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "hermes_regression_gate", ROOT / "scripts" / "hermes-regression-gate.py"
)
PROBE_SPEC = importlib.util.spec_from_file_location(
    "hermes_provider_probe", ROOT / "scripts" / "hermes-provider-probe.py"
)


class HermesRegressionGateTests(unittest.TestCase):
    METRICS_ZERO = """web_retrieval_requests_total{endpoint="search",status_class="2xx"} 0
web_retrieval_requests_total{endpoint="scrape",status_class="2xx"} 0
web_retrieval_cache_hits_total{endpoint="search"} 0
web_retrieval_cache_hits_total{endpoint="scrape"} 0
web_retrieval_local_attempts_total{endpoint="search"} 0
web_retrieval_local_attempts_total{endpoint="scrape"} 0
web_retrieval_cloud_attempts_total{endpoint="search"} 0
web_retrieval_cloud_attempts_total{endpoint="scrape"} 0
"""

    @staticmethod
    def metrics(search_requests=0, scrape_requests=0, search_cache=0, scrape_cache=0,
                search_local=0, scrape_local=0, search_cloud=0, scrape_cloud=0):
        return f"""web_retrieval_requests_total{{endpoint="search",status_class="2xx"}} {search_requests}
web_retrieval_requests_total{{endpoint="scrape",status_class="2xx"}} {scrape_requests}
web_retrieval_cache_hits_total{{endpoint="search"}} {search_cache}
web_retrieval_cache_hits_total{{endpoint="scrape"}} {scrape_cache}
web_retrieval_local_attempts_total{{endpoint="search"}} {search_local}
web_retrieval_local_attempts_total{{endpoint="scrape"}} {scrape_local}
web_retrieval_cloud_attempts_total{{endpoint="search"}} {search_cloud}
web_retrieval_cloud_attempts_total{{endpoint="scrape"}} {scrape_cloud}
"""

    def test_probe_uses_actual_hermes_search_and_extraction_payloads(self):
        probe = importlib.util.module_from_spec(PROBE_SPEC)
        PROBE_SPEC.loader.exec_module(probe)

        class FakeProvider:
            def __init__(self):
                self.calls = []

            def is_available(self):
                return True

            def search(self, query, limit=5):
                self.calls.append(("search", query, limit))
                return {"success": True, "data": {"web": [{"title": "T", "url": "https://x", "description": "D"}]}}

            async def extract(self, urls, **kwargs):
                self.calls.append(("extract", urls, kwargs))
                return [{"url": urls[0], "title": "Example Domain", "content": "body"}]

        provider = FakeProvider()
        result = probe.run_probe(provider, clock=lambda: 1.0)
        self.assertEqual(provider.calls[0], ("search", probe.SEARCH_QUERY, probe.SEARCH_LIMIT))
        self.assertEqual(provider.calls[1], ("extract", [probe.EXTRACT_URL], {"format": "markdown"}))
        self.assertTrue(result["search"]["success"])
        self.assertTrue(result["extract"]["success"])
        self.assertEqual(probe.SEARCH_QUERY, "Python programming language official documentation")

    def test_provider_probe_runs_once_per_endpoint_in_separate_processes(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        calls = []

        def command_runner(command, env, timeout):
            config = pathlib.Path(env["HERMES_HOME"], "config.yaml").read_text()
            self.assertIn("backend: firecrawl", config)
            self.assertIn("use_gateway: false", config)
            calls.append((command, env, timeout))
            return json.dumps(
                {
                    "available": True,
                    "search": {"success": True, "results": []},
                    "extract": {"success": True, "items": []},
                    "latency_seconds": {"search": 0.05, "extract": 0.05, "total": 0.1},
                }
            )

        with mock.patch.dict(
            "os.environ",
            {"OPENAI_API_KEY": "must-not-leak", "UNRELATED_SECRET": "must-not-leak"},
            clear=False,
        ), tempfile.TemporaryDirectory() as directory:
            gate.run_gate(
                production_url="http://production.invalid",
                candidate_url="http://candidate.invalid",
                api_key="test-secret",
                output=pathlib.Path(directory) / "result.json",
                command_runner=command_runner,
                metrics_fetcher=lambda _: self.METRICS_ZERO,
                hermes_python="/custom/hermes-python",
            )

        self.assertEqual(len(calls), 2)
        self.assertEqual(
            [call[1]["FIRECRAWL_API_URL"] for call in calls],
            ["http://production.invalid", "http://candidate.invalid"],
        )
        self.assertTrue(all(call[1]["FIRECRAWL_API_KEY"] == "test-secret" for call in calls))
        self.assertTrue(all("hermes-provider-probe.py" in call[0][-1] for call in calls))
        self.assertTrue(all(call[0][0] == "/custom/hermes-python" for call in calls))
        homes = [call[1]["HERMES_HOME"] for call in calls]
        self.assertEqual(len(set(homes)), 2)
        self.assertTrue(all(home != "/root/.hermes" for home in homes))
        self.assertTrue(all(call[1].get("HOME") == call[1]["HERMES_HOME"] for call in calls))
        forbidden = {"FIRECRAWL_GATEWAY_URL", "TOOL_GATEWAY_DOMAIN", "TOOL_GATEWAY_SCHEME",
                     "TOOL_GATEWAY_USER_TOKEN", "HERMES_TOOL_PROVIDER", "WEB_PROVIDER",
                     "OPENAI_API_KEY", "UNRELATED_SECRET"}
        self.assertTrue(all(not forbidden.intersection(call[1]) for call in calls))

    def test_artifact_records_semantic_checks_and_cloud_metric_deltas(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        probe = {
            "available": True,
            "search": {"success": True, "results": [
                {"title": "Hermes", "url": "https://example.test/result", "description": "result"}
                for _ in range(5)
            ]},
            "extract": {"success": True, "items": [{
                "url": "https://example.com/", "title": "Example Domain", "content_chars": 167
            }]},
            "latency_seconds": {"search": 1.0, "extract": 1.0, "total": 2.0},
        }
        metrics = iter([self.metrics(search_cloud=41, scrape_cloud=8),
                        self.metrics(1, 1, 1, 0, 0, 1, 41, 8), self.METRICS_ZERO,
                        self.metrics(1, 1, 0, 1, 1, 0, 0, 0)])
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "artifact.json"
            artifact = gate.run_gate(
                production_url="http://production.invalid",
                candidate_url="http://candidate.invalid",
                api_key="do-not-record",
                output=output,
                command_runner=lambda *args: json.dumps(probe),
                metrics_fetcher=lambda _: next(metrics),
            )
            stored = json.loads(output.read_text())

        self.assertTrue(artifact["passed"])
        self.assertEqual(stored["metrics"]["production"]["delta"]["search"], 0)
        self.assertEqual(stored["metrics"]["candidate"]["delta"]["scrape"], 0)
        self.assertEqual(stored["metrics"]["production"]["delta"]["requests"]["search"], 1)
        self.assertEqual(stored["metrics"]["candidate"]["delta"]["cache_hits"]["scrape"], 1)
        self.assertEqual(stored["metrics"]["candidate"]["delta"]["local_attempts"]["search"], 1)
        self.assertIn("timestamp", stored)
        self.assertIn("thresholds", stored)
        self.assertNotIn("do-not-record", json.dumps(stored))
        self.assertNotIn("content", stored["probes"]["candidate"]["extract"]["items"][0])

    def test_custom_search_query_reaches_probe_and_artifact(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        probe = {
            "available": True,
            "search": {"success": True, "results": [
                {"title": "Python", "url": "https://python.org", "description": "docs"}]},
            "extract": {"success": True, "items": [{
                "url": "https://example.com/", "title": "Example Domain", "content_chars": 167}]},
            "latency_seconds": {"search": 1.0, "extract": 1.0, "total": 2.0},
        }
        observed_queries = []
        metrics = iter([
            self.METRICS_ZERO,
            self.metrics(1, 1, search_local=1, scrape_local=1),
            self.METRICS_ZERO,
            self.metrics(1, 1, search_local=1, scrape_local=1),
        ])

        def command_runner(command, env, timeout):
            observed_queries.append(env["HERMES_REGRESSION_SEARCH_QUERY"])
            return json.dumps(probe)

        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(
                production_url="http://p", candidate_url="http://c", api_key="x",
                output=pathlib.Path(directory) / "a.json",
                search_query="Python docs cold-run marker",
                command_runner=command_runner,
                metrics_fetcher=lambda _: next(metrics),
            )

        self.assertTrue(artifact["passed"], artifact["reasons"])
        self.assertEqual(observed_queries, ["Python docs cold-run marker"] * 2)
        self.assertEqual(
            artifact["reproducibility"]["probe_inputs"]["search_query"],
            "Python docs cold-run marker",
        )

    def test_production_cloud_fallback_is_baseline_when_candidate_stays_local(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        probe = {
            "available": True,
            "search": {"success": True, "results": [
                {"title": "Python", "url": "https://python.org", "description": "docs"}]},
            "extract": {"success": True, "items": [{
                "url": "https://example.com/", "title": "Example Domain", "content_chars": 167}]},
            "latency_seconds": {"search": 1.0, "extract": 1.0, "total": 2.0},
        }
        metrics = iter([
            self.METRICS_ZERO,
            self.metrics(1, 1, search_local=1, search_cloud=1),
            self.METRICS_ZERO,
            self.metrics(1, 1, search_local=1),
        ])
        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(
                production_url="http://p", candidate_url="http://c", api_key="x",
                output=pathlib.Path(directory) / "a.json",
                command_runner=lambda *args: json.dumps(probe),
                metrics_fetcher=lambda _: next(metrics),
            )

        self.assertTrue(artifact["passed"], artifact["reasons"])
        self.assertEqual(
            artifact["metrics"]["production"]["delta"]["cloud_attempts"]["search"], 1
        )
        self.assertEqual(
            artifact["metrics"]["candidate"]["delta"]["cloud_attempts"]["search"], 0
        )

    def test_candidate_cloud_attempt_or_semantic_regression_fails_with_reasons(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        weak = {
            "available": True,
            "search": {"success": True, "results": [{"title": "missing fields"}]},
            "extract": {"success": True, "items": [{"title": "Wrong", "content_chars": 12}]},
            "latency_seconds": {"total": 121.0},
        }
        metrics = iter([self.METRICS_ZERO, self.metrics(1, 1),
                        self.METRICS_ZERO, self.metrics(1, 1, search_cloud=1)])
        responses = iter([json.dumps(weak | {"latency_seconds": {"total": 1.0}}), json.dumps(weak)])
        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(
                production_url="http://p", candidate_url="http://c", api_key="x",
                output=pathlib.Path(directory) / "a.json",
                command_runner=lambda *args: next(responses), metrics_fetcher=lambda _: next(metrics),
            )
        self.assertFalse(artifact["passed"])
        joined = " ".join(artifact["reasons"])
        self.assertIn("candidate cloud attempts increased", joined)
        self.assertIn("required fields", joined)
        self.assertIn("extraction title", joined)
        self.assertIn("latency", joined)

    def test_missing_metrics_or_wrong_endpoint_is_fail_closed(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        good = {"available": True, "search": {"success": True, "results": [
            {"title": "Python", "url": "https://python.org", "description": "docs"}]},
            "extract": {"success": True, "items": [{"title": "Example Domain", "content_chars": 150}]},
            "latency_seconds": {"total": 1.0}}
        metrics = iter([self.METRICS_ZERO, self.METRICS_ZERO, self.METRICS_ZERO, ""])
        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(
                production_url="http://p", candidate_url="http://c", api_key="x",
                output=pathlib.Path(directory) / "a.json",
                command_runner=lambda *args: json.dumps(good), metrics_fetcher=lambda _: next(metrics))
        self.assertFalse(artifact["passed"])
        self.assertIn("missing required metric", " ".join(artifact["reasons"]))

    def test_success_requires_search_and_scrape_request_deltas_on_each_router(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        probe = {"available": True, "search": {"success": True, "results": [
            {"title": "Python", "url": "https://python.org", "description": "docs"}]},
            "extract": {"success": True, "items": [{"title": "Example Domain", "content_chars": 150}]},
            "latency_seconds": {"total": 1.0}}
        metrics = iter([self.METRICS_ZERO, self.metrics(1, 1),
                        self.METRICS_ZERO, self.metrics(1, 0)])
        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(production_url="http://p", candidate_url="http://c", api_key="x",
                output=pathlib.Path(directory) / "a.json", command_runner=lambda *a: json.dumps(probe),
                metrics_fetcher=lambda _: next(metrics))
        self.assertFalse(artifact["passed"])
        self.assertIn("candidate scrape request delta", " ".join(artifact["reasons"]))

    def test_candidate_uses_absolute_latency_ceiling(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        self.assertNotIn("candidate_latency_multiplier", gate.THRESHOLDS)
        self.assertEqual(gate.THRESHOLDS["candidate_total_timeout_seconds"], 120.0)

    def test_probe_failure_writes_sanitized_failing_artifact(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        secret = "super-secret-token"
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "failure.json"
            artifact = gate.run_gate(
                production_url="http://p", candidate_url="http://c", api_key=secret, output=output,
                command_runner=lambda *args: (_ for _ in ()).throw(
                    subprocess.TimeoutExpired(args[0], args[2], stderr=secret)),
                metrics_fetcher=lambda _: self.METRICS_ZERO)
            stored = output.read_text()
        self.assertFalse(artifact["passed"])
        self.assertIn("probe timed out", " ".join(artifact["reasons"]))
        self.assertNotIn(secret, stored)
        self.assertNotIn("stderr", stored)

    def test_artifact_records_reproducibility_identity_without_secrets(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        probe = {"available": False, "identity": {"python": "3.13.7", "provider": "FirecrawlWebSearchProvider"}}
        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(
                production_url="http://p", candidate_url="http://c", api_key="hidden",
                output=pathlib.Path(directory) / "a.json", command_runner=lambda *a: json.dumps(probe),
                metrics_fetcher=lambda _: self.METRICS_ZERO, repo_revision="abc123")
        self.assertEqual(artifact["reproducibility"]["repo_head"], "abc123")
        self.assertEqual(artifact["reproducibility"]["probe_inputs"]["search_query"],
                         "Python programming language official documentation")
        self.assertIn("check_definitions", artifact["reproducibility"])
        self.assertNotIn("hidden", json.dumps(artifact))

    def test_cli_endpoints_are_fixed_and_distinct(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        gate._validate_cli_endpoints(gate.PRODUCTION_URL, gate.CANDIDATE_URL)
        for production, candidate in (
            (gate.PRODUCTION_URL, gate.PRODUCTION_URL),
            (gate.CANDIDATE_URL, gate.PRODUCTION_URL),
            ("http://elsewhere", gate.CANDIDATE_URL),
        ):
            with self.assertRaises(ValueError):
                gate._validate_cli_endpoints(production, candidate)

    def test_metrics_are_bracketed_per_probe_and_extra_traffic_fails(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        probe = {
            "available": True,
            "search": {"success": True, "results": [
                {"title": "Python", "url": "https://python.org", "description": "docs"}]},
            "extract": {"success": True, "items": [{
                "url": "https://example.com/", "title": "Example Domain", "content_chars": 167}]},
            "latency_seconds": {"total": 1.0},
        }
        fetches = []
        samples = iter([
            self.METRICS_ZERO, self.metrics(2, 1),
            self.METRICS_ZERO, self.metrics(1, 1),
        ])
        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(
                production_url="http://p", candidate_url="http://c", api_key="x",
                output=pathlib.Path(directory) / "a.json",
                command_runner=lambda *a: json.dumps(probe),
                metrics_fetcher=lambda endpoint: fetches.append(endpoint) or next(samples),
            )
        self.assertEqual(fetches, ["http://p", "http://p", "http://c", "http://c"])
        self.assertFalse(artifact["passed"])
        self.assertIn("production search request delta is 2", " ".join(artifact["reasons"]))

    def test_candidate_must_match_production_behavior(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        production = {
            "available": True,
            "search": {"success": True, "results": [
                {"title": "Python docs", "url": "https://python.org", "description": "docs"},
                {"title": "Library reference", "url": "https://docs.python.org", "description": "reference"},
            ]},
            "extract": {"success": True, "items": [{
                "url": "https://example.com/", "title": "Example Domain", "content_chars": 167}]},
            "latency_seconds": {"total": 1.0},
        }
        candidate = {
            **production,
            "search": {"success": True, "results": [
                {"title": "Unrelated one", "url": "https://one.invalid", "description": "x"},
                {"title": "Unrelated two", "url": "https://two.invalid", "description": "y"},
            ]},
            "extract": {"success": True, "items": [{
                "url": "https://example.com/", "title": "Example Domain", "content_chars": 100}]},
        }
        responses = iter([json.dumps(production), json.dumps(candidate)])
        samples = iter([
            self.METRICS_ZERO, self.metrics(1, 1),
            self.METRICS_ZERO, self.metrics(1, 1),
        ])
        with tempfile.TemporaryDirectory() as directory:
            artifact = gate.run_gate(
                production_url="http://p", candidate_url="http://c", api_key="x",
                output=pathlib.Path(directory) / "a.json",
                command_runner=lambda *a: next(responses), metrics_fetcher=lambda _: next(samples),
            )
        self.assertFalse(artifact["passed"])
        joined = " ".join(artifact["reasons"])
        self.assertIn("search title overlap", joined)
        self.assertIn("extraction content is smaller than production", joined)

    def test_artifact_bounds_and_redacts_untrusted_strings_and_refuses_overwrite(self):
        gate = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(gate)
        secret = "sk-test-secret-value"
        probe = {
            "available": True,
            "identity": {"python": "3.13", "provider": "provider"},
            "search": {"success": True, "results": [{
                "title": "T" * 1000 + secret,
                "url": f"https://user:{secret}@example.com/path?token={secret}#fragment",
                "description": f"API_KEY={secret}" + "D" * 1000,
            }]},
            "extract": {"success": True, "items": [{
                "url": f"https://example.com/?token={secret}",
                "title": "Example Domain", "content_chars": 167}]},
            "latency_seconds": {"total": 1.0},
        }
        samples = iter([
            self.METRICS_ZERO, self.metrics(1, 1),
            self.METRICS_ZERO, self.metrics(1, 1),
        ])
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "a.json"
            gate.run_gate(
                production_url="http://p", candidate_url="http://c", api_key="x",
                output=output, search_query=f"query token={secret}",
                command_runner=lambda *a: json.dumps(probe), metrics_fetcher=lambda _: next(samples),
            )
            stored = output.read_text()
            self.assertNotIn(secret, stored)
            self.assertNotIn("/path", stored)
            self.assertNotIn("fragment", stored)
            self.assertLess(len(stored), 20000)
            with self.assertRaises(FileExistsError):
                gate.run_gate(
                    production_url="http://p", candidate_url="http://c", api_key="x",
                    output=output, command_runner=lambda *a: json.dumps(probe),
                    metrics_fetcher=lambda _: self.METRICS_ZERO,
                )

    def test_make_target_invokes_gate_with_python(self):
        makefile = (ROOT / "Makefile").read_text()
        self.assertIn('$(HERMES_PYTHON) scripts/hermes-regression-gate.py', makefile)


if __name__ == "__main__":
    unittest.main()
