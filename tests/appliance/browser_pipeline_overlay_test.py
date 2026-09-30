"""Render the opt-in candidate contract without starting or building services."""

import json
import os
import pathlib
import shutil
import subprocess
import tomllib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
OVERLAY = ROOT / "dev" / "compose.browser-pipeline.yaml"
CONFIG = ROOT / "deployment" / "crw.browser-pipeline.toml"
LEGACY_BROWSER = "ghcr.io/redf0x1/camofox-browser@sha256:41e79fb61d50f0a8292b2a51c81ebcb0a2be24d89e9eac970edd12613006ced7"
PIPELINE_BROWSER = "ghcr.io/redf0x1/camofox-browser:2.4.8@sha256:1c1370acdd17f7d0336b64aff4ba4cf31e2b41cc90b23ddcf6135aee81380a55"


class BrowserPipelineOverlayTest(unittest.TestCase):
    def test_candidate_toml_adds_only_dedicated_browser_endpoint(self):
        baseline = tomllib.loads((ROOT / "deployment" / "crw.toml").read_text())
        candidate = tomllib.loads(CONFIG.read_text())
        dedicated = candidate["renderer"].pop("browser_pipeline")
        self.assertEqual(dedicated, {"base_url": "http://camofox-pipeline:9377"})
        self.assertEqual(candidate, baseline, "legacy renderer/search settings must remain equivalent")

    @unittest.skipUnless(shutil.which("docker"), "Docker Compose is required to render the candidate")
    def test_rendered_overlay_preserves_baseline_and_isolates_pipeline(self):
        self.assertTrue(OVERLAY.is_file(), "optional browser-pipeline overlay must exist")
        env = dict(os.environ)
        env.update({
            "CRW_IMAGE": "baseline-crw:contract-test",
            "BROWSER_PIPELINE_CRW_VERSION": "1.2.0-browser-pipeline-contract",
            "BROWSER_PIPELINE_ROUTER_VERSION": "browser-pipeline-contract",
            "MONOREPO_REVISION": "a" * 40,
            "BROWSER_PIPELINE_BUILD_DATE": "2026-09-30T00:00:00Z",
            "ROUTER_HOST_PORT": "33031",
            "ROUTER_API_KEY": "contract-test-placeholder",
            # Deliberately hostile inherited settings must be disabled by overlay.
            "FIRECRAWL_CLOUD_API_KEY": "unused-contract-test-placeholder",
            "MCP_ENABLED": "false",
            "ROUTER_BROWSER_PIPELINE_ENABLED": "false",
            "ROUTER_MONTHLY_CLOUD_CREDITS": "200",
            "ROUTER_CLOUD_BURST_CREDITS": "20",
            "ROUTER_CLOUD_REFILL_CREDITS_PER_DAY": "20",
        })
        result = subprocess.run([
            "docker", "compose", "--project-directory", str(ROOT / "dev"),
            "--env-file", str(ROOT / "dev" / ".env.example"),
            "-f", str(ROOT / "dev" / "compose.yaml"),
            "-f", str(OVERLAY),
            "-f", str(ROOT / "dev" / "compose.staging.yaml"),
            "--project-name", "fireghost-browser-pipeline-contract", "config", "--format", "json",
        ], cwd=ROOT, env=env, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr)
        rendered = json.loads(result.stdout)
        services = rendered["services"]
        legacy, pipeline = services["camofox"], services["camofox-pipeline"]
        self.assertEqual(legacy["image"], LEGACY_BROWSER)
        self.assertEqual(pipeline["image"], PIPELINE_BROWSER)
        self.assertEqual(pipeline["healthcheck"], legacy["healthcheck"])
        self.assertEqual(pipeline["deploy"], legacy["deploy"])
        self.assertEqual(pipeline["environment"], legacy["environment"])
        self.assertEqual(legacy["volumes"][0]["source"], "camofox-profiles")
        self.assertEqual(pipeline["volumes"][0]["source"], "camofox-pipeline-profiles")
        self.assertEqual(pipeline["volumes"][0]["target"], "/home/node/.camofox")
        self.assertEqual(set(pipeline["networks"]), {"appliance"})
        self.assertEqual(rendered["networks"]["appliance"]["name"], "fireghost-browser-pipeline-contract_appliance")
        for name in ("camofox-profiles", "camofox-pipeline-profiles", "router-data"):
            self.assertEqual(rendered["volumes"][name]["name"], "fireghost-browser-pipeline-contract_" + name)
            self.assertFalse(rendered["volumes"][name].get("external", False))
        for name, service in services.items():
            if name != "router":
                self.assertFalse(service.get("ports"), name + " must stay private")
        router = services["router"]
        self.assertEqual(len(router["ports"]), 1)
        self.assertEqual(router["ports"][0]["host_ip"], "127.0.0.1")
        self.assertEqual(str(router["ports"][0]["published"]), "33031")
        self.assertEqual(router["image"], "fireghost-router:browser-pipeline-contract")
        self.assertEqual(router["build"]["args"]["VERSION"], "browser-pipeline-contract")
        self.assertEqual(router["build"]["args"]["REVISION"], "a" * 40)
        self.assertEqual(router["environment"]["MCP_ENABLED"], "true")
        self.assertEqual(router["environment"]["ROUTER_BROWSER_PIPELINE_ENABLED"], "true")
        self.assertEqual(router["environment"]["FIRECRAWL_CLOUD_API_KEY"], "")
        for setting in ("ROUTER_DAILY_CLOUD_CREDITS", "ROUTER_MONTHLY_CLOUD_CREDITS",
                        "ROUTER_CLOUD_BURST_CREDITS", "ROUTER_CLOUD_REFILL_CREDITS_PER_DAY"):
            self.assertEqual(router["environment"][setting], "0")
        crw = services["crw"]
        self.assertEqual(crw["image"], "fireghost-crw:1.2.0-browser-pipeline-contract")
        self.assertEqual(crw["build"]["args"]["CRW_VERSION"], "1.2.0-browser-pipeline-contract")
        self.assertEqual(crw["build"]["args"]["CRW_REVISION"], "a" * 40)
        config_mounts = [mount for mount in crw["volumes"] if mount["target"] == "/app/config/crw.toml"]
        self.assertEqual(len(config_mounts), 1)
        self.assertEqual(pathlib.Path(config_mounts[0]["source"]), CONFIG)
        self.assertTrue(config_mounts[0]["read_only"])
        self.assertEqual(crw["depends_on"]["camofox-pipeline"]["condition"], "service_healthy")
        self.assertEqual(crw["depends_on"]["camofox"]["condition"], "service_healthy")
        self.assertIn("extends:", OVERLAY.read_text(), "reuse baseline browser operational settings")


if __name__ == "__main__":
    unittest.main()
