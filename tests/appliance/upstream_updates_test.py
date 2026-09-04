import json
import pathlib
import subprocess
import sys
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
TOOL = ROOT / "scripts" / "upstream_updates.py"
CHECK_UPDATES = ROOT / "scripts" / "check-updates.sh"
REVIEWED = ROOT / "upstreams.json"


def manifest():
    return {
        "schemaVersion": 1,
        "sources": {
            "crwVendor": {
                "url": "https://github.com/adambenhassen/crw-camofox",
                "ref": "refs/heads/main",
                "reviewedCommit": "1" * 40,
                "importPath": "crw",
            },
            "crwFoundation": {
                "url": "https://github.com/us/crw",
                "ref": "refs/heads/main",
                "reviewedCommit": "2" * 40,
            },
        },
        "images": {
            "camofoxBrowser": {
                "source": "https://github.com/redf0x1/camofox-browser",
                "image": "ghcr.io/redf0x1/camofox-browser:latest",
                "reviewedDigest": "sha256:" + "3" * 64,
            },
            "lightpandaBrowser": {
                "source": "https://github.com/lightpanda-io/browser",
                "image": "lightpanda/browser:latest",
                "reviewedDigest": "sha256:" + "4" * 64,
            },
        },
    }


class UpstreamManifestTest(unittest.TestCase):
    def git(self, repo, *args):
        return subprocess.run(
            ["git", "-C", str(repo), *args],
            check=True,
            text=True,
            capture_output=True,
            env={
                **__import__("os").environ,
                "GIT_AUTHOR_NAME": "Test",
                "GIT_AUTHOR_EMAIL": "test@example.invalid",
                "GIT_COMMITTER_NAME": "Test",
                "GIT_COMMITTER_EMAIL": "test@example.invalid",
            },
        )

    def target_git_state(self, repo):
        return {
            "status": self.git(repo, "status", "--porcelain=v1").stdout,
            "refs": self.git(
                repo, "for-each-ref", "--format=%(refname) %(objectname)"
            ).stdout,
            "objects": self.git(repo, "count-objects", "-v").stdout,
        }

    def test_validate_accepts_reviewed_upstreams_manifest(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = pathlib.Path(temp_dir) / "upstreams.json"
            path.write_text(json.dumps(manifest()), encoding="utf-8")
            result = subprocess.run(
                [sys.executable, str(TOOL), "--manifest", str(path), "validate"],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("valid: 2 sources, 2 images", result.stdout)

    def test_repository_manifest_tracks_discovery_not_deployment(self):
        result = subprocess.run(
            [sys.executable, str(TOOL), "validate"],
            cwd=ROOT,
            text=True,
            capture_output=True,
        )
        data = json.loads(REVIEWED.read_text(encoding="utf-8"))

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(set(data), {"schemaVersion", "sources", "images"})
        self.assertEqual(
            data["sources"]["crwVendor"]["reviewedCommit"],
            "ca65413060fc3daaf621c0a81cd3d0368160402e",
        )
        self.assertEqual(
            data["sources"]["crwFoundation"]["reviewedCommit"],
            "aac7999b9379fd8b6ef818ce37f78634416f79c1",
        )

    def test_check_updates_entrypoint_delegates_to_manifest_tool(self):
        result = subprocess.run(
            [str(CHECK_UPDATES), "--manifest", str(REVIEWED), "validate"],
            cwd=ROOT,
            text=True,
            capture_output=True,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("valid: 2 sources, 2 images", result.stdout)

    def test_check_reports_current_changed_and_unavailable(self):
        data = manifest()
        data["sources"]["crwFoundation"]["url"] = "unavailable-source"
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = pathlib.Path(temp_dir)
            path = temp / "upstreams.json"
            path.write_text(json.dumps(data), encoding="utf-8")
            fake_git = temp / "fake-git"
            fake_git.write_text(
                "#!/bin/sh\n"
                "case \"$3\" in\n"
                "  *crw-camofox) printf '%s\\t%s\\n' '1111111111111111111111111111111111111111' \"$4\" ;;\n"
                "  *) printf '%s\\n' 'network disabled' >&2; exit 1 ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            fake_image = temp / "fake-image"
            fake_image.write_text(
                "#!/bin/sh\n"
                "case \"$1\" in\n"
                "  *camofox*) printf 'Digest: %s\\n' 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' ;;\n"
                "  *) printf 'Digest: %s\\n' 'sha256:4444444444444444444444444444444444444444444444444444444444444444' ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            fake_git.chmod(0o755)
            fake_image.chmod(0o755)
            result = subprocess.run(
                [
                    sys.executable,
                    str(TOOL),
                    "--manifest",
                    str(path),
                    "check",
                    "--git-command",
                    str(fake_git),
                    "--image-command",
                    str(fake_image),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )

        self.assertEqual(result.returncode, 1)
        self.assertIn("source crwVendor: current", result.stdout)
        self.assertIn(
            "observed 1111111111111111111111111111111111111111",
            result.stdout,
        )
        self.assertIn("source crwFoundation: unavailable", result.stdout)
        self.assertIn("image camofoxBrowser: changed", result.stdout)
        self.assertIn("image lightpandaBrowser: current", result.stdout)
        self.assertIn(
            "observed sha256:4444444444444444444444444444444444444444444444444444444444444444",
            result.stdout,
        )

    def test_check_reports_non_utf8_probe_output_as_unavailable(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = pathlib.Path(temp_dir)
            path = temp / "upstreams.json"
            path.write_text(json.dumps(manifest()), encoding="utf-8")
            malformed = temp / "malformed-probe"
            malformed.write_bytes(b"#!/bin/sh\nprintf '\\377'\n")
            malformed.chmod(0o755)
            result = subprocess.run(
                [
                    sys.executable,
                    str(TOOL),
                    "--manifest",
                    str(path),
                    "check",
                    "--git-command",
                    str(malformed),
                    "--image-command",
                    str(malformed),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )

        self.assertEqual(result.returncode, 1)
        self.assertIn("source crwVendor: unavailable", result.stdout)
        self.assertIn("image camofoxBrowser: unavailable", result.stdout)
        self.assertNotIn("Traceback", result.stderr)

    def test_check_reports_missing_probe_commands_as_unavailable(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            path = pathlib.Path(temp_dir) / "upstreams.json"
            path.write_text(json.dumps(manifest()), encoding="utf-8")
            result = subprocess.run(
                [
                    sys.executable,
                    str(TOOL),
                    "--manifest",
                    str(path),
                    "check",
                    "--git-command",
                    str(path.parent / "missing-git"),
                    "--image-command",
                    str(path.parent / "missing-image"),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )

        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout.count(": unavailable"), 4)
        self.assertNotIn("Traceback", result.stderr)

    def test_check_uses_labeled_root_manifest_digest(self):
        data = manifest()
        root_digest = data["images"]["camofoxBrowser"]["reviewedDigest"]
        platform_digest = "sha256:" + "a" * 64
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = pathlib.Path(temp_dir)
            path = temp / "upstreams.json"
            path.write_text(json.dumps(data), encoding="utf-8")
            fake_git = temp / "fake-git"
            fake_git.write_text(
                "#!/bin/sh\nprintf '%s\\t%s\\n' \"$4\" \"$4\"\n",
                encoding="utf-8",
            )
            fake_image = temp / "fake-image"
            fake_image.write_text(
                "#!/bin/sh\n"
                f"printf '%s\\n' 'Platform digest: {platform_digest}' "
                f"'Digest: {root_digest}'\n",
                encoding="utf-8",
            )
            fake_git.chmod(0o755)
            fake_image.chmod(0o755)
            result = subprocess.run(
                [
                    sys.executable,
                    str(TOOL),
                    "--manifest",
                    str(path),
                    "check",
                    "--git-command",
                    str(fake_git),
                    "--image-command",
                    str(fake_image),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )

        self.assertIn("image camofoxBrowser: current", result.stdout)
        self.assertIn(f"observed {root_digest}", result.stdout)

    def test_prepare_crw_writes_reviewable_patch_without_changing_target(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = pathlib.Path(temp_dir)
            upstream = temp / "upstream"
            target = temp / "monorepo"
            output = temp / "review"
            upstream.mkdir()
            target.mkdir()
            self.git(upstream, "init", "-q")
            (upstream / "server.txt").write_text("reviewed\n", encoding="utf-8")
            self.git(upstream, "add", "server.txt")
            self.git(upstream, "commit", "-qm", "reviewed")
            reviewed = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
            (upstream / "server.txt").write_text("updated\n", encoding="utf-8")
            self.git(upstream, "commit", "-qam", "upstream update")
            candidate = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
            (target / "crw").mkdir()
            (target / "crw" / "server.txt").write_text("reviewed\n", encoding="utf-8")
            self.git(target, "init", "-q")
            self.git(target, "add", "crw/server.txt")
            self.git(target, "commit", "-qm", "import")
            data = manifest()
            data["sources"]["crwVendor"]["reviewedCommit"] = reviewed
            path = temp / "upstreams.json"
            path.write_text(json.dumps(data), encoding="utf-8")

            result = subprocess.run(
                [
                    sys.executable,
                    str(TOOL),
                    "--manifest",
                    str(path),
                    "prepare-crw",
                    "--source-repo",
                    str(upstream),
                    "--candidate",
                    candidate,
                    "--target-repo",
                    str(target),
                    "--output",
                    str(output),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(
                (target / "crw" / "server.txt").read_text(encoding="utf-8"),
                "reviewed\n",
            )
            patch_text = (output / "crw-fork.patch").read_text(encoding="utf-8")
            review = json.loads((output / "review.json").read_text(encoding="utf-8"))

        self.assertIn("a/crw/server.txt", patch_text)
        self.assertEqual(review["reviewedCommit"], reviewed)
        self.assertEqual(review["candidateCommit"], candidate)
        self.assertEqual(review["applicability"], "clean")
        self.assertIn("prepared:", result.stdout)

    def test_prepare_crw_retains_patch_and_reports_conflict(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = pathlib.Path(temp_dir)
            upstream = temp / "upstream"
            target = temp / "monorepo"
            output = temp / "review"
            upstream.mkdir()
            target.mkdir()
            self.git(upstream, "init", "-q")
            (upstream / "server.txt").write_text("reviewed\n", encoding="utf-8")
            self.git(upstream, "add", "server.txt")
            self.git(upstream, "commit", "-qm", "reviewed")
            reviewed = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
            (upstream / "server.txt").write_text("upstream\n", encoding="utf-8")
            self.git(upstream, "commit", "-qam", "upstream update")
            candidate = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
            (target / "crw").mkdir()
            (target / "crw" / "server.txt").write_text("fork patch\n", encoding="utf-8")
            self.git(target, "init", "-q")
            self.git(target, "add", "crw/server.txt")
            self.git(target, "commit", "-qm", "fork patch")
            data = manifest()
            data["sources"]["crwVendor"]["reviewedCommit"] = reviewed
            path = temp / "upstreams.json"
            path.write_text(json.dumps(data), encoding="utf-8")

            result = subprocess.run(
                [
                    sys.executable,
                    str(TOOL),
                    "--manifest",
                    str(path),
                    "prepare-crw",
                    "--source-repo",
                    str(upstream),
                    "--candidate",
                    candidate,
                    "--target-repo",
                    str(target),
                    "--output",
                    str(output),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )
            review = json.loads((output / "review.json").read_text(encoding="utf-8"))
            patch_retained = (output / "crw-fork.patch").is_file()

        self.assertEqual(result.returncode, 1)
        self.assertEqual(review["applicability"], "conflict")
        self.assertTrue(patch_retained)
        self.assertIn("conflict: patch retained for review", result.stderr)

    def test_prepare_crw_reports_patch_already_integrated(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = pathlib.Path(temp_dir)
            upstream = temp / "upstream"
            target = temp / "monorepo"
            output = temp / "review"
            upstream.mkdir()
            target.mkdir()
            self.git(upstream, "init", "-q")
            (upstream / "README.md").write_text("reviewed\n", encoding="utf-8")
            self.git(upstream, "add", "README.md")
            self.git(upstream, "commit", "-qm", "reviewed")
            reviewed = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
            (upstream / "workflow.yml").write_text("already here\n", encoding="utf-8")
            self.git(upstream, "add", "workflow.yml")
            self.git(upstream, "commit", "-qm", "upstream addition")
            candidate = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
            self.git(target, "init", "-q")
            (target / "README.md").write_text("reviewed\n", encoding="utf-8")
            self.git(target, "add", "README.md")
            self.git(target, "commit", "-qm", "fork base")
            (target / "workflow.yml").write_text("already here\n", encoding="utf-8")
            self.git(target, "add", "workflow.yml")
            self.git(target, "commit", "-qm", "equivalent upstream addition")
            (target / "workflow.yml").write_text(
                "already here\nmaintained improvement\n", encoding="utf-8"
            )
            self.git(target, "commit", "-qam", "maintain integrated file")
            (target / "crw").mkdir()
            self.git(target, "mv", "README.md", "workflow.yml", "crw")
            self.git(target, "commit", "-qm", "import maintained fork")
            before = self.target_git_state(target)
            candidate_before = subprocess.run(
                ["git", "-C", str(target), "cat-file", "-e", f"{candidate}^{{commit}}"],
                text=True,
                capture_output=True,
            )
            data = manifest()
            data["sources"]["crwVendor"]["reviewedCommit"] = reviewed
            path = temp / "upstreams.json"
            path.write_text(json.dumps(data), encoding="utf-8")

            result = subprocess.run(
                [
                    sys.executable,
                    str(TOOL),
                    "--manifest",
                    str(path),
                    "prepare-crw",
                    "--source-repo",
                    str(upstream),
                    "--candidate",
                    candidate,
                    "--target-repo",
                    str(target),
                    "--output",
                    str(output),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )
            review = json.loads((output / "review.json").read_text(encoding="utf-8"))
            patch_retained = (output / "crw-fork.patch").is_file()
            after = self.target_git_state(target)
            candidate_after = subprocess.run(
                ["git", "-C", str(target), "cat-file", "-e", f"{candidate}^{{commit}}"],
                text=True,
                capture_output=True,
            )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(review["applicability"], "already-integrated")
        self.assertFalse(review["actionRequired"])
        self.assertTrue(patch_retained)
        self.assertEqual(after, before)
        self.assertNotEqual(candidate_before.returncode, 0)
        self.assertNotEqual(candidate_after.returncode, 0)
        self.assertIn("already-integrated", result.stdout)

    def test_prepare_crw_does_not_treat_patchless_ranges_as_integrated(self):
        for range_kind in ("empty-commit", "merge-only"):
            with self.subTest(range_kind=range_kind), tempfile.TemporaryDirectory() as temp_dir:
                temp = pathlib.Path(temp_dir)
                upstream = temp / "upstream"
                target = temp / "monorepo"
                output = temp / "review"
                upstream.mkdir()
                target.mkdir()
                self.git(upstream, "init", "-q")
                (upstream / "server.txt").write_text("base\n", encoding="utf-8")
                self.git(upstream, "add", "server.txt")
                self.git(upstream, "commit", "-qm", "base")
                base = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
                (upstream / "server.txt").write_text("reviewed\n", encoding="utf-8")
                self.git(upstream, "commit", "-qam", "reviewed")
                reviewed = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
                if range_kind == "empty-commit":
                    self.git(upstream, "commit", "--allow-empty", "-qm", "empty")
                    candidate = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
                else:
                    tree = self.git(upstream, "rev-parse", f"{reviewed}^{{tree}}").stdout.strip()
                    candidate = self.git(
                        upstream,
                        "commit-tree",
                        tree,
                        "-p",
                        reviewed,
                        "-p",
                        base,
                        "-m",
                        "merge only",
                    ).stdout.strip()
                self.git(target, "init", "-q")
                (target / "crw").mkdir()
                (target / "crw" / "server.txt").write_text(
                    "reviewed\n", encoding="utf-8"
                )
                self.git(target, "add", "crw/server.txt")
                self.git(target, "commit", "-qm", "import")
                data = manifest()
                data["sources"]["crwVendor"]["reviewedCommit"] = reviewed
                path = temp / "upstreams.json"
                path.write_text(json.dumps(data), encoding="utf-8")

                result = subprocess.run(
                    [
                        sys.executable,
                        str(TOOL),
                        "--manifest",
                        str(path),
                        "prepare-crw",
                        "--source-repo",
                        str(upstream),
                        "--candidate",
                        candidate,
                        "--target-repo",
                        str(target),
                        "--output",
                        str(output),
                    ],
                    cwd=ROOT,
                    text=True,
                    capture_output=True,
                )
                review = json.loads(
                    (output / "review.json").read_text(encoding="utf-8")
                )

            self.assertEqual(result.returncode, 1)
            self.assertEqual(review["applicability"], "conflict")

    def test_prepare_crw_requires_every_source_patch_in_target_history(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = pathlib.Path(temp_dir)
            upstream = temp / "upstream"
            target = temp / "monorepo"
            output = temp / "review"
            upstream.mkdir()
            target.mkdir()
            self.git(upstream, "init", "-q")
            (upstream / "README.md").write_text("reviewed\n", encoding="utf-8")
            self.git(upstream, "add", "README.md")
            self.git(upstream, "commit", "-qm", "reviewed")
            reviewed = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
            (upstream / "workflow.yml").write_text("represented\n", encoding="utf-8")
            self.git(upstream, "add", "workflow.yml")
            self.git(upstream, "commit", "-qm", "represented update")
            (upstream / "config.toml").write_text("missing = true\n", encoding="utf-8")
            self.git(upstream, "add", "config.toml")
            self.git(upstream, "commit", "-qm", "missing update")
            candidate = self.git(upstream, "rev-parse", "HEAD").stdout.strip()
            self.git(target, "init", "-q")
            (target / "README.md").write_text("reviewed\n", encoding="utf-8")
            self.git(target, "add", "README.md")
            self.git(target, "commit", "-qm", "fork base")
            (target / "workflow.yml").write_text("represented\n", encoding="utf-8")
            self.git(target, "add", "workflow.yml")
            self.git(target, "commit", "-qm", "equivalent represented update")
            (target / "crw").mkdir()
            self.git(target, "mv", "README.md", "workflow.yml", "crw")
            self.git(target, "commit", "-qm", "import")
            data = manifest()
            data["sources"]["crwVendor"]["reviewedCommit"] = reviewed
            path = temp / "upstreams.json"
            path.write_text(json.dumps(data), encoding="utf-8")

            result = subprocess.run(
                [
                    sys.executable,
                    str(TOOL),
                    "--manifest",
                    str(path),
                    "prepare-crw",
                    "--source-repo",
                    str(upstream),
                    "--candidate",
                    candidate,
                    "--target-repo",
                    str(target),
                    "--output",
                    str(output),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
            )
            review = json.loads((output / "review.json").read_text(encoding="utf-8"))

        self.assertEqual(result.returncode, 1)
        self.assertEqual(review["applicability"], "conflict")



if __name__ == "__main__":
    unittest.main()
