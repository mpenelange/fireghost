import pathlib
import re
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
COMPOSE = ROOT / "compose.yaml"
ENV_EXAMPLE = ROOT / ".env.example"


class ComposeContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.text = COMPOSE.read_text(encoding="utf-8")

    def service_block(self, name):
        match = re.search(
            rf"^  {re.escape(name)}:\n(?P<body>(?:^(?:    .*|\s*)$\n?)*)",
            self.text,
            re.MULTILINE,
        )
        self.assertIsNotNone(match, f"missing {name} service")
        return match.group("body")

    def test_renderer_images_are_immutable(self):
        self.assertIn(
            "ghcr.io/redf0x1/camofox-browser@sha256:41e79fb61d50f0a8292b2a51c81ebcb0a2be24d89e9eac970edd12613006ced7",
            self.service_block("camofox"),
        )
        self.assertIn(
            "lightpanda/browser@sha256:b4f155389e172bbc82c3dcbc2282e64db3e2160b27871ef1c53dbc28f7e96887",
            self.service_block("lightpanda"),
        )

    def test_only_router_is_published_on_loopback(self):
        router = self.service_block("router")
        self.assertRegex(router, r'127\.0\.0\.1:33000:8080')
        for name in ("crw", "camofox", "lightpanda"):
            self.assertNotRegex(self.service_block(name), r"(?m)^    ports:")
        self.assertEqual(len(re.findall(r"(?m)^    ports:", self.text)), 1)

    def test_no_mcp_service_or_port(self):
        self.assertNotRegex(self.text.lower(), r"(?m)^  .*mcp.*:")
        self.assertNotRegex(self.text, r"(?m)^\s*-?\s*['\"]?\d+:\d+.*#.*mcp")

    def test_no_secret_value_is_embedded(self):
        self.assertRegex(self.service_block("router"), r"FIRECRAWL_CLOUD_API_KEY")
        for line in self.text.splitlines():
            if not re.search(r"(?i)(api[_-]?key|token|secret)", line):
                continue
            value = re.split(r"[:=]", line.strip().removeprefix("- "), maxsplit=1)[-1].strip()
            self.assertTrue(not value or value.startswith("${"), f"embedded secret-like value: {line}")

    def test_router_receives_documented_byte_limits(self):
        router = self.service_block("router")
        for setting, default in (
            ("ROUTER_MAX_REQUEST_BYTES", "2097152"),
            ("ROUTER_MAX_RESPONSE_BYTES", "16777216"),
            ("ROUTER_CACHE_MAX_ENTRY_BYTES", "16777216"),
            ("ROUTER_CACHE_MAX_BYTES", "1073741824"),
            ("ROUTER_MAX_INFLIGHT", "64"),
        ):
            self.assertIn(f"{setting}=${{{setting}:-{default}}}", router)

    def test_router_receives_server_read_timeout(self):
        self.assertIn(
            "ROUTER_SERVER_READ_TIMEOUT=${ROUTER_SERVER_READ_TIMEOUT:-30s}",
            self.service_block("router"),
        )

    def test_router_receives_documented_monthly_reset_day(self):
        setting = "ROUTER_MONTHLY_RESET_DAY"
        self.assertIn(f"{setting}=${{{setting}:-1}}", self.service_block("router"))
        self.assertRegex(ENV_EXAMPLE.read_text(encoding="utf-8"), rf"(?m)^{setting}=1$")

    def test_services_have_security_and_operational_limits(self):
        for name in ("router", "crw", "camofox", "lightpanda"):
            block = self.service_block(name)
            self.assertRegex(block, r"(?m)^    cap_drop:\n      - ALL$")
            self.assertIn("no-new-privileges:true", block)
            self.assertRegex(block, r"(?m)^    restart: (?:unless-stopped|on-failure(?::\d+)?)$")
            self.assertRegex(block, r"(?m)^    healthcheck:$")
            self.assertRegex(block, r"(?m)^    deploy:$")
            self.assertRegex(block, r"(?m)^      resources:$")
            self.assertRegex(block, r"(?m)^        limits:$")

    def test_persistent_volumes_are_named_and_mounted(self):
        self.assertRegex(self.service_block("router"), r"router-data:/data")
        camofox = self.service_block("camofox")
        self.assertRegex(camofox, r"camofox-profiles:/home/node/\.camofox")
        self.assertIn("CAMOFOX_PROFILES_DIR=/home/node/.camofox/profiles", camofox)
        self.assertRegex(self.text, r"(?m)^volumes:\n  router-data:\n  camofox-profiles:")


if __name__ == "__main__":
    unittest.main()
