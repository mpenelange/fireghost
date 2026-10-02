import os
import pathlib
import subprocess
import tempfile
import unittest
import uuid


ROOT = pathlib.Path(__file__).resolve().parents[2]
ALPINE = "alpine:3.22.6@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"


class BackupRestoreTest(unittest.TestCase):
    def setUp(self):
        self.project = f"web-retrieval-test-{uuid.uuid4().hex}"
        self.volumes = [
            f"{self.project}_router-data",
            f"{self.project}_camofox-profiles",
        ]
        for volume in self.volumes:
            subprocess.run(["docker", "volume", "create", volume], check=True, capture_output=True)
        self.env = os.environ | {
            "COMPOSE_PROJECT_NAME": self.project,
            "CRW_IMAGE": ALPINE,
        }

    def tearDown(self):
        subprocess.run(["docker", "volume", "rm", "-f", *self.volumes], check=False, capture_output=True)

    def run_in_volumes(self, script):
        subprocess.run(
            [
                "docker",
                "run",
                "--rm",
                "--mount",
                f"type=volume,src={self.volumes[0]},dst=/source",
                "--mount",
                f"type=volume,src={self.volumes[1]},dst=/profiles",
                ALPINE,
                "sh",
                "-eu",
                "-c",
                script,
            ],
            check=True,
        )

    def volume_entries(self):
        result = subprocess.run(
            [
                "docker",
                "run",
                "--rm",
                "--mount",
                f"type=volume,src={self.volumes[0]},dst=/source,readonly",
                "--mount",
                f"type=volume,src={self.volumes[1]},dst=/profiles,readonly",
                ALPINE,
                "sh",
                "-c",
                "find /source /profiles -mindepth 1 -print | sort",
            ],
            check=True,
            capture_output=True,
            text=True,
        )
        return result.stdout.splitlines()

    def test_restore_replaces_both_volume_trees_including_dotfiles(self):
        self.run_in_volumes(
            "mkdir -p /source/.archived /profiles/.archived; "
            "printf router >/source/.archived/value; printf profile >/profiles/.archived/value"
        )
        with tempfile.TemporaryDirectory() as temp_dir:
            archive = pathlib.Path(temp_dir) / "round-trip.tar.gz"
            subprocess.run(
                [str(ROOT / "scripts/backup.sh"), str(archive)],
                cwd=ROOT,
                env=self.env,
                check=True,
                capture_output=True,
                text=True,
            )
            self.run_in_volumes(
                "mkdir -p /source/.stale/nested /profiles/.stale/nested; "
                "touch /source/stale /source/.stale/value /profiles/stale /profiles/.stale/value"
            )
            subprocess.run(
                [str(ROOT / "scripts/restore.sh"), "--force", str(archive)],
                cwd=ROOT,
                env=self.env,
                check=True,
                capture_output=True,
                text=True,
            )

        self.assertEqual(
            self.volume_entries(),
            [
                "/profiles/.archived",
                "/profiles/.archived/value",
                "/source/.archived",
                "/source/.archived/value",
            ],
        )


if __name__ == "__main__":
    unittest.main()
