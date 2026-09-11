#!/usr/bin/env python3
"""Validate appliance release refs and decide monotonic alias promotion."""

import json
import re
import sys


PATTERN = re.compile(r"^appliance-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")
USAGE = "expected appliance-vMAJOR.MINOR.PATCH"


def parse(ref: str) -> tuple[int, int, int] | None:
    match = PATTERN.fullmatch(ref)
    if not match:
        return None
    return int(match.group(1)), int(match.group(2)), int(match.group(3))


def release_metadata(ref: str) -> dict[str, object] | None:
    parsed = parse(ref)
    if parsed is None:
        return None
    major, minor, patch = parsed
    version = f"{major}.{minor}.{patch}"
    minor_tag = f"{major}.{minor}"
    return {"minor": minor_tag, "tags": [version, minor_tag, "latest"], "version": version}


def promotion_status(current_ref: str, refs: list[str]) -> dict[str, bool] | None:
    current = parse(current_ref)
    if current is None:
        return None
    releases = [parsed for ref in refs if (parsed := parse(ref)) is not None]
    if current not in releases:
        releases.append(current)
    same_minor = [release for release in releases if release[:2] == current[:2]]
    return {
        "publish_latest": current == max(releases),
        "publish_minor": current == max(same_minor),
    }


def main() -> int:
    if len(sys.argv) >= 3 and sys.argv[1] == "--promotion-status":
        status = promotion_status(sys.argv[2], sys.argv[3:])
        if status is None:
            print(USAGE, file=sys.stderr)
            return 2
        print(json.dumps(status, sort_keys=True))
        return 0

    if len(sys.argv) != 2:
        print(USAGE, file=sys.stderr)
        return 2
    metadata = release_metadata(sys.argv[1])
    if metadata is None:
        print(USAGE, file=sys.stderr)
        return 2
    print(json.dumps(metadata, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
