#!/usr/bin/env python3
"""Hard live compatibility gate for a CRW + Camofox candidate stack.

Unlike `mcp_smoke.py`, browser-backed searches are release assertions here.
The script uses only the Python standard library so it can run directly on a
Hermes host. Test an isolated candidate; never point it at production traffic.
"""

from __future__ import annotations

import argparse
import concurrent.futures
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
    engine: str
    query: str
    require_results: bool = True
    require_warning_if_empty: bool = False


@dataclass
class Result:
    phase: str
    engine: str
    query: str
    ok: bool
    elapsed_seconds: float
    result_count: int | None = None
    warning: str | None = None
    error: str | None = None


CASES = (
    # Datacenter Google can complete at a challenge wall with an explicit
    # warning. Completion is the contract; a transport/HTTP timeout is not.
    Case(
        "google",
        "Camoufox browser release",
        require_results=False,
        require_warning_if_empty=True,
    ),
    # The captured 2.4.6 baseline completed Bing with zero results.
    Case("bing", "Rust programming language", require_results=False),
    Case("duckduckgo", "Python programming language"),
    Case("wikipedia", "Alan Turing"),
    Case("youtube", "Rust programming language tutorial"),
    Case("reddit", "Rust programming language"),
    Case("amazon", "USB C cable"),
    Case("github", "rust-lang rust"),
)

CONCURRENT_ENGINES = {"duckduckgo", "wikipedia", "youtube", "reddit"}


class Client:
    def __init__(self, base_url: str, api_key: str | None, timeout: float):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def request(self, method: str, path: str, body: dict[str, Any] | None = None) -> Any:
        headers = {"Accept": "application/json"}
        data = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(body).encode()
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        request = urllib.request.Request(
            f"{self.base_url}{path}", data=data, headers=headers, method=method
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                raw = response.read().decode()
        except urllib.error.HTTPError as error:
            detail = error.read().decode(errors="replace")[:500]
            raise RuntimeError(f"HTTP {error.code}: {detail}") from error
        except (urllib.error.URLError, TimeoutError) as error:
            raise RuntimeError(f"transport failure: {error}") from error
        try:
            return json.loads(raw)
        except json.JSONDecodeError as error:
            raise RuntimeError(f"non-JSON response: {raw[:200]!r}") from error

    def health(self) -> dict[str, Any]:
        payload = self.request("GET", "/health")
        if not isinstance(payload, dict) or payload.get("status") != "ok":
            raise RuntimeError(f"unhealthy CRW response: {payload!r}")
        return payload

    def search(self, case: Case, phase: str) -> Result:
        started = time.monotonic()
        try:
            payload = self.request(
                "POST",
                "/v1/search",
                {"query": case.query, "engines": [case.engine], "limit": 5},
            )
            if not isinstance(payload, dict) or payload.get("success") is not True:
                raise RuntimeError(f"unsuccessful API response: {payload!r}")
            data = payload.get("data")
            if not isinstance(data, dict):
                raise RuntimeError(f"missing response data: {payload!r}")
            results = data.get("results")
            if not isinstance(results, list):
                raise RuntimeError(f"results is not a list: {results!r}")
            warnings = data.get("warnings", [])
            warning_parts = [str(item) for item in warnings] if isinstance(warnings, list) else []
            if payload.get("warning"):
                warning_parts.append(str(payload["warning"]))
            warning = "; ".join(warning_parts) or None
            if case.require_results and not results:
                raise RuntimeError(f"unexpected empty result set; warning={warning!r}")
            if not results and case.require_warning_if_empty and not warning:
                raise RuntimeError("empty result set did not surface a challenge warning")
            return Result(
                phase=phase,
                engine=case.engine,
                query=case.query,
                ok=True,
                elapsed_seconds=round(time.monotonic() - started, 3),
                result_count=len(results),
                warning=warning,
            )
        except Exception as error:  # noqa: BLE001 - gate records every failure
            return Result(
                phase=phase,
                engine=case.engine,
                query=case.query,
                ok=False,
                elapsed_seconds=round(time.monotonic() - started, 3),
                error=str(error),
            )


def print_result(result: Result) -> None:
    status = "PASS" if result.ok else "FAIL"
    detail = (
        f"results={result.result_count}"
        if result.ok
        else f"error={result.error}"
    )
    if result.warning:
        detail += f" warning={result.warning}"
    print(
        f"{status:4} {result.phase:10} {result.engine:12} "
        f"{result.elapsed_seconds:7.3f}s {detail}"
    )


def run_serial(client: Client, phase: str) -> list[Result]:
    results = []
    for case in CASES:
        result = client.search(case, phase)
        results.append(result)
        print_result(result)
    return results


def run_concurrent(client: Client) -> list[Result]:
    cases = [case for case in CASES if case.engine in CONCURRENT_ENGINES]
    with concurrent.futures.ThreadPoolExecutor(max_workers=len(cases)) as pool:
        futures = [pool.submit(client.search, case, "concurrent") for case in cases]
        results = [future.result() for future in futures]
    for result in results:
        print_result(result)
    return results


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--api-url",
        default=os.environ.get("CRW_API_URL", "http://127.0.0.1:3000"),
    )
    parser.add_argument(
        "--api-key",
        default=os.environ.get("CRW_API_KEY"),
        help="CRW bearer key (defaults to CRW_API_KEY; never written to output)",
    )
    parser.add_argument("--timeout", type=float, default=25.0)
    parser.add_argument(
        "--phase",
        choices=("cold", "warm", "concurrent", "all"),
        default="all",
    )
    parser.add_argument("--output", type=Path)
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    client = Client(args.api_url, args.api_key, args.timeout)
    started_at = dt.datetime.now(dt.timezone.utc).isoformat()
    try:
        health = client.health()
    except Exception as error:  # noqa: BLE001
        print(f"FAIL health: {error}", file=sys.stderr)
        return 1
    print(f"CRW health: {json.dumps(health, sort_keys=True)}")

    results: list[Result] = []
    if args.phase in ("cold", "all"):
        results.extend(run_serial(client, "cold"))
    if args.phase in ("warm", "all"):
        results.extend(run_serial(client, "warm"))
    if args.phase in ("concurrent", "all"):
        results.extend(run_concurrent(client))

    failures = [result for result in results if not result.ok]
    report = {
        "schemaVersion": 1,
        "startedAt": started_at,
        "finishedAt": dt.datetime.now(dt.timezone.utc).isoformat(),
        "apiUrl": args.api_url,
        "health": health,
        "phase": args.phase,
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
