import json
import pathlib
import re
import subprocess
import sys
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "release_tags.py"
WORKFLOW = ROOT / ".forgejo" / "workflows" / "release.yaml"
FORGEJO_APPLIANCE = ROOT / ".forgejo" / "workflows" / "appliance.yaml"
FORGEJO_CRW = ROOT / ".forgejo" / "workflows" / "crw.yaml"
FORGEJO_ROUTER = ROOT / ".forgejo" / "workflows" / "router.yaml"
GITHUB_CI = ROOT / ".github" / "workflows" / "ci.yaml"
COMPOSE = ROOT / "docker-compose.yml"
ENV_EXAMPLE = ROOT / ".env.example"


class ReleaseTagContractTest(unittest.TestCase):
    def run_script(self, tag):
        return subprocess.run(
            [sys.executable, str(SCRIPT), tag],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )

    def test_stable_appliance_tag_emits_version_minor_and_latest(self):
        result = self.run_script("appliance-v2.3.4")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            json.loads(result.stdout),
            {"minor": "2.3", "tags": ["2.3.4", "2.3", "latest"], "version": "2.3.4"},
        )

    def test_component_and_malformed_tags_are_rejected(self):
        for tag in ("v2.3.4", "1.2.0-fw.3", "appliance-v2.3", "appliance-v2.3.4-rc.1"):
            with self.subTest(tag=tag):
                result = self.run_script(tag)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("appliance-vMAJOR.MINOR.PATCH", result.stderr)

    def test_promotion_status_prevents_latest_and_minor_regression(self):
        result = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--promotion-status",
                "appliance-v2.3.4",
                "router-v9.0.0",
                "appliance-v2.3.5",
                "appliance-v3.0.0",
                "appliance-v2.3.4",
            ],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            json.loads(result.stdout),
            {"publish_latest": False, "publish_minor": False},
        )

    def test_newest_version_advances_latest_and_its_minor_alias(self):
        result = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--promotion-status",
                "appliance-v3.0.0",
                "appliance-v2.3.5",
                "appliance-v3.0.0",
            ],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            json.loads(result.stdout),
            {"publish_latest": True, "publish_minor": True},
        )


