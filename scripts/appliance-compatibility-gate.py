#!/usr/bin/env python3
"""Compare legacy REST behavior on isolated copies of baseline and candidate.

This gate tolerates additive response fields and old metadata without renderer
provenance. It proves REST content compatibility, not renderer identity; use
browser-regression-gate.py separately for browser-specific approval.
"""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.parse

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from scripts.gate_http import request_bytes

MAX_RESPONSE_BYTES = 16 * 1024 * 1024


def safe_url(value):
    try:
        url = urllib.parse.urlsplit(value)
        if url.scheme not in ("http", "https") or not url.hostname or url.username or url.password:
            return ""
        return urllib.parse.urlunsplit((url.scheme, url.netloc, url.path, "", ""))
    except ValueError:
        return ""


def request(endpoint, key, path, body, timeout):
    status, raw = request_bytes(endpoint, key, "POST" if body is not None else "GET",
        path, body, timeout, MAX_RESPONSE_BYTES)
    return status, raw.decode("utf-8") if path == "/metrics" else json.loads(raw)


def validate_matrix(matrix):
    if matrix.get("schemaVersion") != 1 or not isinstance(matrix.get("cases"), list) or not matrix["cases"]:
        raise ValueError("expected schemaVersion 1 and nonempty cases")
    names = set()
    for case in matrix["cases"]:
        name = case.get("name")
        if not isinstance(name, str) or not re.fullmatch(r"[a-zA-Z0-9_-]{1,96}", name) or name in names:
            raise ValueError("case names must be unique bounded identifiers")
        names.add(name)
        kind = case.get("kind")
        if kind not in ("search", "scrape") or case.get("path") != "/v2/" + kind:
            raise ValueError("case must use the matching legacy REST path")
        if not isinstance(case.get("body"), dict) or not 0 < case.get("timeoutSeconds", 0) <= 125:
            raise ValueError("case requires a request object and bounded timeout")
        if kind == "scrape":
            if not safe_url(case["body"].get("url", "")) or case.get("minimumContentChars", 0) < 1:
                raise ValueError("scrape requires a public URL and content floor")
            ratio = case.get("minimumCandidateRatio")
            markers = case.get("requiredText", [])
            if not isinstance(markers, list) or not all(isinstance(item, str) and item for item in markers):
                raise ValueError("requiredText must contain nonempty strings")
        else:
            if not isinstance(case["body"].get("query"), str) or not case["body"]["query"].strip() or case.get("minimumResults", 0) < 1:
                raise ValueError("search requires a query and result floor")
            ratio = case.get("minimumTitleOverlap")
        if not isinstance(ratio, (int, float)) or not 0 <= ratio <= 1:
            raise ValueError("comparison ratio must be between zero and one")


def probe(case, endpoint, key, requester):
    start = time.monotonic()
    record = {"passed": False, "reasons": []}
    content, titles = "", set()
    try:
        status, reply = requester(endpoint, key, case["path"], case["body"], case["timeoutSeconds"])
        record["status"] = status
        if status != 200 or not isinstance(reply, dict) or reply.get("success") is not True:
            record["reasons"].append("unsuccessful response")
        data = reply.get("data") if isinstance(reply, dict) else None
        data = data if isinstance(data, dict) else {}
        if case["kind"] == "scrape":
            content = data.get("markdown") if isinstance(data.get("markdown"), str) else ""
            metadata = data.get("metadata")
            metadata = metadata if isinstance(metadata, dict) else {}
            origin_status = metadata.get("statusCode")
            if type(origin_status) is not int or not 100 <= origin_status <= 599:
                origin_status = None
            record.update(contentChars=len(content), contentSHA256=hashlib.sha256(content.encode()).hexdigest(), originStatus=origin_status)
            if record["originStatus"] != 200:
                record["reasons"].append("origin status is not 200")
            if len(content) < case["minimumContentChars"]:
                record["reasons"].append("content below minimum size")
            if any(marker.casefold() not in content.casefold() for marker in case.get("requiredText", [])):
                record["reasons"].append("required content missing")
        else:
            rows = data.get("web", [])
            rows = rows if isinstance(rows, list) else []
            for row in rows:
                if not isinstance(row, dict) or not isinstance(row.get("title"), str) or not row["title"].strip() or not safe_url(row.get("url", "")):
                    record["reasons"].append("invalid search result fields")
                    continue
                titles.add(row["title"].strip().casefold())
            record["resultCount"] = len(rows)
            if len(rows) < case["minimumResults"]:
                record["reasons"].append("too few search results")
        record["passed"] = not record["reasons"]
    except Exception as error:
        record["reasons"].append("request failed: " + type(error).__name__)
    record["elapsedSeconds"] = round(time.monotonic() - start, 3)
    return record, content, titles


