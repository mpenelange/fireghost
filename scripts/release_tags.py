#!/usr/bin/env python3
"""Validate Fireghost release refs and protect mutable image aliases."""

import json
import re
import sys


TAG_PATTERN = re.compile(
    r"^fireghost-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$"
)
VERSION_PATTERN = re.compile(
    r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$"
)
USAGE = "expected fireghost-vMAJOR.MINOR.PATCH"
VERSION_USAGE = "existing image versions must use MAJOR.MINOR.PATCH"


def parse(ref: str) -> tuple[int, int, int] | None:
    match = TAG_PATTERN.fullmatch(ref)
    if not match:
        return None
    return int(match.group(1)), int(match.group(2)), int(match.group(3))


def parse_version(version: str) -> tuple[int, int, int] | None:
    match = VERSION_PATTERN.fullmatch(version)
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


def can_promote(current_ref: str, existing_versions: list[str]) -> bool | None:
    current = parse(current_ref)
    if current is None:
        return None
    parsed_existing: list[tuple[int, int, int]] = []
    for version in existing_versions:
        parsed = parse_version(version)
        if parsed is None:
            raise ValueError(VERSION_USAGE)
        parsed_existing.append(parsed)
    return not parsed_existing or current >= max(parsed_existing)


def is_missing_inspection(message: str) -> bool:
    normalized = message.casefold()
    if "manifest unknown" in normalized or "no such manifest" in normalized:
        return True
    return any(line.rstrip().endswith(": not found") for line in normalized.splitlines())


def main() -> int:
    if len(sys.argv) == 2 and sys.argv[1] == "--is-missing-inspection":
        return 0 if is_missing_inspection(sys.stdin.read()) else 1

    if len(sys.argv) >= 3 and sys.argv[1] == "--can-promote":
        try:
            promote = can_promote(sys.argv[2], sys.argv[3:])
        except ValueError as error:
            print(error, file=sys.stderr)
            return 2
        if promote is None:
            print(USAGE, file=sys.stderr)
            return 2
        print(json.dumps({"promote": promote}, sort_keys=True))
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
