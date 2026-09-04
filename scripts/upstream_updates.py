#!/usr/bin/env python3
"""Discover and prepare upstream updates without changing deployment state."""

import argparse
import json
import re
import shlex
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
DEFAULT_MANIFEST = ROOT / "upstreams.json"
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")


class ManifestError(ValueError):
    pass


def load_manifest(path):
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ManifestError(str(error)) from error
    if data.get("schemaVersion") != 1:
        raise ManifestError("schemaVersion must be 1")
    sources = data.get("sources")
    images = data.get("images")
    if not isinstance(sources, dict) or not sources:
        raise ManifestError("sources must be a non-empty object")
    if not isinstance(images, dict) or not images:
        raise ManifestError("images must be a non-empty object")
    for name, source in sources.items():
        if not isinstance(source, dict):
            raise ManifestError(f"sources.{name} must be an object")
        for field in ("url", "ref", "reviewedCommit"):
            if not isinstance(source.get(field), str) or not source[field]:
                raise ManifestError(f"sources.{name}.{field} must be a non-empty string")
        if not source["ref"].startswith("refs/heads/"):
            raise ManifestError(f"sources.{name}.ref must name a branch head")
        if not COMMIT_RE.fullmatch(source["reviewedCommit"]):
            raise ManifestError(f"sources.{name}.reviewedCommit must be a full commit ID")
    for name, image in images.items():
        if not isinstance(image, dict):
            raise ManifestError(f"images.{name} must be an object")
        for field in ("source", "image", "reviewedDigest"):
            if not isinstance(image.get(field), str) or not image[field]:
                raise ManifestError(f"images.{name}.{field} must be a non-empty string")
        if "@" in image["image"] or ":" not in image["image"].rsplit("/", 1)[-1]:
            raise ManifestError(f"images.{name}.image must use a mutable tag")
        if not DIGEST_RE.fullmatch(image["reviewedDigest"]):
            raise ManifestError(f"images.{name}.reviewedDigest must be a sha256 digest")
    return data


def run(command):
    try:
        return subprocess.run(
            command, text=True, encoding="utf-8", errors="replace", capture_output=True
        )
    except OSError as error:
        return subprocess.CompletedProcess(command, 127, "", str(error))


def check_upstreams(data, git_command, image_command):
    noncurrent = False
    for name, source in data["sources"].items():
        result = run(
            shlex.split(git_command)
            + ["ls-remote", "--exit-code", source["url"], source["ref"]]
        )
        fields = result.stdout.split()
        observed = fields[0].lower() if result.returncode == 0 and fields else None
        if not observed or not COMMIT_RE.fullmatch(observed):
            state = "unavailable"
        elif observed == source["reviewedCommit"]:
            state = "current"
        else:
            state = "changed"
        noncurrent |= state != "current"
        detail = observed or (
            result.stderr.strip().splitlines()[-1]
            if result.stderr.strip()
            else "no result"
        )
        print(
            f"source {name}: {state} "
            f"(reviewed {source['reviewedCommit']}, observed {detail})"
        )
    for name, image in data["images"].items():
        result = run(shlex.split(image_command) + [image["image"]])
        match = re.search(
            r"^Digest:\s*(sha256:[0-9a-fA-F]{64})\s*$",
            result.stdout,
            re.IGNORECASE | re.MULTILINE,
        )
        observed = match.group(1).lower() if result.returncode == 0 and match else None
        if not observed:
            state = "unavailable"
        elif observed == image["reviewedDigest"]:
            state = "current"
        else:
            state = "changed"
        noncurrent |= state != "current"
        detail = observed or (
            result.stderr.strip().splitlines()[-1]
            if result.stderr.strip()
            else "no result"
        )
        print(
            f"image {name}: {state} "
            f"(reviewed {image['reviewedDigest']}, observed {detail})"
        )
    return 1 if noncurrent else 0


def git(repo, *args):
    return run(["git", "-C", str(repo), *args])


def stable_patch_ids(repo, revision):
    history = subprocess.run(
        [
            "git",
            "-C",
            str(repo),
            "log",
            "--no-merges",
            "--pretty=format:%H",
            "-p",
            "--no-ext-diff",
            revision,
            "--",
        ],
        capture_output=True,
    )
    if history.returncode != 0:
        return None
    patch_ids = subprocess.run(
        ["git", "-C", str(repo), "patch-id", "--stable"],
        input=history.stdout,
        capture_output=True,
    )
    if patch_ids.returncode != 0:
        return None
    return [line.split()[0] for line in patch_ids.stdout.splitlines()]


