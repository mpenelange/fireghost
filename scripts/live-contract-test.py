#!/usr/bin/env python3
"""Bounded live contract test for a running local appliance.

This never configures or invokes Firecrawl Cloud. It exercises local search,
scrape, persistent cache, and four distinct concurrent searches.
"""
from __future__ import annotations

import concurrent.futures
import json
import os
from pathlib import Path
import sys
import time
import urllib.error
import urllib.request

BASE_URL = os.environ.get("ROUTER_URL", "http://127.0.0.1:33000").rstrip("/")
REPO_ROOT = Path(__file__).resolve().parents[1]


def env_file_value(name: str) -> str:
    path = REPO_ROOT / "dev" / ".env"
    if not path.exists():
        return ""
    for raw in path.read_text(encoding="utf-8").splitlines():
        if raw.startswith(name + "="):
            return raw.split("=", 1)[1]
    return ""


API_KEY = os.environ.get("ROUTER_API_KEY") or env_file_value("ROUTER_API_KEY")
# Reddit serves a JavaScript challenge instead of content to datacenter egress
# such as GitHub-hosted runners. Hosted CI opts out; local and staging gates,
# which run from the appliance's own network, keep the anti-bot check.
SCRAPE_REDDIT = os.environ.get("LIVE_CONTRACT_REDDIT", "1") != "0"


def post(path: str, body: dict, timeout: float = 120.0) -> tuple[int, float, dict]:
    headers = {"Content-Type": "application/json"}
    if API_KEY:
        headers["Authorization"] = "Bearer " + API_KEY
    request = urllib.request.Request(
        BASE_URL + path,
        data=json.dumps(body, separators=(",", ":")).encode(),
        headers=headers,
        method="POST",
    )
    started = time.monotonic()
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            status, payload = response.status, response.read()
    except urllib.error.HTTPError as exc:
        status, payload = exc.code, exc.read()
    elapsed = time.monotonic() - started
    try:
        decoded = json.loads(payload)
    except json.JSONDecodeError as exc:
        raise AssertionError(f"{path} returned non-JSON status={status}") from exc
    return status, elapsed, decoded


def require_success(label: str, result: tuple[int, float, dict]) -> tuple[float, dict]:
    status, elapsed, body = result
    if status != 200 or body.get("success") is not True:
        raise AssertionError(f"{label}: status={status}, body={body!r}")
    return elapsed, body


def main() -> int:
    first_elapsed, first = require_success(
        "search first",
        post(
            "/v2/search",
            {"query": "Hermes mythology", "limit": 5, "engines": ["wikipedia"]},
        ),
    )
    second_elapsed, second = require_success(
        "search cached",
        post(
            "/v2/search",
            {"engines": ["wikipedia"], "limit": 5, "query": "Hermes mythology"},
        ),
    )
    first_web = first.get("data", {}).get("web", [])
    second_web = second.get("data", {}).get("web", [])
    if not first_web or first_web != second_web:
        raise AssertionError("search cache did not preserve the successful result")

    example_elapsed, example = require_success(
        "scrape example.com",
        post("/v2/scrape", {"url": "https://example.com", "formats": ["markdown"]}),
    )
    example_markdown = example.get("data", {}).get("markdown", "")
    if len(example_markdown) < 100:
        raise AssertionError(
            f"example.com scrape markdown was unexpectedly incomplete "
            f"({len(example_markdown)} chars)"
        )
    reddit_elapsed, reddit_markdown = None, None
    if SCRAPE_REDDIT:
        reddit_elapsed, reddit = require_success(
            "scrape reddit",
            post(
                "/v2/scrape",
                {"url": "https://www.reddit.com/r/selfhosted/", "formats": ["markdown"]},
            ),
        )
        reddit_markdown = reddit.get("data", {}).get("markdown", "")
        if len(reddit_markdown) < 1_000:
            raise AssertionError(
                f"reddit scrape markdown was unexpectedly incomplete "
                f"({len(reddit_markdown)} chars): {reddit_markdown[:200]!r}"
            )

    queries = [
        "Linux",
        "Chicago",
        "Rust programming language",
        "Docker software",
    ]
    wall_started = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as executor:
        futures = {
            query: executor.submit(
                post,
                "/v2/search",
                {"query": query, "limit": 5, "engines": ["wikipedia"]},
            )
            for query in queries
        }
        concurrent_rows = []
        nonempty_results = 0
        for query, future in futures.items():
            elapsed, body = require_success("concurrent " + query, future.result())
            result_count = len(body.get("data", {}).get("web", []))
            nonempty_results += result_count > 0
            concurrent_rows.append((query, elapsed, result_count))
    concurrent_wall = time.monotonic() - wall_started
    # A live public search engine can legitimately return an empty SERP for one
    # query. Require a strong majority while still proving all four requests
    # completed successfully through independent workers.
    if nonempty_results < 3:
        raise AssertionError(
            f"only {nonempty_results} of four concurrent searches returned results"
        )

    print(
        json.dumps(
            {
                "search": {
                    "firstSeconds": round(first_elapsed, 3),
                    "cachedSeconds": round(second_elapsed, 3),
                    "results": len(first_web),
                },
                "scrape": {
                    "exampleSeconds": round(example_elapsed, 3),
                    "exampleChars": len(example_markdown),
                    "redditSeconds": (
                        round(reddit_elapsed, 3) if reddit_elapsed is not None else None
                    ),
                    "redditChars": (
                        len(reddit_markdown) if reddit_markdown is not None else None
                    ),
                },
                "concurrency": {
                    "wallSeconds": round(concurrent_wall, 3),
                    "requests": [
                        {
                            "query": query,
                            "seconds": round(elapsed, 3),
                            "results": result_count,
                        }
                        for query, elapsed, result_count in concurrent_rows
                    ],
                },
            },
            indent=2,
        )
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"live contract failed: {exc}", file=sys.stderr)
        raise
