#!/usr/bin/env python3
"""Check a pinned Camofox HTTP contract in a disposable validation runtime."""

import argparse
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import urllib.parse
import uuid
from datetime import datetime, timezone


ROOT = pathlib.Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from scripts.gate_http import HTTPBoundError, request_bytes

DEFAULT_FIXTURE = ROOT / "tests" / "fixtures" / "browser-http-contract.json"
MAX_RESPONSE_BYTES = 262144
MAX_REQUEST_BYTES = 65536


class ContractError(Exception):
    """A reviewed check identifier, never an arbitrary upstream error message."""


def _expect(condition, code):
    if not condition:
        raise ContractError(code)


def _version(value):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", value):
        raise ValueError("version must have three numeric components")
    return tuple(int(part) for part in value.split("."))


def _endpoint(value):
    parsed = urllib.parse.urlsplit(value)
    if (parsed.scheme not in ("http", "https") or not parsed.hostname
            or parsed.username is not None or parsed.password is not None
            or parsed.query or parsed.fragment or parsed.path not in ("", "/")):
        raise ValueError("browser URL must be an HTTP(S) origin without credentials or query")
    parsed.port  # Reject an invalid port before any request.
    return parsed


def _validate_fixture(fixture):
    if not isinstance(fixture, dict) or fixture.get("schemaVersion") != 1:
        raise ValueError("fixture must have schemaVersion 1")
    if fixture.get("browser") != "camofox" or fixture.get("cycles") != 2:
        raise ValueError("contract requires Camofox and two consecutive cycles")
    if fixture.get("createServerErrorRecovery") != "scopedResetOnce":
        raise ValueError("unsupported create recovery contract")
    _version(fixture.get("minimumVersion"))
    if fixture.get("requiredHealth") != {"ok": True, "running": True, "engine": "camoufox"}:
        raise ValueError("unsupported health contract")
    target = fixture.get("target", {})
    if target.get("url") != "https://example.com/":
        raise ValueError("contract target must remain the public example.com control")
    if target.get("requiredTitle") != "Example Domain":
        raise ValueError("contract target must require the exact Example Domain title")
    minimum = target.get("minimumTextChars")
    markers = target.get("requiredText")
    if (type(minimum) is not int or not 1 <= minimum <= 1000
            or not isinstance(markers, list) or not 1 <= len(markers) <= 10
            or not all(isinstance(marker, str) and 1 <= len(marker) <= 256 for marker in markers)):
        raise ValueError("invalid public target content checks")
    limits = {
        "requestTimeoutSeconds": 30, "totalTimeoutSeconds": 180,
        "cleanupTimeoutSeconds": 10, "maxResponseBytes": MAX_RESPONSE_BYTES,
        "maxExpressionBytes": MAX_REQUEST_BYTES,
    }
    budgets = fixture.get("budgets", {})
    if not isinstance(budgets, dict):
        raise ValueError("budgets must be an object")
    for key, ceiling in limits.items():
        value = budgets.get(key)
        if type(value) is not int or not 1 <= value <= ceiling:
            raise ValueError(f"invalid bounded budget: {key}")
    evaluations = fixture.get("evaluations")
    if (not isinstance(evaluations, list) or len(evaluations) != 2
            or not all(isinstance(item, dict) for item in evaluations)
            or {item.get("encoding") for item in evaluations} != {"object", "string"}):
        raise ValueError("contract requires object and string evaluation results")
    for item in evaluations:
        expression = item.get("expression")
        if (not isinstance(expression, str) or not expression
                or len(expression.encode("utf-8")) > budgets["maxExpressionBytes"]):
            raise ValueError("evaluation expression exceeds bounded budget")


def _request(endpoint, api_key, method, path, payload, timeout, max_response_bytes):
    """Decode successful contract replies; discard bounded error bodies."""
    _endpoint(endpoint)
    try:
        status, raw = request_bytes(endpoint, api_key, method, path, payload, timeout, max_response_bytes)
    except HTTPBoundError as exc:
        raise ContractError(str(exc)) from None
    if not 200 <= status < 300:
        return status, None
    try:
        return status, json.loads(raw)
    except (ValueError, UnicodeDecodeError):
        raise ContractError("invalid-json") from None


def _repo_revision():
    try:
        return subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True, stderr=subprocess.DEVNULL).strip()
    except (OSError, subprocess.SubprocessError):
        return "unknown"


