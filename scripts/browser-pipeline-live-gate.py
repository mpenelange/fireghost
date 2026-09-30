#!/usr/bin/env python3
"""Validate opt-in browser pipeline REST and optional MCP results on public pages."""

import argparse
import hashlib
import ipaddress
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import urllib.parse
from datetime import datetime, timezone


ROOT = pathlib.Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from scripts.gate_http import request_bytes

DEFAULT_MATRIX = ROOT / "tests" / "fixtures" / "browser-pipeline-live-matrix.json"
MAX_RESPONSE_BYTES = 1048576
REDDIT_HOSTS = {"reddit.com", "www.reddit.com", "old.reddit.com", "new.reddit.com"}
STOP_REASONS = {"snapshot", "snapshotLimit", "maxItems", "maxBytes", "maxRounds", "noControls", "progressStalled", "deadline"}


def _url(value):
    if not isinstance(value, str):
        return None
    try:
        parsed = urllib.parse.urlsplit(value)
        if (parsed.scheme not in ("http", "https") or not parsed.hostname
                or parsed.username is not None or parsed.password is not None):
            return None
        parsed.port
        return parsed
    except ValueError:
        return None


def _safe_url(value):
    parsed = _url(value)
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, "", "")) if parsed else ""


def thread_id(value):
    parsed = _url(value)
    if not parsed or parsed.hostname not in REDDIT_HOSTS or parsed.port not in (None, 80, 443):
        return None
    matched = re.search(r"/comments/([a-z0-9]+)(?:/|$)", parsed.path, re.I)
    return matched.group(1).lower() if matched else None


def _natural(value, ceiling):
    return type(value) is int and 0 <= value <= ceiling


def _validate_matrix(matrix):
    if not isinstance(matrix, dict) or matrix.get("schemaVersion") != 1:
        raise ValueError("matrix requires schemaVersion 1")
    if not _natural(matrix.get("maxResponseBytes"), MAX_RESPONSE_BYTES) or matrix["maxResponseBytes"] == 0:
        raise ValueError("invalid bounded response limit")
    cases = matrix.get("cases")
    if not isinstance(cases, list) or not 1 <= len(cases) <= 12:
        raise ValueError("matrix requires one to twelve cases")
    names = set()
    for case in cases:
        if not isinstance(case, dict):
            raise ValueError("case must be an object")
        name = case.get("name")
        if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", name) or name in names:
            raise ValueError("case names must be unique bounded identifiers")
        names.add(name)
        payload = case.get("request")
        if not isinstance(payload, dict) or set(payload) != {"url", "profile", "timeout", "maxRounds", "maxItems", "maxBytes"}:
            raise ValueError("case requires the typed browser pipeline request")
        if payload["profile"] not in ("article", "redditThread"):
            raise ValueError("unsupported pipeline profile")
        parsed = _url(payload["url"])
        if not parsed or parsed.query or parsed.fragment or len(payload["url"]) > 2048:
            raise ValueError("case URL must be public HTTP(S) without credentials or query")
        host = parsed.hostname.rstrip(".")
        if host == "localhost" or host.endswith((".localhost", ".local")):
            raise ValueError("private targets are not live controls")
        try:
            address = ipaddress.ip_address(host)
        except ValueError:
            address = None
        if address is not None and not address.is_global:
            raise ValueError("private targets are not live controls")
        if payload["profile"] == "redditThread" and not thread_id(payload["url"]):
            raise ValueError("Reddit profile requires a public thread URL")
        for key, ceiling in {"timeout": 60000, "maxRounds": 100, "maxItems": 1000, "maxBytes": 262144}.items():
            if not _natural(payload[key], ceiling) or payload[key] == 0:
                raise ValueError("invalid typed pipeline budget")
        timeout = case.get("timeoutSeconds")
        if type(timeout) is not int or not 1 <= timeout <= 65 or timeout * 1000 < payload["timeout"]:
            raise ValueError("invalid bounded HTTP timeout")
        minimum = case.get("minimumMarkdownBytes")
        if not _natural(minimum, payload["maxBytes"]) or minimum == 0:
            raise ValueError("invalid Markdown floor")
        comments = case.get("minimumComments")
        if not _natural(comments, payload["maxItems"]):
            raise ValueError("invalid comment floor")
        if (payload["profile"] == "article" and comments != 0) or (payload["profile"] == "redditThread" and comments == 0):
            raise ValueError("comment floor must match the profile")


