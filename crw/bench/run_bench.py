#!/usr/bin/env python3
"""CRW Scrape-Evals Benchmark

Tests CRW against Firecrawl's scrape-content-dataset-v1 (1000 real URLs).
Measures: coverage, content quality (truth hit rate), noise rejection, latency.
"""

import json
import time
import sys
import os
import asyncio
import aiohttp
from dataclasses import dataclass, field
from datasets import load_dataset

CRW_URL = os.getenv("CRW_API_URL", "http://localhost:3000")
CONCURRENCY = int(os.getenv("BENCH_CONCURRENCY", "10"))
TIMEOUT = int(os.getenv("BENCH_TIMEOUT", "30"))
MAX_URLS = int(os.getenv("BENCH_MAX_URLS", "0"))  # 0 = all

@dataclass
class Result:
    url: str
    success: bool = False
    status_code: int = 0
    has_markdown: bool = False
    truth_found: bool = False
    truth_matchable: bool = False  # truth_text yielded usable phrases
    lie_found: bool = False
    latency_ms: float = 0
    error: str = ""
    markdown_len: int = 0

@dataclass
class Stats:
    total: int = 0
    success: int = 0
    failed: int = 0
    truth_hits: int = 0
    truth_matchable: int = 0  # excludes dataset rows with None / nav-only truth_text
    lie_hits: int = 0
    latencies: list = field(default_factory=list)
    errors: dict = field(default_factory=dict)

def check_content(markdown: str, truth_text: str, lie_text: str):
    """Check if truth content is found and noise is rejected.

    Returns (truth_found, truth_matchable, lie_found). truth_matchable=False when
    the dataset row's truth_text is None / empty / contains no >20-char lines —
    such rows must be excluded from recall denominator (bench artifact, not scraper fault).
    """
    md_lower = markdown.lower()
    truth_words = [w.strip().lower() for w in (truth_text or "").split("\n") if len(w.strip()) > 20]
    truth_matchable = len(truth_words) > 0
    truth_found = False
    if truth_matchable:
        matches = sum(1 for phrase in truth_words if phrase in md_lower)
        truth_found = matches / len(truth_words) >= 0.3

    lie_words = [w.strip().lower() for w in (lie_text or "").split("\n") if len(w.strip()) > 10]
    lie_found = False
    if lie_words:
        matches = sum(1 for phrase in lie_words if phrase in md_lower)
        lie_found = matches / len(lie_words) >= 0.5

    return truth_found, truth_matchable, lie_found

async def scrape_url(session: aiohttp.ClientSession, url: str, truth: str, lie: str, sem: asyncio.Semaphore) -> Result:
    result = Result(url=url)
    async with sem:
        try:
            start = time.monotonic()
            async with session.post(
                f"{CRW_URL}/v1/scrape",
                json={"url": url, "formats": ["markdown"]},
                timeout=aiohttp.ClientTimeout(total=TIMEOUT),
            ) as resp:
                result.latency_ms = (time.monotonic() - start) * 1000
                result.status_code = resp.status
                body = await resp.json()

                if body.get("success") and body.get("data", {}).get("markdown"):
                    result.success = True
                    result.has_markdown = True
                    md = body["data"]["markdown"]
                    result.markdown_len = len(md)
                    result.truth_found, result.truth_matchable, result.lie_found = check_content(md, truth, lie)
                else:
                    result.error = body.get("error", "no markdown")
        except asyncio.TimeoutError:
            result.latency_ms = TIMEOUT * 1000
            result.error = "timeout"
        except Exception as e:
            result.error = str(e)[:100]
    return result