def run_gate(*, endpoint, api_key, expected_version, image_reference, output, fixture,
             requester=_request, repo_revision=None):
    _validate_fixture(fixture)
    _endpoint(endpoint)
    if _version(expected_version) < _version(fixture["minimumVersion"]):
        raise ValueError("expected browser version is below the contract minimum")
    if not isinstance(image_reference, str) or not re.fullmatch(r"[A-Za-z0-9./_:-]+@sha256:[a-f0-9]{64}", image_reference):
        raise ValueError("image reference must include an immutable sha256 registry digest")
    revision = repo_revision or _repo_revision()
    if not isinstance(revision, str) or not re.fullmatch(r"[A-Za-z0-9._/-]{1,128}", revision):
        raise ValueError("invalid repository revision")
    output = pathlib.Path(output)
    if output.exists() or output.is_symlink():
        raise FileExistsError(output)
    budgets = fixture["budgets"]
    user = "crw-contract-" + uuid.uuid4().hex
    deadline = time.monotonic() + budgets["totalTimeoutSeconds"]
    artifact = {
        "schemaVersion": 1, "timestamp": datetime.now(timezone.utc).isoformat(),
        "scope_id": user, "browser_url": endpoint.rstrip("/"),
        "runtime_manifest": {
            "repo_revision": revision, "image_reference": image_reference,
            "image_reference_provenance": "operator-supplied; verify against container image",
            "expected_version": expected_version, "observed_version": None,
            "gate_sha256": hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),
            "transport_sha256": hashlib.sha256((ROOT / "scripts" / "gate_http.py").read_bytes()).hexdigest(),
            "fixture_sha256": hashlib.sha256(json.dumps(fixture, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest(),
        },
        "budgets": dict(budgets), "http_checks": [], "evaluations": [],
        "create_recoveries": 0, "cleanup": {"passed": False}, "reasons": [],
        "passed": False,
    }
    stage = "initialization"

    def request(method, path, payload=None, *, label, cleanup_deadline=None):
        nonlocal stage
        stage = label
        budget = min(budgets["requestTimeoutSeconds"], (cleanup_deadline or deadline) - time.monotonic())
        if budget <= 0:
            raise TimeoutError()
        start = time.monotonic()
        status, reply = requester(endpoint, api_key, method, path, payload, budget, budgets["maxResponseBytes"])
        artifact["http_checks"].append({"operation": label, "status": status, "latency_seconds": round(time.monotonic() - start, 3)})
        if time.monotonic() >= (cleanup_deadline or deadline):
            raise TimeoutError()
        return status, reply

    def successful(status):
        _expect(type(status) is int and 200 <= status < 300, f"http-{status}" if type(status) is int else "invalid-http-status")

    def acknowledged(status, reply):
        successful(status)
        _expect(isinstance(reply, dict) and reply.get("ok") is True, "missing-ok-acknowledgement")

    def listed(expected, *, label, cleanup_deadline=None):
        status, reply = request("GET", "/tabs?" + urllib.parse.urlencode({"userId": user}), label=label, cleanup_deadline=cleanup_deadline)
        successful(status)
        _expect(isinstance(reply, dict) and reply.get("running") is True and isinstance(reply.get("tabs"), list), "invalid-tab-list")
        tabs = reply["tabs"]
        _expect(all(isinstance(tab, dict) for tab in tabs), "invalid-tab-list")
        ids = [tab.get("tabId", tab.get("id")) for tab in tabs]
        _expect(ids == expected, "unexpected-scoped-tabs")

    def reset(*, label, cleanup_deadline=None):
        status, reply = request("DELETE", f"/sessions/{user}", {"userId": user}, label=label, cleanup_deadline=cleanup_deadline)
        acknowledged(status, reply)
        listed([], label=label + ".list", cleanup_deadline=cleanup_deadline)

    def reason(exc):
        code = str(exc) if isinstance(exc, ContractError) else type(exc).__name__
        return f"{stage}: {code}"

    try:
        status, health = request("GET", "/health", label="health")
        successful(status)
        _expect(isinstance(health, dict), "health.shape")
        for key, value in fixture["requiredHealth"].items():
            _expect(health.get(key) == value and type(health.get(key)) is type(value), f"health.{key}")
        observed = health.get("version")
        try:
            parsed_version = _version(observed)
        except ValueError:
            raise ContractError("health.version") from None
        artifact["runtime_manifest"]["observed_version"] = observed
        _expect(observed == expected_version and parsed_version >= _version(fixture["minimumVersion"]), "health.version")
        listed([], label="initial.list")
        for cycle in range(1, fixture["cycles"] + 1):
            prefix = f"cycle-{cycle}"
            status, created = request("POST", "/tabs", {"userId": user, "sessionKey": "contract"}, label=prefix + ".create")
            if type(status) is int and 500 <= status < 600:
                reset(label=prefix + ".create-reset")
                artifact["create_recoveries"] += 1
                status, created = request("POST", "/tabs", {"userId": user, "sessionKey": "contract"}, label=prefix + ".create-retry")
            successful(status)
            tab = created.get("tabId") if isinstance(created, dict) else None
            _expect(isinstance(tab, str) and re.fullmatch(r"[A-Za-z0-9_-]{1,128}", tab), "invalid-tab-id")
            listed([tab], label=prefix + ".created-list")
            status, navigated = request("POST", f"/tabs/{tab}/navigate", {"userId": user, "url": fixture["target"]["url"]}, label=prefix + ".navigate")
            acknowledged(status, navigated)
            _expect(navigated.get("url") == fixture["target"]["url"], "navigation-url-mismatch")
            status, ready = request("POST", f"/tabs/{tab}/wait", {"userId": user, "timeout": 5000}, label=prefix + ".wait")
            acknowledged(status, ready)
            _expect(ready.get("ready") is True, "readiness-not-confirmed")
            for evaluation in fixture["evaluations"]:
                encoding = evaluation["encoding"]
                status, reply = request("POST", f"/tabs/{tab}/evaluate", {"userId": user, "expression": evaluation["expression"], "timeout": 5000}, label=prefix + ".evaluate-" + encoding)
                successful(status)
                _expect(isinstance(reply, dict) and reply.get("ok") is not False and reply.get("truncated", False) is False, "evaluation-truncated-or-unsuccessful")
                snapshot = reply.get("result")
                if encoding == "string":
                    _expect(isinstance(snapshot, str), "evaluation-string-shape")
                    try:
                        snapshot = json.loads(snapshot)
                    except (ValueError, TypeError):
                        raise ContractError("evaluation-string-json") from None
                _expect(isinstance(snapshot, dict), "evaluation-object-shape")
                text = snapshot.get("text")
                _expect(snapshot.get("url") == fixture["target"]["url"] and isinstance(snapshot.get("title"), str), "evaluation-identity")
                _expect(snapshot["title"] == fixture["target"]["requiredTitle"], "evaluation-title")
                _expect(isinstance(text, str) and len(text) >= fixture["target"]["minimumTextChars"], "evaluation-content-size")
                _expect(all(marker.casefold() in text.casefold() for marker in fixture["target"]["requiredText"]), "evaluation-required-text")
                artifact["evaluations"].append({"cycle": cycle, "encoding": encoding, "text_chars": len(text), "text_sha256": hashlib.sha256(text.encode("utf-8")).hexdigest()})
            status, closed = request("DELETE", f"/tabs/{tab}", {"userId": user}, label=prefix + ".close")
            acknowledged(status, closed)
            listed([], label=prefix + ".closed-list")
        reset(label="session-reset")
    except Exception as exc:
        artifact["reasons"].append(reason(exc))
    finally:
        cleanup_deadline = time.monotonic() + budgets["cleanupTimeoutSeconds"]
        try:
            reset(label="cleanup", cleanup_deadline=cleanup_deadline)
            artifact["cleanup"]["passed"] = True
        except Exception as exc:
            artifact["reasons"].append(reason(exc))
    artifact["passed"] = not artifact["reasons"]
    output.parent.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as destination:
        json.dump(artifact, destination, indent=2, sort_keys=True)
        destination.write("\n")
    return artifact


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--browser-url", required=True)
    parser.add_argument("--expected-version", required=True)
    parser.add_argument("--image-reference", required=True)
    parser.add_argument("--repo-revision")
    parser.add_argument("--fixture", type=pathlib.Path, default=DEFAULT_FIXTURE)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args(argv)
    try:
        fixture = json.loads(args.fixture.read_text(encoding="utf-8"))
        result = run_gate(endpoint=args.browser_url, api_key=os.environ.get("CAMOFOX_API_KEY", ""), expected_version=args.expected_version, image_reference=args.image_reference, output=args.output, fixture=fixture, repo_revision=args.repo_revision)
    except (OSError, ValueError) as exc:
        print(f"Contract gate configuration failed: {type(exc).__name__}")
        return 2
    print("Browser HTTP contract " + ("PASS" if result["passed"] else "FAIL"))
    for failure in result["reasons"]:
        print(f"- {failure}")
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
