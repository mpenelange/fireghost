#!/usr/bin/env python3
"""Validate the deployment lock against Compose and the public env template."""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SHA256 = re.compile(r"^sha256:[0-9a-f]{64}$")
REVISION = re.compile(r"^[0-9a-f]{40}$")


def fail(message: str) -> None:
    raise ValueError(message)


def require_object(value: object, field: str) -> dict:
    if not isinstance(value, dict):
        fail(f"{field} must be an object")
    return value


def require_digest(value: object, field: str) -> str:
    if not isinstance(value, str) or "@sha256:" not in value:
        fail(f"{field} must be pinned by repository digest")
    digest = value.rsplit("@", 1)[1]
    if not SHA256.fullmatch(digest):
        fail(f"{field} contains an invalid sha256 digest")
    return value


def require_image_id(value: object, field: str) -> None:
    if not isinstance(value, str) or not SHA256.fullmatch(value):
        fail(f"{field} must be a sha256 image ID")


def require_revision(value: object, field: str) -> None:
    if not isinstance(value, str) or not REVISION.fullmatch(value):
        fail(f"{field} must be a full Git revision")


def env_value(path: Path, name: str) -> str:
    values = [
        line.split("=", 1)[1]
        for line in path.read_text(encoding="utf-8").splitlines()
        if line.startswith(name + "=")
    ]
    if len(values) != 1 or not values[0]:
        fail(f"{path}: expected exactly one non-empty {name}")
    return values[0]


def validate(lock_path: Path, compose_path: Path, env_path: Path) -> None:
    lock = json.loads(lock_path.read_text(encoding="utf-8"))
    if lock.get("schemaVersion") != 1:
        fail("unsupported or missing schemaVersion")

    source = require_object(lock.get("source"), "source")
    for field in (
        "monorepoImportRevision",
        "crwRevision",
        "applianceRevision",
        "routerRevision",
    ):
        require_revision(source.get(field), f"source.{field}")

    components = require_object(lock.get("components"), "components")
    for name in ("router", "crw", "camofox", "lightpanda"):
        component = require_object(components.get(name), f"components.{name}")
        require_image_id(component.get("imageId"), f"components.{name}.imageId")

    router = components["router"]
    require_revision(router.get("sourceRevision"), "components.router.sourceRevision")
    if router.get("sourceRevision") != source["routerRevision"]:
        fail("components.router.sourceRevision must match source.routerRevision")

    crw = components["crw"]
    require_revision(crw.get("sourceRevision"), "components.crw.sourceRevision")
    if crw.get("sourceRevision") != source["crwRevision"]:
        fail("components.crw.sourceRevision must match source.crwRevision")

    for name in ("crw", "camofox", "lightpanda"):
        require_digest(components[name].get("image"), f"components.{name}.image")

    configured_crw = env_value(env_path, "CRW_IMAGE")
    if configured_crw != crw["image"]:
        fail("CRW_IMAGE does not match components.crw.image")

    compose = compose_path.read_text(encoding="utf-8")
    if "image: ${CRW_IMAGE:" not in compose:
        fail("Compose crw service must consume CRW_IMAGE")

    literal_images = re.findall(r"^\s+image:\s+([^\s#]+)", compose, re.MULTILINE)
    for image in literal_images:
        if image.startswith("${") or image.startswith("web-retrieval-router:"):
            continue
        require_digest(image, f"Compose image {image}")

    for name in ("camofox", "lightpanda"):
        image = components[name]["image"]
        if image not in literal_images:
            fail(f"Compose does not use components.{name}.image")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--lock", type=Path, default=ROOT / "deploy" / "stack.lock.json")
    parser.add_argument("--compose", type=Path, default=ROOT / "deploy" / "compose.yaml")
    parser.add_argument("--env", type=Path, default=ROOT / "deploy" / ".env.example")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    validate(args.lock, args.compose, args.env)
    print(
        "PASS: deployment lock matches CRW_IMAGE and digest-pinned "
        "Camofox/Lightpanda Compose inputs"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        raise SystemExit(1) from error