async def run_benchmark():
    print(f"Loading dataset from HuggingFace...")
    ds = load_dataset("firecrawl/scrape-content-dataset-v1", split="train")

    urls = list(ds)
    if MAX_URLS > 0:
        urls = urls[:MAX_URLS]

    total = len(urls)
    print(f"Benchmarking CRW against {total} URLs (concurrency={CONCURRENCY}, timeout={TIMEOUT}s)")
    print(f"Server: {CRW_URL}")
    print("=" * 60)

    stats = Stats(total=total)
    sem = asyncio.Semaphore(CONCURRENCY)
    completed = 0

    async with aiohttp.ClientSession() as session:
        tasks = [
            scrape_url(session, row["url"], row.get("truth_text", ""), row.get("lie_text", ""), sem)
            for row in urls
        ]

        for coro in asyncio.as_completed(tasks):
            result = await coro
            completed += 1

            if result.success:
                stats.success += 1
                stats.latencies.append(result.latency_ms)
                if result.truth_matchable:
                    stats.truth_matchable += 1
                    if result.truth_found:
                        stats.truth_hits += 1
                if result.lie_found:
                    stats.lie_hits += 1
            else:
                stats.failed += 1
                err_key = result.error[:40] if result.error else "unknown"
                stats.errors[err_key] = stats.errors.get(err_key, 0) + 1

            if completed % 50 == 0 or completed == total:
                pct = completed / total * 100
                sr = stats.success / completed * 100 if completed else 0
                print(f"  [{completed}/{total}] {pct:.0f}% done | success rate: {sr:.1f}%")

    # Calculate percentiles
    latencies = sorted(stats.latencies)
    p50 = latencies[len(latencies) // 2] if latencies else 0
    p95 = latencies[int(len(latencies) * 0.95)] if latencies else 0
    p99 = latencies[int(len(latencies) * 0.99)] if latencies else 0
    avg = sum(latencies) / len(latencies) if latencies else 0

    # Quality metrics
    quality_total = stats.success  # successful scrapes (denominator for noise rejection)
    matchable = stats.truth_matchable  # excludes dataset rows with no usable truth_text
    precision = ((quality_total - stats.lie_hits) / quality_total * 100) if quality_total else 0
    # Fair recall: only count rows where the dataset actually provided matchable truth_text
    recall = (stats.truth_hits / matchable * 100) if matchable else 0
    recall_naive = (stats.truth_hits / quality_total * 100) if quality_total else 0

    print("\n" + "=" * 60)
    print("RESULTS")
    print("=" * 60)
    print(f"\n📊 Coverage:")
    print(f"  Total URLs:      {stats.total}")
    print(f"  Successful:      {stats.success} ({stats.success/stats.total*100:.1f}%)")
    print(f"  Failed:          {stats.failed} ({stats.failed/stats.total*100:.1f}%)")

    print(f"\n⚡ Latency (successful requests):")
    print(f"  Average:         {avg:.0f}ms")
    print(f"  P50:             {p50:.0f}ms")
    print(f"  P95:             {p95:.0f}ms")
    print(f"  P99:             {p99:.0f}ms")

    print(f"\n📝 Content Quality:")
    print(f"  Successful scrapes:   {quality_total}")
    print(f"  Matchable (truth ok): {matchable}  (rest = dataset has no usable truth_text)")
    print(f"  Truth recall (fair):  {recall:.1f}% = {stats.truth_hits}/{matchable}")
    print(f"  Truth recall (naive): {recall_naive:.1f}% = {stats.truth_hits}/{quality_total}")
    print(f"  Noise rejection:      {precision:.1f}% (noise NOT in output)")
    print(f"  Noise leaks:          {stats.lie_hits}")

    if stats.errors:
        print(f"\n❌ Top errors:")
        for err, count in sorted(stats.errors.items(), key=lambda x: -x[1])[:10]:
            print(f"  {count:4d}x  {err}")

    # Save detailed results as JSON
    report = {
        "server": CRW_URL,
        "total": stats.total,
        "concurrency": CONCURRENCY,
        "timeout_s": TIMEOUT,
        "coverage": {
            "success": stats.success,
            "failed": stats.failed,
            "success_rate": round(stats.success / stats.total * 100, 2),
        },
        "latency_ms": {
            "avg": round(avg, 1),
            "p50": round(p50, 1),
            "p95": round(p95, 1),
            "p99": round(p99, 1),
        },
        "quality": {
            "truth_recall": round(recall, 2),
            "truth_recall_naive": round(recall_naive, 2),
            "matchable": matchable,
            "noise_rejection": round(precision, 2),
            "truth_hits": stats.truth_hits,
            "noise_leaks": stats.lie_hits,
        },
        "errors": dict(sorted(stats.errors.items(), key=lambda x: -x[1])[:10]),
    }
    out_path = os.getenv("BENCH_RESULTS_PATH", "bench/results.json")
    with open(out_path, "w") as f:
        json.dump(report, f, indent=2)
    print(f"\nDetailed results saved to {out_path}")

if __name__ == "__main__":
    asyncio.run(run_benchmark())
