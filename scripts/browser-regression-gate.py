#!/usr/bin/env python3
"""Compare explicitly pinned browser scrapes on production and candidate routers."""

import argparse
import hashlib
import json
import os
import pathlib
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone


ROOT = pathlib.Path(__file__).resolve().parents[1]
DEFAULT_MATRIX = ROOT / "tests" / "fixtures" / "browser-regression-matrix.json"
PRODUCTION_URL = "http://127.0.0.1:33000"
CANDIDATE_URL = "http://127.0.0.1:33010"
RENDERERS = {"lightpanda", "camofox"}


def _safe_url(value):
    parsed = urllib.parse.urlsplit(str(value))
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        return ""
    host = parsed.hostname
    if parsed.port:
        host += f":{parsed.port}"
    return urllib.parse.urlunsplit((parsed.scheme, host, parsed.path, "", ""))


def _repo_head():
    try:
        return subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True,
            stderr=subprocess.DEVNULL,
        ).strip()
    except (OSError, subprocess.SubprocessError):
        return "unknown"


def _validate_matrix(matrix):
    if matrix.get("schemaVersion") != 1 or not isinstance(matrix.get("cases"), list):
        raise ValueError("matrix must have schemaVersion 1 and a cases array")
    if not matrix["cases"]:
        raise ValueError("matrix must contain at least one case")
    names = set()
    for case in matrix["cases"]:
        name = case.get("name")
        if not isinstance(name, str) or not name or name in names:
            raise ValueError("every matrix case needs a unique nonempty name")
        names.add(name)
        if case.get("renderer") not in RENDERERS:
            raise ValueError(f"{name}: renderer must be lightpanda or camofox")
        if not _safe_url(case.get("url", "")):
            raise ValueError(f"{name}: url must be HTTP or HTTPS")
        if not isinstance(case.get("minimumMarkdownChars"), int) or case["minimumMarkdownChars"] < 1:
            raise ValueError(f"{name}: minimumMarkdownChars must be positive")
        ratio = case.get("minimumCandidateRatio")
        if not isinstance(ratio, (int, float)) or not 0 <= ratio <= 1:
            raise ValueError(f"{name}: minimumCandidateRatio must be between 0 and 1")
        for key in ("requiredText", "forbiddenText"):
            if not isinstance(case.get(key, []), list) or not all(
                isinstance(value, str) and value for value in case.get(key, [])
            ):
                raise ValueError(f"{name}: {key} must be an array of nonempty strings")
        if not isinstance(case.get("informational", False), bool):
            raise ValueError(f"{name}: informational must be true or false")