class ReleaseWorkflowContractTest(unittest.TestCase):
    def test_release_installs_pinned_rust_before_full_gate(self):
        text = WORKFLOW.read_text(encoding="utf-8")
        install = text.index("uses: https://github.com/dtolnay/rust-toolchain@stable")
        gate = text.index("make check")
        self.assertLess(install, gate)
        self.assertIn("toolchain: 1.93.1", text[install:gate])

    def test_non_release_workflows_do_not_run_for_tags(self):
        for path in (FORGEJO_APPLIANCE, FORGEJO_CRW, FORGEJO_ROUTER):
            with self.subTest(workflow=path.name):
                text = path.read_text(encoding="utf-8")
                push_block = text[text.index("  push:"):text.index("  pull_request:")]
                self.assertIn("branches:", push_block)
                self.assertNotIn("tags:", push_block)

    def test_forgejo_smoke_uses_host_runner_localhost(self):
        text = FORGEJO_APPLIANCE.read_text(encoding="utf-8")
        self.assertIn("docker exec fake-cloud python -c 'import socket", text)
        self.assertIn("curl -fsS http://127.0.0.1:33000/health", text)
        self.assertIn("make smoke", text)
        self.assertNotIn("docker network connect", text)

    def test_github_ci_splits_checks_across_standard_hosted_runners(self):
        text = GITHUB_CI.read_text(encoding="utf-8")
        self.assertEqual(text.count("runs-on: ubuntu-latest"), 3)
        self.assertIn("make check-router", text)
        self.assertIn("make check-crw", text)
        self.assertIn("make test-appliance compose-config check-stack-lock test-hermes-regression", text)
        self.assertIn("make build-router", text)
        self.assertIn("docker exec fake-cloud python -c 'import socket", text)
        self.assertNotIn("REGISTRY_TOKEN", text)
        self.assertNotIn("docker buildx build", text)

    def test_workflow_is_tag_only_and_runs_checks_before_publishing_both_images(self):
        text = WORKFLOW.read_text(encoding="utf-8")
        self.assertRegex(text, r"(?m)^\s+tags:\s*$")
        self.assertIn("appliance-v*.*.*", text)
        self.assertNotIn("pull_request:", text)
        self.assertLess(text.index("make check"), text.index("docker buildx build"))
        self.assertIn("scripts/release_tags.py", text)
        self.assertIn("hermes-web-retrieval-router", text)
        self.assertIn("hermes-web-retrieval-crw", text)
        self.assertIn("REGISTRY_USERNAME", text)
        self.assertIn("REGISTRY_TOKEN", text)

    def test_stable_tags_are_promoted_only_after_both_candidate_images_pass_live_contract(self):
        text = WORKFLOW.read_text(encoding="utf-8")
        build_positions = [match.start() for match in re.finditer("docker buildx build", text)]
        self.assertEqual(len(build_positions), 2)
        self.assertIn("candidate-$version-$GITHUB_RUN_ID", text)
        self.assertIn("group: appliance-release", text)
        validation = text.index("name: Validate candidate appliance")
        promotion = text.index("name: Promote tested images")
        self.assertGreater(validation, max(build_positions))
        self.assertGreater(promotion, validation)
        validation_block = text[validation:promotion]
        self.assertIn("deployment/compose.release-smoke.yaml", validation_block)
        self.assertIn("scripts/live-contract-test.py", validation_block)
        self.assertIn("FIRECRAWL_ENABLED=false", validation_block)
        self.assertIn("down -v", validation_block)
        self.assertGreater(text.index("docker buildx imagetools create"), promotion)
        promotion_block = text[promotion:]
        self.assertIn("--promotion-status", promotion_block)
        self.assertIn("already exists with unexpected provenance", promotion_block)
        self.assertIn("org.opencontainers.image.revision", promotion_block)
        self.assertIn('git show -s --format=%cI "$GITHUB_SHA"', text)
        self.assertNotIn("date -u +%Y-%m-%dT%H:%M:%SZ", text)

    def test_release_uses_separate_bounded_build_validation_and_promotion_jobs(self):
        text = WORKFLOW.read_text(encoding="utf-8")
        self.assertEqual(text.count("runs-on: ubuntu-latest"), 5)
        self.assertIn("  build-router:\n    needs: verify", text)
        self.assertIn("  build-crw:\n    needs: verify", text)
        self.assertIn("  validate:\n    needs: [verify, build-router, build-crw]", text)
        self.assertIn("  promote:\n    needs: [verify, validate]", text)
        validation = text[text.index("  validate:"):text.index("  promote:")]
        self.assertLess(
            validation.index("docker system prune -af --volumes"),
            validation.index('"${compose[@]}" up -d --pull always --wait'),
        )
        self.assertIn("if: always()", validation)
        self.assertIn("needs.verify.outputs.candidate", validation)

    def test_public_compose_tracks_owned_latest_tags_but_pins_third_party_images(self):
        compose = COMPOSE.read_text(encoding="utf-8")
        env = ENV_EXAMPLE.read_text(encoding="utf-8")
        self.assertIn(
            "image: ${ROUTER_IMAGE:-git.firewire.cc/michael/hermes-web-retrieval-router:latest}",
            compose,
        )
        self.assertIn(
            "image: ${CRW_IMAGE:-git.firewire.cc/michael/hermes-web-retrieval-crw:latest}",
            compose,
        )
        self.assertRegex(env, r"(?m)^ROUTER_IMAGE=\S+/hermes-web-retrieval-router:latest$")
        self.assertRegex(env, r"(?m)^CRW_IMAGE=\S+/hermes-web-retrieval-crw:latest$")
        third_party = [
            value
            for value in re.findall(r"(?m)^\s+image:\s*(.+)$", compose)
            if "camofox-browser" in value or "lightpanda/browser" in value
        ]
        self.assertEqual(len(third_party), 2)
        for image in third_party:
            self.assertRegex(image, r"@sha256:[0-9a-f]{64}$")


if __name__ == "__main__":
    unittest.main()
