import re
import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
DEV = ROOT / "dev"
OLD_DIRECTORY = "de" + "ploy"


class RepositoryLayoutTest(unittest.TestCase):
    def test_developer_appliance_has_an_unambiguous_home(self):
        self.assertTrue((DEV / "compose.yaml").is_file())
        self.assertTrue((DEV / "compose.staging.yaml").is_file())
        self.assertTrue((DEV / ".env.example").is_file())
        self.assertTrue((DEV / "stack.lock.json").is_file())
        self.assertFalse((ROOT / OLD_DIRECTORY).exists())

    def test_make_uses_the_developer_directory_as_compose_project_directory(self):
        makefile = (ROOT / "Makefile").read_text()
        expected = "COMPOSE = docker compose --project-directory dev -f dev/compose.yaml"
        self.assertIn(expected, makefile)

    def test_developer_appliance_reuses_public_crw_configuration(self):
        compose = (DEV / "compose.yaml").read_text()
        self.assertIn(
            "../deployment/crw.toml:/app/config/crw.toml:ro",
            compose,
        )
        self.assertFalse((DEV / "config" / "crw.toml").exists())

    def test_tracked_files_do_not_reference_the_old_directory(self):
        tracked = subprocess.run(
            ["git", "ls-files", "-z"],
            cwd=ROOT,
            check=True,
            capture_output=True,
        ).stdout.decode().split("\0")
        old_prefix = OLD_DIRECTORY + "/"
        offenders = []
        for relative in filter(None, tracked):
            if relative.startswith(old_prefix):
                offenders.append(relative)
                continue
            path = ROOT / relative
            try:
                text = path.read_text()
            except UnicodeDecodeError:
                continue
            old_reference = r"(?<![A-Za-z])" + re.escape(OLD_DIRECTORY) + r"(?:/|[\"'])"
            if re.search(old_reference, text):
                offenders.append(relative)
        self.assertEqual([], offenders)


if __name__ == "__main__":
    unittest.main()
