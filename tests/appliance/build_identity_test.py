import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]


class BuildIdentityTest(unittest.TestCase):
    def test_current_public_identity_is_fireghost(self):
        readme = (ROOT / "README.md").read_text(encoding="utf-8")
        makefile = (ROOT / "Makefile").read_text(encoding="utf-8")
        self.assertTrue(readme.startswith("# Fireghost\n"))
        self.assertIn("git clone https://git.firewire.cc/michael/fireghost.git", readme)
        self.assertIn("MONOREPO_SOURCE ?= https://git.firewire.cc/michael/fireghost", makefile)

    def test_root_builds_forward_monorepo_identity(self):
        makefile = (ROOT / "Makefile").read_text(encoding="utf-8")
        for argument in (
            "--build-arg VERSION=$(ROUTER_VERSION)",
            "--build-arg REVISION=$(MONOREPO_REVISION)",
            "--build-arg BUILD_DATE=$(BUILD_DATE)",
            "--build-arg SOURCE=$(MONOREPO_SOURCE)",
            "--build-arg CRW_VERSION=$(CRW_CANDIDATE_VERSION)",
            "--build-arg CRW_REVISION=$(MONOREPO_REVISION)",
            "--build-arg CRW_BUILD_DATE=$(BUILD_DATE)",
            "--build-arg CRW_SOURCE=$(MONOREPO_SOURCE)",
        ):
            self.assertIn(argument, makefile)

    def test_crw_image_source_is_the_monorepo(self):
        dockerfile = (ROOT / "crw" / "Dockerfile").read_text(encoding="utf-8")
        runtime_stage = dockerfile.split("FROM debian:bookworm-slim", maxsplit=1)[1]
        self.assertIn("ARG CRW_SOURCE", runtime_stage)
        self.assertIn('org.opencontainers.image.source="$CRW_SOURCE"', runtime_stage)
        self.assertIn('org.opencontainers.image.title="Fireghost CRW"', runtime_stage)

    def test_router_oci_identity_is_fireghost(self):
        dockerfile = (ROOT / "router" / "Dockerfile").read_text(encoding="utf-8")
        self.assertIn('org.opencontainers.image.title="Fireghost Router"', dockerfile)

    def test_compose_forwards_router_source(self):
        compose = (ROOT / "deploy" / "compose.yaml").read_text(encoding="utf-8")
        self.assertIn("SOURCE: ${MONOREPO_SOURCE", compose)


if __name__ == "__main__":
    unittest.main()