def _comment_checks(comments, expected_thread, reasons):
    ids = {}
    parents = {}
    known_count = parent_count = deleted_count = 0
    for comment in comments:
        if not isinstance(comment, dict):
            reasons.append("comment-shape")
            continue
        markdown = comment.get("markdown")
        if not isinstance(markdown, str) or not markdown.strip():
            reasons.append("comment-body")
            markdown = ""
        marker = markdown.strip().replace("\\[", "[").replace("\\]", "]").casefold()
        deleted = marker in ("[deleted]", "[removed]")
        identity = comment.get("id")
        valid_id = isinstance(identity, str) and re.fullmatch(r"t1_[a-z0-9]{1,64}", identity)
        if valid_id:
            known_count += 1
            if identity in ids:
                reasons.append("duplicate-comment-id")
            ids[identity] = comment
        elif identity is not None or not deleted:
            reasons.append("comment-id")
        else:
            deleted_count += 1
        parent = comment.get("parentId")
        valid_parent = isinstance(parent, str) and re.fullmatch(r"t[13]_[a-z0-9]{1,64}", parent)
        if valid_parent:
            parent_count += 1
            if parent.startswith("t3_") and parent != "t3_" + expected_thread:
                reasons.append("parent-thread-identity")
            if parent == identity:
                reasons.append("self-parent-link")
            if valid_id:
                parents[identity] = parent
        elif parent is not None or not deleted:
            reasons.append("comment-parent-link")
        if not _natural(comment.get("depth"), 256):
            reasons.append("comment-depth")
        permalink = comment.get("permalink")
        if permalink is not None and thread_id(permalink) != expected_thread:
            reasons.append("comment-permalink-thread")
    # A valid partial tree may refer to an unloaded parent. Verify relationships
    # among known nodes without pretending those external parents were loaded.
    for identity, parent in parents.items():
        if parent in ids:
            child_depth, parent_depth = ids[identity].get("depth"), ids[parent].get("depth")
            if _natural(child_depth, 256) and _natural(parent_depth, 256) and child_depth <= parent_depth:
                reasons.append("parent-depth-order")
        chain = set()
        current = identity
        while current in parents:
            if current in chain:
                reasons.append("parent-cycle")
                break
            chain.add(current)
            current = parents[current]
    if not known_count or not parent_count:
        reasons.append("no-known-comment-parent-evidence")
    return known_count, parent_count, deleted_count