def prepare_crw(data, source_repo, candidate, target_repo, output):
    source = data["sources"].get("crwVendor")
    if not source or source.get("importPath") != "crw":
        raise ManifestError("sources.crwVendor.importPath must be crw")
    reviewed = source["reviewedCommit"]
    resolved = git(source_repo, "rev-parse", "--verify", f"{candidate}^{{commit}}")
    candidate_commit = resolved.stdout.strip().lower()
    if resolved.returncode or not COMMIT_RE.fullmatch(candidate_commit):
        raise ManifestError(f"candidate commit is unavailable: {candidate}")
    ancestry = git(source_repo, "merge-base", "--is-ancestor", reviewed, candidate_commit)
    if ancestry.returncode != 0:
        raise ManifestError("reviewed commit is not an ancestor of candidate")
    diff = git(
        source_repo,
        "diff",
        "--binary",
        "--full-index",
        "--src-prefix=a/crw/",
        "--dst-prefix=b/crw/",
        reviewed,
        candidate_commit,
        "--",
    )
    if diff.returncode != 0:
        raise ManifestError(diff.stderr.strip() or "could not create upstream patch")
    output.mkdir(parents=True, exist_ok=False)
    patch_path = output / "crw-fork.patch"
    patch_path.write_text(diff.stdout, encoding="utf-8")
    applicability = git(target_repo, "apply", "--check", str(patch_path))
    if applicability.returncode == 0:
        state = "clean"
    else:
        reverse_applicability = git(
            target_repo, "apply", "--reverse", "--check", str(patch_path)
        )
        already_integrated = reverse_applicability.returncode == 0
        if not already_integrated:
            target_candidate = git(
                target_repo, "rev-parse", "--verify", f"{candidate_commit}^{{commit}}"
            )
            target_ancestry = git(
                target_repo, "merge-base", "--is-ancestor", candidate_commit, "HEAD"
            )
            if target_candidate.returncode == 0 and target_ancestry.returncode == 0:
                already_integrated = True
            else:
                source_commits = git(
                    source_repo, "rev-list", "--no-merges", f"{reviewed}..{candidate_commit}"
                )
                commit_count = len(source_commits.stdout.splitlines())
                source_patch_ids = stable_patch_ids(
                    source_repo, f"{reviewed}..{candidate_commit}"
                )
                target_patch_ids = stable_patch_ids(target_repo, "HEAD")
                already_integrated = (
                    source_commits.returncode == 0
                    and commit_count > 0
                    and source_patch_ids is not None
                    and len(source_patch_ids) == commit_count
                    and target_patch_ids is not None
                    and set(source_patch_ids).issubset(target_patch_ids)
                )
        state = "already-integrated" if already_integrated else "conflict"
    review = {
        "source": source["url"],
        "reviewedCommit": reviewed,
        "candidateCommit": candidate_commit,
        "importPath": "crw",
        "applicability": state,
        "actionRequired": state != "already-integrated",
        "applyCommand": f"git apply --index {patch_path}",
    }
    (output / "review.json").write_text(
        json.dumps(review, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    if state == "conflict":
        print(applicability.stderr.strip(), file=sys.stderr)
        print(f"conflict: patch retained for review at {patch_path}", file=sys.stderr)
        return 1
    if state == "already-integrated":
        print(
            f"prepared: {patch_path} "
            "(applicability already-integrated; no action required; target unchanged)"
        )
        return 0
    print(f"prepared: {patch_path} (applicability clean; target unchanged)")
    return 0


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, default=DEFAULT_MANIFEST)
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("validate")
    check_parser = subparsers.add_parser("check")
    check_parser.add_argument("--git-command", default="git")
    check_parser.add_argument(
        "--image-command", default="docker buildx imagetools inspect"
    )
    prepare_parser = subparsers.add_parser("prepare-crw")
    prepare_parser.add_argument("--source-repo", type=Path, required=True)
    prepare_parser.add_argument("--candidate", required=True)
    prepare_parser.add_argument("--target-repo", type=Path, default=ROOT)
    prepare_parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        data = load_manifest(args.manifest)
    except ManifestError as error:
        print(f"invalid manifest: {error}", file=sys.stderr)
        return 2
    if args.command == "validate":
        print(f"valid: {len(data['sources'])} sources, {len(data['images'])} images")
    elif args.command == "check":
        return check_upstreams(data, args.git_command, args.image_command)
    elif args.command == "prepare-crw":
        try:
            return prepare_crw(
                data,
                args.source_repo,
                args.candidate,
                args.target_repo,
                args.output,
            )
        except (ManifestError, OSError) as error:
            print(f"could not prepare update: {error}", file=sys.stderr)
            return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
