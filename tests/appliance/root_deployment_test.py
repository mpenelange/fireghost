import os
import pathlib
import re
import subprocess
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
COMPOSE = ROOT / "docker-compose.yml"
ENV_EXAMPLE = ROOT / ".env.example"
ENTRYPOINT = ROOT / "deployment" / "router-entrypoint.sh"
CRW_CONFIG = ROOT / "deployment" / "crw.toml"


class RootDeploymentContractTest(unittest.TestCase):
    def test_compose_and_environment_are_auto_discoverable_at_root(self):
        self.assertTrue(COMPOSE.is_file())
        self.assertTrue(ENV_EXAMPLE.is_file())

    def test_supporting_files_have_stable_root_relative_mounts(self):
        text = COMPOSE.read_text(encoding="utf-8")
        self.assertIn(
            "./deployment/router-entrypoint.sh:/etc/web-retrieval/router-entrypoint.sh:ro",
            text,
        )
        self.assertIn("./deployment/crw.toml:/app/config/crw.toml:ro", text)
        self.assertTrue(ENTRYPOINT.is_file())
        self.assertTrue(CRW_CONFIG.is_file())

    def test_route_and_operational_defaults_are_interpolated(self):
        text = COMPOSE.read_text(encoding="utf-8")
        env = ENV_EXAMPLE.read_text(encoding="utf-8")
        self.assertIn("${API_HOST:-api.firewire.cc}", text)
        self.assertGreaterEqual(text.count("${API_PATH_PREFIX:-/web/api}"), 3)
        for setting, default in (
            ("API_HOST", "api.firewire.cc"),
            ("API_PATH_PREFIX", "/web/api"),
            ("ROUTER_MAX_INFLIGHT", "64"),
            ("ROUTER_CLOUD_BURST_CREDITS", "20"),
            ("ROUTER_CLOUD_REFILL_CREDITS_PER_DAY", "20"),
            ("FIRECRAWL_MONTHLY_ALLOWANCE", "1500"),
            ("FIRECRAWL_BUFFER_PERCENT", "20"),
            ("ROUTER_MONTHLY_RESET_DAY", "3"),
            ("ROUTER_CLOUD_CREDIT_FLOOR", "50"),
        ):
            self.assertRegex(env, rf"(?m)^{setting}={re.escape(default)}$")

    def test_mcp_is_disabled_by_default_and_uses_existing_private_route(self):
        text = COMPOSE.read_text(encoding="utf-8")
        env = ENV_EXAMPLE.read_text(encoding="utf-8")
        self.assertIn("MCP_ENABLED=${MCP_ENABLED:-false}", text)
        self.assertRegex(env, r"(?m)^MCP_ENABLED=false$")
        self.assertIn('Path(`${API_PATH_PREFIX:-/web/api}/mcp`)', text)
        self.assertIn('PathPrefix(`${API_PATH_PREFIX:-/web/api}/v2/`)', text)
        self.assertIn("web-retrieval-private,web-retrieval-strip", text)

    def test_crw_llm_key_is_always_defined(self):
        # CRW exits at startup when the LLM provider is set without a key.
        text = COMPOSE.read_text(encoding="utf-8")
        self.assertIn("CRW_EXTRACTION__LLM__API_KEY: ${CRW_LLM_API_KEY:-}", text)
        env = ENV_EXAMPLE.read_text(encoding="utf-8")
        self.assertRegex(env, r"(?m)^CRW_LLM_API_KEY=$")

    def test_public_compose_is_pull_only_private_and_versioned(self):
        text = COMPOSE.read_text(encoding="utf-8")
        self.assertNotRegex(text, r"(?m)^\s+build:")
        self.assertNotRegex(text, r"(?m)^\s+ports:")
        images = re.findall(r"(?m)^\s+image:\s*(.+)$", text)
        self.assertEqual(len(images), 4)
        for image in (
            value
            for value in images
            if "camofox-browser" in value or "lightpanda/browser" in value
        ):
            self.assertRegex(image, r"@sha256:[0-9a-f]{64}$")
        env = ENV_EXAMPLE.read_text(encoding="utf-8")
        self.assertRegex(env, r"(?m)^ROUTER_IMAGE=git\.firewire\.cc/michael/fireghost-router:latest$")
        self.assertRegex(env, r"(?m)^CRW_IMAGE=git\.firewire\.cc/michael/fireghost-crw:latest$")

    def test_public_rename_preserves_deployment_compatibility_identifiers(self):
        text = COMPOSE.read_text(encoding="utf-8")
        self.assertIn("name: web-retrieval", text)
        self.assertIn("traefik.http.routers.web-retrieval.rule", text)
        self.assertIn("traefik.http.services.web-retrieval.loadbalancer.server.port", text)
        self.assertIn("./deployment/router-entrypoint.sh:/etc/web-retrieval/router-entrypoint.sh:ro", text)
        self.assertIn("router-data:/data", text)
        self.assertIn("camofox-profiles:/home/node/.camofox", text)
        self.assertIn("${API_HOST:-api.firewire.cc}", text)
        self.assertIn("${API_PATH_PREFIX:-/web/api}", text)

    def test_templates_contain_no_embedded_secrets(self):
        compose = COMPOSE.read_text(encoding="utf-8")
        env = ENV_EXAMPLE.read_text(encoding="utf-8")
        self.assertIn("ROUTER_API_KEY=${ROUTER_API_KEY:?", compose)
        self.assertIn("FIRECRAWL_CLOUD_API_KEY=${FIRECRAWL_CLOUD_API_KEY:-}", compose)
        self.assertRegex(env, r"(?m)^ROUTER_API_KEY=REPLACE_WITH_RANDOM_SECRET$")
        self.assertRegex(env, r"(?m)^FIRECRAWL_CLOUD_API_KEY=$")


class RootEntrypointContractTest(unittest.TestCase):
    def run_entrypoint(self, command, **values):
        env = os.environ.copy()
        env.update(values)
        return subprocess.run(
            ["sh", str(ENTRYPOINT), "sh", "-c", command],
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )

    def test_buffered_monthly_math_and_continuous_pacing(self):
        result = self.run_entrypoint(
            'printf "%s,%s" "$ROUTER_MONTHLY_CLOUD_CREDITS" "$ROUTER_DAILY_CLOUD_CREDITS"',
            FIRECRAWL_ENABLED="false",
            FIRECRAWL_MONTHLY_ALLOWANCE="1500",
            FIRECRAWL_BUFFER_PERCENT="20",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "1200,0")

    def test_cloud_enable_switch_removes_or_requires_key(self):
        disabled = self.run_entrypoint(
            'printf "%s" "${FIRECRAWL_CLOUD_API_KEY-unset}"',
            FIRECRAWL_ENABLED="false",
            FIRECRAWL_CLOUD_API_KEY="test-placeholder",
        )
        self.assertEqual(disabled.returncode, 0, disabled.stderr)
        self.assertEqual(disabled.stdout, "unset")

        enabled_without_key = self.run_entrypoint(
            "true", FIRECRAWL_ENABLED="true", FIRECRAWL_CLOUD_API_KEY=""
        )
        self.assertNotEqual(enabled_without_key.returncode, 0)


if __name__ == "__main__":
    unittest.main()