def _validate_reply(reply, case):
    reasons = []
    payload = case["request"]
    profile = payload["profile"]
    if not isinstance(reply, dict) or reply.get("success") is not True:
        reasons.append("success-flag")
    data = reply.get("data") if isinstance(reply, dict) else None
    data = data if isinstance(data, dict) else {}
    metadata = data.get("metadata")
    metadata = metadata if isinstance(metadata, dict) else {}
    markdown = data.get("markdown")
    markdown = markdown if isinstance(markdown, str) else ""
    markdown_bytes = len(markdown.encode("utf-8"))
    if not markdown.strip() or markdown_bytes < case["minimumMarkdownBytes"]:
        reasons.append("markdown-floor")
    for key, expected in [("pipeline", "browser-v1"), ("profile", profile), ("renderedWith", "camofox"), ("sourceURL", payload["url"])]:
        if metadata.get(key) != expected:
            reasons.append("metadata-" + key)
    valid_final_url = (thread_id(metadata.get("url")) == thread_id(payload["url"])) if profile == "redditThread" else (_safe_url(metadata.get("url")) == payload["url"])
    if not valid_final_url:
        reasons.append("final-url-identity")
    if type(metadata.get("complete")) is not bool or (profile == "redditThread" and metadata.get("complete") is not False):
        reasons.append("truthful-completeness")
    if metadata.get("stopReason") not in STOP_REASONS:
        reasons.append("stop-reason")
    rounds, items, elapsed = metadata.get("rounds"), metadata.get("itemsCollected"), metadata.get("elapsedMs")
    if not _natural(rounds, payload["maxRounds"]) or rounds == 0:
        reasons.append("round-budget")
    if not _natural(items, payload["maxItems"]):
        reasons.append("item-budget")
    if not _natural(elapsed, payload["timeout"]):
        reasons.append("elapsed-budget")
    warnings = data.get("warnings")
    if not isinstance(warnings, list) or not all(isinstance(warning, str) for warning in warnings):
        reasons.append("warning-shape")
    content_bytes = markdown_bytes
    comments = []
    known_count = parent_count = deleted_count = 0
    if profile == "redditThread":
        structured = data.get("json")
        if not isinstance(structured, dict) or not isinstance(structured.get("comments"), list):
            reasons.append("structured-comments")
            structured = {}
        post = structured.get("post")
        if not isinstance(post, dict) or not isinstance(post.get("markdown"), str) or not post["markdown"].strip():
            reasons.append("structured-post")
        comments = structured.get("comments", [])
        content_bytes += len(json.dumps(structured, ensure_ascii=False, separators=(",", ":")).encode("utf-8"))
        if len(comments) < case["minimumComments"]:
            reasons.append("comment-floor")
        if len(comments) > payload["maxItems"] or items != len(comments):
            reasons.append("comment-count-budget")
        known_count, parent_count, deleted_count = _comment_checks(comments, thread_id(payload["url"]), reasons)
    elif items != 0:
        reasons.append("article-item-count")
    if content_bytes > payload["maxBytes"]:
        reasons.append("content-byte-budget")
    reported = metadata.get("reportedTotal")
    if reported is not None and not _natural(reported, 2**64 - 1):
        reasons.append("reported-total-shape")
    # Every recorded server value is normalized or admitted by a closed check.
    # Untrusted titles, warning/error strings, authors and bodies never escape.
    summary = {
        "markdown_bytes": markdown_bytes, "markdown_sha256": hashlib.sha256(markdown.encode("utf-8")).hexdigest(),
        "content_budget_bytes": content_bytes, "comments": len(comments),
        "known_comment_ids": known_count, "parent_links": parent_count,
        "nullable_deleted_ids": deleted_count, "warning_count": len(warnings) if isinstance(warnings, list) else 0,
        "metadata": {
            "pipeline": "browser-v1" if metadata.get("pipeline") == "browser-v1" else None,
            "profile": metadata.get("profile") if metadata.get("profile") in ("article", "redditThread") else None,
            "rendered_with": "camofox" if metadata.get("renderedWith") == "camofox" else None,
            "source_url": payload["url"] if metadata.get("sourceURL") == payload["url"] else None,
            "url": _safe_url(metadata.get("url")) if valid_final_url else None,
            "complete": metadata.get("complete") if type(metadata.get("complete")) is bool else None,
            "stop_reason": metadata.get("stopReason") if metadata.get("stopReason") in STOP_REASONS else None,
            "rounds": rounds if _natural(rounds, payload["maxRounds"]) else None,
            "items_collected": items if _natural(items, payload["maxItems"]) else None,
            "elapsed_ms": elapsed if _natural(elapsed, payload["timeout"]) else None,
            "reported_total": reported if _natural(reported, 2**64 - 1) else None,
        },
    }
    return summary, sorted(set(reasons))


def _reject_constant(_value):
    raise ValueError("invalid JSON constant")


