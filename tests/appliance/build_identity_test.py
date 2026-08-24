import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]


class BuildIdentityTest(unittest.TestCase):
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
        self.assertIn("ARG CRW_SOURCE", dockerfile)
        self.assertIn('org.opencontainers.image.source="$CRW_SOURCE"', dockerfile)

    def test_compose_forwards_router_source(self):
        compose = (ROOT / "deploy" / "compose.yaml").read_text(encoding="utf-8")
        self.assertIn("SOURCE: ${MONOREPO_SOURCE", compose)


if __name__ == "__main__":
    unittest.main()
