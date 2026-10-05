#!/usr/bin/env python3
"""Build the release-page payload for a promoted appliance tag.

Prints JSON accepted unchanged by both the GitHub and Forgejo
`POST /repos/{owner}/{repo}/releases` APIs. The title is the first line of
the annotated tag message and the notes are the rest, followed by the
published image references so operators can pin exact digests.

Usage: release_page.py TAG   (env: VERSION, ROUTER_DIGEST, CRW_DIGEST,
                              IMAGE_NAMESPACE)
"""

import json
import os
import subprocess
import sys


def build_payload(tag, message, version, router_digest, crw_digest, namespace):
    lines = message.strip().splitlines()
    title = lines[0].strip() if lines and lines[0].strip() else tag
    notes = "\n".join(lines[1:]).strip()
    images = [
        "## Images",
        "",
        f"- `{namespace}/fireghost-router:{version}` (`{router_digest}`)",
        f"- `{namespace}/fireghost-crw:{version}` (`{crw_digest}`)",
    ]
    body = "\n\n".join(part for part in (notes, "\n".join(images)) if part)
    return {
        "tag_name": tag,
        "name": title,
        "body": body + "\n",
        "draft": False,
        "prerelease": "-" in version,
    }


def tag_message(tag):
    return subprocess.run(
        ["git", "tag", "-l", "--format=%(contents)", tag],
        check=True,
        capture_output=True,
        text=True,
    ).stdout


def main(argv):
    if len(argv) != 2:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    tag = argv[1]
    for name in ("VERSION", "ROUTER_DIGEST", "CRW_DIGEST"):
        if not os.environ.get(name):
            print(f"{name} is required", file=sys.stderr)
            return 2
    payload = build_payload(
        tag,
        tag_message(tag),
        os.environ["VERSION"],
        os.environ["ROUTER_DIGEST"],
        os.environ["CRW_DIGEST"],
        os.environ.get("IMAGE_NAMESPACE", "ghcr.io/mpenelange"),
    )
    json.dump(payload, sys.stdout)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
