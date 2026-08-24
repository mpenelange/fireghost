#!/usr/bin/env python3
"""Live scrape compatibility gate for an isolated CRW candidate stack.

The cases capture the production MVP's useful behavior plus one correctness
invariant that the original smoke transcript exposed: a browser fallback must
not turn a directly observed origin 404 into a synthetic Camofox 200.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import sys
import time
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class Case:
    name: str
    url: str
    expect_success: bool
    expected_status: int | None = None
    needle: str | None = None
    min_markdown_chars: int = 0
    error_contains: str | None = None
    canonical_https: bool = False


@dataclass
class Result:
    case: str
    url: str
    ok: bool
    elapsed_seconds: float
    http_status: int | None = None
    api_success: bool | None = None
    origin_status: int | None = None
    rendered_with: str | None = None
    markdown_chars: int | None = None
    error: str | None = None


CASES = (
    Case("static", "https://example.com", True, 200, "Example Domain", 100),
    Case("dynamic", "https://www.reddit.com/r/rust/", True, 200, "r/rust", 500),
    Case(
        "redirect",
        "http://github.com",
        True,
        200,
        "GitHub",
        500,
        canonical_https=True,
    ),
    Case(
        "pdf",
        "https://www.w3.org/WAI/ER/tests/xhtml/testfiles/resources/pdf/dummy.pdf",
        True,
        200,
        "Dummy PDF",
        10,
    ),
    Case(
        "protected",
        "https://www.g2.com/products/cloudflare/reviews",
        False,
        error_contains="anti-bot",
    ),
    Case(
        "origin_404",
        "https://example.com/this-path-must-not-exist-crw-compatibility",
        False,
        expected_status=404,
    ),
)


class Client:
    def __init__(self, base_url: str, api_key: str | None, timeout: float):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def request(self, path: str, body: dict[str, Any]) -> tuple[int, Any]:
        headers = {"Accept": "application/json", "Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        request = urllib.request.Request(
            f"{self.base_url}{path}",
            data=json.dumps(body).encode(),
            headers=headers,
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                status = response.status
                raw = response.read().decode()
        except urllib.error.HTTPError as error:
            status = error.code
            raw = error.read().decode(errors="replace")
        except (urllib.error.URLError, TimeoutError) as error:
            raise RuntimeError(f"transport failure: {error}") from error
        try:
            return status, json.loads(raw)
        except json.JSONDecodeError as error:
            raise RuntimeError(f"non-JSON response (HTTP {status}): {raw[:200]!r}") from error

    def scrape(self, case: Case) -> Result:
        started = time.monotonic()
        try:
            http_status, payload = self.request(
                "/v1/scrape", {"url": case.url, "formats": ["markdown"]}
            )
            if not isinstance(payload, dict):
                raise RuntimeError(f"response is not an object: {payload!r}")
            api_success = payload.get("success")
            if api_success is not case.expect_success:
                raise RuntimeError(
                    f"expected success={case.expect_success}, got {api_success!r}"
                )
            data = payload.get("data") or {}
            if not isinstance(data, dict):
                raise RuntimeError(f"response data is not an object: {data!r}")
            metadata = data.get("metadata") or {}
            if not isinstance(metadata, dict):
                raise RuntimeError(f"metadata is not an object: {metadata!r}")
            origin_status = metadata.get("statusCode")
            markdown = data.get("markdown") or ""
            if not isinstance(markdown, str):
                raise RuntimeError("markdown is not a string")
            if case.expected_status is not None and origin_status != case.expected_status:
                raise RuntimeError(
                    f"expected origin status {case.expected_status}, got {origin_status!r}"
                )
            if case.needle and case.needle.casefold() not in markdown.casefold():
                raise RuntimeError(f"markdown did not contain {case.needle!r}")
            if len(markdown) < case.min_markdown_chars:
                raise RuntimeError(
                    f"markdown too short: {len(markdown)} < {case.min_markdown_chars}"
                )
            api_error = str(payload.get("error") or "")
            if case.error_contains and case.error_contains.casefold() not in api_error.casefold():
                raise RuntimeError(
                    f"API error did not contain {case.error_contains!r}: {api_error!r}"
                )
            if case.canonical_https:
                canonical = str(metadata.get("canonicalUrl") or "")
                if not canonical.startswith("https://"):
                    raise RuntimeError(f"redirect did not expose HTTPS canonical URL: {canonical!r}")
            return Result(
                case=case.name,
                url=case.url,
                ok=True,
                elapsed_seconds=round(time.monotonic() - started, 3),
                http_status=http_status,
                api_success=api_success,
                origin_status=origin_status,
                rendered_with=metadata.get("renderedWith"),
                markdown_chars=len(markdown),
            )
        except Exception as error:  # noqa: BLE001 - gate records every failure
            return Result(
                case=case.name,
                url=case.url,
                ok=False,
                elapsed_seconds=round(time.monotonic() - started, 3),
                error=str(error),
            )


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--api-url", default=os.environ.get("CRW_API_URL", "http://127.0.0.1:3000")
    )
    parser.add_argument("--api-key", default=os.environ.get("CRW_API_KEY"))
    parser.add_argument("--timeout", type=float, default=45.0)
    parser.add_argument("--output", type=Path)
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    client = Client(args.api_url, args.api_key, args.timeout)
    started_at = dt.datetime.now(dt.timezone.utc).isoformat()
    results = []
    for case in CASES:
        result = client.scrape(case)
        results.append(result)
        detail = (
            f"origin={result.origin_status} renderer={result.rendered_with} "
            f"markdown={result.markdown_chars}"
            if result.ok
            else f"error={result.error}"
        )
        print(
            f"{'PASS' if result.ok else 'FAIL':4} {result.case:12} "
            f"{result.elapsed_seconds:7.3f}s {detail}"
        )
    failures = [result for result in results if not result.ok]
    report = {
        "schemaVersion": 1,
        "startedAt": started_at,
        "finishedAt": dt.datetime.now(dt.timezone.utc).isoformat(),
        "apiUrl": args.api_url,
        "results": [asdict(result) for result in results],
        "passed": not failures,
        "failureCount": len(failures),
    }
    if args.output:
        args.output.write_text(json.dumps(report, indent=2) + "\n")
        print(f"Report: {args.output}")
    print(f"{'PASS' if not failures else 'FAIL'}: {len(results) - len(failures)}/{len(results)} checks")
    return 0 if not failures else 1


if __name__ == "__main__":
    raise SystemExit(main())
