import json
import pathlib
import subprocess
import sys
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
CHECK = ROOT / "scripts" / "check_stack_lock.py"
LOCK = ROOT / "dev" / "stack.lock.json"
COMPOSE = ROOT / "dev" / "compose.yaml"
ENV_EXAMPLE = ROOT / "dev" / ".env.example"


class StackLockTest(unittest.TestCase):
    def run_check(self, lock=LOCK, compose=COMPOSE, env=ENV_EXAMPLE):
        return subprocess.run(
            [
                sys.executable,
                str(CHECK),
                "--lock",
                str(lock),
                "--compose",
                str(compose),
                "--env",
                str(env),
            ],
            cwd=ROOT,
            text=True,
            capture_output=True,
        )

    def test_current_manifest_matches_deployment_inputs(self):
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("PASS:", result.stdout)

    def test_rejects_crw_env_digest_drift(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            env = pathlib.Path(temp_dir) / ".env"
            env.write_text(
                ENV_EXAMPLE.read_text(encoding="utf-8").replace(
                    json.loads(LOCK.read_text(encoding="utf-8"))["components"]["crw"]["image"].split("@")[1],
                    "sha256:" + "a" * 64,
                ),
                encoding="utf-8",
            )
            result = self.run_check(env=env)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("CRW_IMAGE", result.stderr)

    def test_rejects_mutable_compose_image(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            compose = pathlib.Path(temp_dir) / "compose.yaml"
            compose.write_text(
                COMPOSE.read_text(encoding="utf-8").replace(
                    "lightpanda/browser@sha256:b4f155389e172bbc82c3dcbc2282e64db3e2160b27871ef1c53dbc28f7e96887",
                    "lightpanda/browser:latest",
                ),
                encoding="utf-8",
            )
            result = self.run_check(compose=compose)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("repository digest", result.stderr)

    def test_rejects_missing_source_provenance(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            lock_path = pathlib.Path(temp_dir) / "stack.lock.json"
            lock = json.loads(LOCK.read_text(encoding="utf-8"))
            del lock["source"]["crwRevision"]
            lock_path.write_text(json.dumps(lock), encoding="utf-8")
            result = self.run_check(lock=lock_path)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("source.crwRevision", result.stderr)


if __name__ == "__main__":
    unittest.main()