def _request(endpoint, api_key, payload, timeout):
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    headers = {"Content-Type": "application/json"}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    request = urllib.request.Request(
        endpoint.rstrip("/") + "/v2/scrape", data=body, headers=headers, method="POST"
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8"))


def _summary(reply, elapsed):
    data = reply.get("data") if isinstance(reply, dict) else None
    data = data if isinstance(data, dict) else {}
    metadata = data.get("metadata")
    metadata = metadata if isinstance(metadata, dict) else {}
    markdown = data.get("markdown") if isinstance(data.get("markdown"), str) else ""
    return {
        "success": reply.get("success") is True if isinstance(reply, dict) else False,
        "title": str(metadata.get("title") or "")[:256],
        "status_code": metadata.get("statusCode"),
        "rendered_with": metadata.get("renderedWith"),
        "markdown_chars": len(markdown),
        "markdown_sha256": hashlib.sha256(markdown.encode("utf-8")).hexdigest(),
        "latency_seconds": round(elapsed, 3),
    }, markdown


def _semantic_reasons(case, identity, summary, markdown):
    prefix = f'{case["name"]}: {identity}'
    reasons = []
    if not summary["success"]:
        reasons.append(f"{prefix} scrape was unsuccessful")
    if summary["status_code"] != 200:
        reasons.append(f'{prefix} reported origin status {summary["status_code"]}; expected 200')
    if summary["rendered_with"] != case["renderer"]:
        reasons.append(
            f'{prefix} used {summary["rendered_with"] or "no reported renderer"}; '
            f'expected {case["renderer"]}'
        )
    if summary["markdown_chars"] < case["minimumMarkdownChars"]:
        reasons.append(f"{prefix} markdown is too small")
    folded = markdown.casefold()
    for required in case.get("requiredText", []):
        if required.casefold() not in folded:
            reasons.append(f"{prefix} is missing required text")
    for forbidden in case.get("forbiddenText", []):
        if forbidden.casefold() in folded:
            reasons.append(f"{prefix} contains forbidden text")
    return reasons


def _failure(case, identity, exc):
    if isinstance(exc, urllib.error.HTTPError):
        return f'{case["name"]}: {identity} returned HTTP {exc.code}'
    if isinstance(exc, TimeoutError):
        return f'{case["name"]}: {identity} timed out'
    return f'{case["name"]}: {identity} failed: {type(exc).__name__}'


def run_gate(*, production_url, candidate_url, api_key, output, matrix,
             requester=_request, repo_revision=None):
    _validate_matrix(matrix)
    output = pathlib.Path(output)
    if output.exists() or output.is_symlink():
        raise FileExistsError(output)
    artifact = {
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "repo_head": repo_revision or _repo_head(),
        "endpoints": {
            "production": _safe_url(production_url),
            "candidate": _safe_url(candidate_url),
        },
        "matrix_schema_version": matrix["schemaVersion"],
        "cases": [],
        "checks": {},
        "reasons": [],
        "informational": [],
    }
    for case in matrix["cases"]:
        record = {
            "name": case["name"],
            "informational": case.get("informational", False),
            "renderer": case["renderer"],
            "url": _safe_url(case["url"]),
            "thresholds": {
                "minimum_markdown_chars": case["minimumMarkdownChars"],
                "minimum_candidate_ratio": case["minimumCandidateRatio"],
            },
        }
        checks = {identity: {"passed": False, "reasons": []}
                  for identity in ("production", "candidate", "comparison")}
        record["checks"] = checks
        raw = {}
        timeout = case.get("timeoutMs", 60000) / 1000
        payload = {
            "url": case["url"], "formats": ["markdown"],
            "renderer": case["renderer"], "timeout": case.get("timeoutMs", 60000),
        }
        for identity, endpoint in (
            ("production", production_url), ("candidate", candidate_url)
        ):
            started = time.monotonic()
            try:
                reply = requester(endpoint, api_key, payload.copy(), timeout)
                summary, markdown = _summary(reply, time.monotonic() - started)
                record[identity] = summary
                raw[identity] = markdown
                checks[identity]["reasons"].extend(
                    _semantic_reasons(case, identity, summary, markdown))
            except Exception as exc:
                record[identity] = {"error": type(exc).__name__}
                checks[identity]["reasons"].append(_failure(case, identity, exc))
            checks[identity]["passed"] = not checks[identity]["reasons"]
        checks["comparison"]["baseline_valid"] = checks["production"]["passed"]
        comparison_reasons = checks["comparison"]["reasons"]
        if "production" in raw and "candidate" in raw:
            production_size = len(raw["production"])
            ratio = len(raw["candidate"]) / production_size if production_size else 0.0
            record["comparison"] = {"markdown_size_ratio": round(ratio, 4)}
            if ratio < case["minimumCandidateRatio"]:
                comparison_reasons.append(
                    f'{case["name"]}: candidate markdown size ratio {ratio:.3f} is below threshold'
                )
        else:
            comparison_reasons.append(f'{case["name"]}: markdown size comparison unavailable')
        if not checks["production"]["passed"]:
            comparison_reasons.append(
                f'{case["name"]}: production checks failed; size comparison has no valid baseline'
            )
        checks["comparison"]["passed"] = not comparison_reasons
        # Informational cases are recorded in full but cannot fail the gate.
        destination = artifact["informational"] if record["informational"] else artifact["reasons"]
        for check in checks.values():
            destination.extend(check["reasons"])
        artifact["cases"].append(record)
    decisive = [case for case in artifact["cases"] if not case["informational"]]
    artifact["checks"] = {
        identity: {
            "passed": all(case["checks"][identity]["passed"] for case in decisive),
            "reasons": [reason for case in decisive
                        for reason in case["checks"][identity]["reasons"]],
        }
        for identity in ("production", "candidate", "comparison")
    }
    artifact["passed"] = not artifact["reasons"]
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("x", encoding="utf-8") as handle:
        handle.write(json.dumps(artifact, indent=2) + "\n")
    return artifact


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--production-url", default=PRODUCTION_URL)
    parser.add_argument("--candidate-url", default=CANDIDATE_URL)
    parser.add_argument("--matrix", type=pathlib.Path, default=DEFAULT_MATRIX)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    matrix = json.loads(args.matrix.read_text(encoding="utf-8"))
    artifact = run_gate(
        production_url=args.production_url,
        candidate_url=args.candidate_url,
        api_key=os.environ.get("FIRECRAWL_API_KEY", ""),
        output=args.output,
        matrix=matrix,
    )
    print(f"Browser regression gate: {'PASS' if artifact['passed'] else 'FAIL'}; artifact={args.output}")
    for identity, check in artifact["checks"].items():
        print(f"{identity.capitalize()} checks: {'PASS' if check['passed'] else 'FAIL'}")
    for reason in artifact["reasons"]:
        print(f"- {reason}")
    for note in artifact["informational"]:
        print(f"- informational: {note}")
    raise SystemExit(0 if artifact["passed"] else 1)


if __name__ == "__main__":
    main()
