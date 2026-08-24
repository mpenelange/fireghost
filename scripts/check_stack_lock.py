#!/usr/bin/env python3
"""Validate the known-good stack manifest and Compose image policy."""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
LOCK_PATH = ROOT / "ops" / "hermes-production.lock.json"
COMPOSE_PATH = ROOT / "docker-compose.yml"
SHA256 = re.compile(r"^sha256:[0-9a-f]{64}$")
REVISION = re.compile(r"^[0-9a-f]{40}$")


def fail(message: str) -> None:
    raise ValueError(message)


def require_digest(value: object, field: str) -> None:
    if not isinstance(value, str) or "@sha256:" not in value:
        fail(f"{field} must be pinned by repository digest")
    digest = value.rsplit("@", 1)[1]
    if not SHA256.fullmatch(digest):
        fail(f"{field} contains an invalid sha256 digest")


def main() -> int:
    lock = json.loads(LOCK_PATH.read_text())
    if lock.get("schemaVersion") != 1:
        fail("unsupported or missing schemaVersion")

    components = lock.get("components")
    if not isinstance(components, dict):
        fail("components must be an object")

    for name in ("crw", "camofox", "lightpanda"):
        component = components.get(name)
        if not isinstance(component, dict):
            fail(f"missing component: {name}")
        require_digest(component.get("image"), f"components.{name}.image")
        image_id = component.get("imageId")
        if not isinstance(image_id, str) or not SHA256.fullmatch(image_id):
            fail(f"components.{name}.imageId must be a sha256 image ID")

    for name, field in (
        ("webRetrieval", "revision"),
        ("router", "sourceRevision"),
        ("crw", "expectedSourceRevision"),
    ):
        value = components[name].get(field)
        if not isinstance(value, str) or not REVISION.fullmatch(value):
            fail(f"components.{name}.{field} must be a full Git revision")

    compose = COMPOSE_PATH.read_text()
    image_refs = re.findall(r"^\s+image:\s+([^\s#]+)", compose, re.MULTILINE)
    if not image_refs:
        fail("Compose file contains no image references")
    for image in image_refs:
        require_digest(image, f"Compose image {image}")

    expected_images = {
        components["camofox"]["image"],
        components["lightpanda"]["image"],
    }
    missing = sorted(expected_images.difference(image_refs))
    if missing:
        fail(f"Compose does not use known-good image(s): {missing}")

    print(
        f"PASS: {LOCK_PATH.relative_to(ROOT)} schema and "
        f"{len(image_refs)} digest-pinned Compose images"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        raise SystemExit(1) from error