def pair(case, phase, baseline_url, candidate_url, key, requester):
    baseline, before_content, before_titles = probe(case, baseline_url, key, requester)
    candidate, after_content, after_titles = probe(case, candidate_url, key, requester)
    comparison = {"baselineValid": baseline["passed"], "passed": False, "reasons": []}
    if not baseline["passed"]:
        comparison["reasons"].append("comparison unavailable: invalid baseline")
    elif not candidate["passed"]:
        comparison["reasons"].append("candidate contract failed")
    else:
        if case["kind"] == "scrape":
            ratio = len(after_content) / max(1, len(before_content))
            minimum = case["minimumCandidateRatio"]
        else:
            ratio = len(before_titles & after_titles) / max(1, len(before_titles))
            minimum = case["minimumTitleOverlap"]
        comparison["ratio"] = round(ratio, 4)
        if ratio < minimum:
            comparison["reasons"].append("candidate comparison ratio below threshold")
        comparison["passed"] = not comparison["reasons"]
    return {"name": case["name"], "kind": case["kind"], "phase": phase,
        "baseline": baseline, "candidate": candidate, "comparison": comparison}


def cloud_metrics(endpoint, key, requester):
    status, text = requester(endpoint, key, "/metrics", None, 10)
    if status != 200 or not isinstance(text, str):
        raise ValueError("metrics unavailable")
    values = {}
    for endpoint_name in ("search", "scrape"):
        matched = re.search(r'^web_retrieval_cloud_attempts_total\{endpoint="' + endpoint_name + r'"\}\s+([0-9]+)\s*$', text, re.M)
        if not matched:
            raise ValueError("cloud attempt metrics missing")
        values[endpoint_name] = int(matched.group(1))
        if values[endpoint_name] != 0:
            raise ValueError("isolated comparison must have zero cloud attempts")
    return values


def run_gate(*, baseline_url, candidate_url, api_key, output, matrix, requester=request, repo_revision=None):
    validate_matrix(matrix)
    if not safe_url(baseline_url) or not safe_url(candidate_url) or baseline_url.rstrip("/") == candidate_url.rstrip("/"):
        raise ValueError("use distinct HTTP(S) endpoints without credentials")
    output = Path(output)
    # Reserve exclusively before network work; existing evidence is never overwritten.
    with output.open("x", encoding="utf-8") as artifact_file:
        result = {"schemaVersion": 1, "repoRevision": repo_revision or subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
            "gateSHA256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            "matrixSHA256": hashlib.sha256(json.dumps(matrix, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
            "endpoints": {"baseline": safe_url(baseline_url), "candidate": safe_url(candidate_url)},
            "scope": "legacy REST content compatibility; not renderer identity or full-thread completeness",
            "cases": [], "metrics": {}, "reasons": []}
        for identity, endpoint in [("baseline", baseline_url), ("candidate", candidate_url)]:
            try:
                result["metrics"][identity + "Before"] = cloud_metrics(endpoint, api_key, requester)
            except Exception as error:
                result["reasons"].append(identity + " metrics failed: " + type(error).__name__)
        for phase in ("cold", "warm"):
            for case in matrix["cases"]:
                result["cases"].append(pair(case, phase, baseline_url, candidate_url, api_key, requester))
        cases = [case for case in matrix["cases"] if case.get("concurrent")]
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            result["cases"].extend(pool.map(lambda case: pair(case, "concurrent", baseline_url, candidate_url, api_key, requester), cases))
        for identity, endpoint in [("baseline", baseline_url), ("candidate", candidate_url)]:
            try:
                result["metrics"][identity + "After"] = cloud_metrics(endpoint, api_key, requester)
            except Exception as error:
                result["reasons"].append(identity + " metrics failed: " + type(error).__name__)
        for row in result["cases"]:
            for scope in ("baseline", "candidate", "comparison"):
                result["reasons"].extend(row["phase"] + "/" + row["name"] + "/" + scope + ": " + reason for reason in row[scope]["reasons"])
        result["passed"] = not result["reasons"]
        json.dump(result, artifact_file, indent=2)
        artifact_file.write("\n")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline-url", required=True)
    parser.add_argument("--candidate-url", required=True)
    parser.add_argument("--matrix", type=Path, default=ROOT / "tests/fixtures/appliance-compatibility-matrix.json")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    result = run_gate(baseline_url=args.baseline_url, candidate_url=args.candidate_url,
        api_key=os.environ.get("ROUTER_API_KEY", ""), output=args.output, matrix=json.loads(args.matrix.read_text()))
    print(json.dumps({"passed": result["passed"], "reasons": result["reasons"]}))
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