def _run_case(case, endpoint, api_key, max_bytes, requester, transport):
    payload = case["request"]
    path = "/v2/browser/scrape"
    request_id = "browser-pipeline-live-article"
    if transport == "mcp":
        path = "/mcp"
        payload = {"jsonrpc": "2.0", "id": request_id, "method": "tools/call", "params": {"name": "browser_scrape", "arguments": payload}}
    result = {"name": case["name"], "transport": transport, "profile": case["request"]["profile"],
              "url": case["request"]["url"], "request_budgets": {key: case["request"][key] for key in ("timeout", "maxRounds", "maxItems", "maxBytes")},
              "passed": False, "reasons": [], "summary": {}}
    start = time.monotonic()
    try:
        status, raw = requester(endpoint, api_key, "POST", path, payload, case["timeoutSeconds"], max_bytes)
        result["status"] = status if type(status) is int and 100 <= status <= 599 else None
        if status != 200 or type(status) is not int:
            result["reasons"].append("http-not-200")
        if not isinstance(raw, bytes) or len(raw) > max_bytes:
            raise ValueError("invalid bounded response")
        result["response_bytes"] = len(raw)
        reply = json.loads(raw, parse_constant=_reject_constant)
        if transport == "mcp":
            envelope = reply if isinstance(reply, dict) else {}
            tool = envelope.get("result")
            tool = tool if isinstance(tool, dict) else {}
            if envelope.get("jsonrpc") != "2.0" or envelope.get("id") != request_id or envelope.get("error") is not None:
                result["reasons"].append("mcp-envelope")
            if tool.get("isError", False) is not False:
                result["reasons"].append("mcp-tool-error")
            reply = tool.get("structuredContent")
            if not isinstance(reply, dict):
                result["reasons"].append("mcp-structured-content")
        result["summary"], reasons = _validate_reply(reply, case)
        result["reasons"].extend(reasons)
    except Exception as exc:
        result["reasons"].append("request-or-contract-failed:" + type(exc).__name__)
    result["elapsed_seconds"] = round(time.monotonic() - start, 3)
    result["passed"] = not result["reasons"]
    return result


def _revision():
    try:
        return subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True, stderr=subprocess.DEVNULL).strip()
    except (OSError, subprocess.SubprocessError):
        return "unknown"


def run_gate(*, endpoint, api_key, output, matrix, include_mcp=False,
             requester=request_bytes, repo_revision=None):
    _validate_matrix(matrix)
    parsed = _url(endpoint)
    if not parsed or parsed.query or parsed.fragment:
        raise ValueError("router endpoint must be HTTP(S) without credentials or query")
    articles = [case for case in matrix["cases"] if case["request"]["profile"] == "article"]
    if include_mcp and not articles:
        raise ValueError("optional MCP check requires an article case")
    revision = repo_revision or _revision()
    if not isinstance(revision, str) or not re.fullmatch(r"[A-Za-z0-9._/-]{1,128}", revision):
        raise ValueError("invalid repository revision")
    output = pathlib.Path(output)
    if output.exists() or output.is_symlink():
        raise FileExistsError(output)
    output.parent.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as destination:
        artifact = {
            "schemaVersion": 1, "timestamp": datetime.now(timezone.utc).isoformat(),
            "repo_revision": revision, "router_url": _safe_url(endpoint),
            "gate_sha256": hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),
            "transport_sha256": hashlib.sha256((ROOT / "scripts" / "gate_http.py").read_bytes()).hexdigest(),
            "matrix_sha256": hashlib.sha256(json.dumps(matrix, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
            "scope": "live browser pipeline content and partial thread contract; no full-thread claim",
            "cases": [], "reasons": [],
        }
        for case in matrix["cases"]:
            artifact["cases"].append(_run_case(case, endpoint, api_key, matrix["maxResponseBytes"], requester, "rest"))
        if include_mcp:
            artifact["cases"].append(_run_case(articles[0], endpoint, api_key, matrix["maxResponseBytes"], requester, "mcp"))
        for case in artifact["cases"]:
            artifact["reasons"].extend(case["name"] + "/" + case["transport"] + ": " + reason for reason in case["reasons"])
        artifact["passed"] = not artifact["reasons"]
        json.dump(artifact, destination, indent=2, sort_keys=True)
        destination.write("\n")
    return artifact


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--router-url", required=True)
    parser.add_argument("--matrix", type=pathlib.Path, default=DEFAULT_MATRIX)
    parser.add_argument("--repo-revision")
    parser.add_argument("--include-mcp", action="store_true")
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args(argv)
    try:
        result = run_gate(endpoint=args.router_url, api_key=os.environ.get("ROUTER_API_KEY", ""), output=args.output, matrix=json.loads(args.matrix.read_text()), include_mcp=args.include_mcp, repo_revision=args.repo_revision)
    except (OSError, ValueError) as exc:
        print("Pipeline live gate configuration failed: " + type(exc).__name__)
        return 2
    print("Browser pipeline live " + ("PASS" if result["passed"] else "FAIL"))
    for reason in result["reasons"]:
        print("- " + reason)
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
