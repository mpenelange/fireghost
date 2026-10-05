import importlib.util
import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "release.yaml"

spec = importlib.util.spec_from_file_location("release_page", ROOT / "scripts" / "release_page.py")
release_page = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release_page)


class ReleasePagePayloadTest(unittest.TestCase):
    def payload(self, message, version="1.2.1"):
        return release_page.build_payload(
            "fireghost-v" + version, message, version, "sha256:aaa", "sha256:bbb", "git.firewire.cc/michael"
        )

    def test_tag_message_becomes_title_and_notes_with_image_pins(self):
        payload = self.payload("Fireghost 1.2.1\n\n- Fix one\n- Fix two\n")
        self.assertEqual(payload["tag_name"], "fireghost-v1.2.1")
        self.assertEqual(payload["name"], "Fireghost 1.2.1")
        self.assertTrue(payload["body"].startswith("- Fix one\n- Fix two\n\n## Images"))
        self.assertIn("`git.firewire.cc/michael/fireghost-router:1.2.1` (`sha256:aaa`)", payload["body"])
        self.assertIn("`git.firewire.cc/michael/fireghost-crw:1.2.1` (`sha256:bbb`)", payload["body"])
        self.assertFalse(payload["draft"])
        self.assertFalse(payload["prerelease"])

    def test_missing_message_falls_back_to_tag_name(self):
        payload = self.payload("")
        self.assertEqual(payload["name"], "fireghost-v1.2.1")
        self.assertTrue(payload["body"].startswith("## Images"))

    def test_prerelease_versions_are_marked(self):
        self.assertTrue(self.payload("Fireghost 1.3.0-rc.1", "1.3.0-rc.1")["prerelease"])


class ReleasePageWorkflowTest(unittest.TestCase):
    def test_release_page_is_published_only_after_promotion(self):
        text = WORKFLOW.read_text(encoding="utf-8")
        job = text[text.index("  publish-release:"):]
        self.assertIn("needs: [verify, promote]", job)
        self.assertIn("router_digest: ${{ steps.promote.outputs.router_digest }}", text)
        # One request shape for GitHub and Forgejo, addressed via the runner's API base.
        self.assertIn('api="${GITHUB_API_URL:-$GITHUB_SERVER_URL/api/v1}"', job)
        self.assertIn('"$api/repos/$GITHUB_REPOSITORY/releases"', job)
        self.assertIn("contents: write", job)


if __name__ == "__main__":
    unittest.main()
