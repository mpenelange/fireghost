#!/usr/bin/env python3
"""Per-URL diagnostic: scrape, compare against truth/lie, save full traces.

Same matching logic as run_bench.py, but persists every (url, status, markdown,
truth_match_phrases, missed_phrases, lie_leak_phrases, latency, error) so we
can post-mortem WHY truth_recall is ~45%.

CLI flags override env-var defaults. Phase 0 (v6 plan) added --debug to capture
the server's debugExtraction payload (multi-attempt, with candidate text).
"""

import argparse
import asyncio
import json
import os
import time

import aiohttp
from datasets import load_dataset


def split_phrases(text: str, min_len: int) -> list[str]:
    return [w.strip() for w in text.split("\n") if len(w.strip()) > min_len]


def match_phrases(haystack_lower: str, phrases: list[str]) -> tuple[list[str], list[str]]:
    hits, misses = [], []
    for p in phrases:
        (hits if p.lower() in haystack_lower else misses).append(p)
    return hits, misses


async def diagnose(session, row, sem, args):
    url = row["url"]
    truth = row.get("truth_text", "")
    lie = row.get("lie_text", "")
    record = {
        "url": url,
        "id": row.get("id"),
        "scrape_ok": False,
        "status": 0,
        "latency_ms": 0,
        "error": None,
        "markdown_len": 0,
        "markdown_excerpt": "",
        "truth_phrases_total": 0,
        "truth_phrases_hit": 0,
        "truth_recall": 0.0,
        "truth_found": False,
        "missed_truth_excerpt": [],
        "lie_phrases_total": 0,
        "lie_phrases_hit": 0,
        "lie_found": False,
        "leaked_lie_excerpt": [],
        "debug_extraction": None,
    }
    # Request both markdown and plainText. Phrase matching runs against the
    # union of both (substring in either counts as a hit) — markdown's
    # `[text](url)` link syntax otherwise turns plaintext truth phrases like
    # "supports Sign in with Google for the web, native applications" into
    # false negatives, since the rendered markdown interleaves URL text.
    # Including plainText recovers those without regressing markdown-only hits.
    request_body = {"url": url, "formats": ["markdown", "plainText"]}
    if args.debug:
        request_body["debug"] = True
    async with sem:
        t0 = time.monotonic()
        try:
            async with session.post(
                f"{args.api_url}/v1/scrape",
                json=request_body,
                timeout=aiohttp.ClientTimeout(total=args.timeout),
            ) as resp:
                record["status"] = resp.status
                record["latency_ms"] = (time.monotonic() - t0) * 1000
                body = await resp.json()
                # Always capture debugExtraction when present, even on failure.
                data = body.get("data") or {}
                if "debugExtraction" in data:
                    record["debug_extraction"] = data["debugExtraction"]
                if body.get("success") and data.get("markdown"):
                    md = data["markdown"]
                    plain = data.get("plainText") or data.get("plain_text") or ""
                    record["scrape_ok"] = True
                    record["markdown_len"] = len(md)
                    record["markdown_excerpt"] = md[:600]
                    # Haystack = markdown ∪ plainText. Concatenated lower-cased
                    # so phrase matching succeeds in either rendering.
                    haystack_lower = (md + "\n" + plain).lower()

                    truth_phrases = split_phrases(truth, 20)
                    if truth_phrases:
                        hits, misses = match_phrases(haystack_lower, truth_phrases)
                        record["truth_phrases_total"] = len(truth_phrases)
                        record["truth_phrases_hit"] = len(hits)
                        record["truth_recall"] = round(len(hits) / len(truth_phrases), 3)
                        record["truth_found"] = record["truth_recall"] >= 0.3
                        record["missed_truth_excerpt"] = [m[:120] for m in misses[:5]]

                    lie_phrases = split_phrases(lie, 10)
                    if lie_phrases:
                        hits, _ = match_phrases(haystack_lower, lie_phrases)
                        record["lie_phrases_total"] = len(lie_phrases)
                        record["lie_phrases_hit"] = len(hits)
                        record["lie_found"] = (
                            len(hits) / len(lie_phrases) >= 0.5
                        )
                        record["leaked_lie_excerpt"] = [h[:80] for h in hits[:5]]
                else:
                    record["error"] = body.get("error", "no markdown")
        except asyncio.TimeoutError:
            record["error"] = "timeout"
            record["latency_ms"] = args.timeout * 1000
        except Exception as e:
            record["error"] = str(e)[:160]
    return record


def load_rows(args) -> list[dict]:
    """Load URL rows from --urls-file (JSONL) or HuggingFace dataset.

    --urls-file expects JSONL where each line has at least {"url", "id"};
    "truth_text" and "lie_text" are optional but enable recall/leak metrics.
    """
    if args.urls_file:
        rows = []
        with open(args.urls_file) as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                rows.append(json.loads(line))
    else:
        ds = load_dataset("firecrawl/scrape-content-dataset-v1", split="train")
        rows = list(ds)

    if args.filter_substring:
        needle = args.filter_substring.lower()
        rows = [r for r in rows if needle in r.get("url", "").lower()]

    if args.max_urls > 0:
        rows = rows[: args.max_urls]
    return rows


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Per-URL extraction diagnostic against a running CRW server.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    p.add_argument(
        "--api-url",
        default=os.getenv("CRW_API_URL", "http://localhost:3000"),
        help="CRW server base URL",
    )
    p.add_argument(
        "--concurrency",
        type=int,
        default=int(os.getenv("BENCH_CONCURRENCY", "5")),
    )
    p.add_argument(
        "--timeout",
        type=int,
        default=int(os.getenv("BENCH_TIMEOUT", "30")),
        help="Per-request timeout (seconds)",
    )
    p.add_argument(
        "--max-urls",
        type=int,
        default=int(os.getenv("BENCH_MAX_URLS", "100")),
        help="Max URLs to process; <=0 means no limit",
    )
    p.add_argument(
        "--output",
        default=os.getenv("DIAG_OUT", "bench/server-runs/diagnose.jsonl"),
        help="Output JSONL path",
    )
    p.add_argument(
        "--urls-file",
        default=os.getenv("DIAG_URLS_FILE"),
        help="Read URLs from JSONL file instead of HuggingFace dataset",
    )
    p.add_argument(
        "--filter-substring",
        default=os.getenv("DIAG_FILTER"),
        help="Keep only URLs whose URL contains this substring (case-insensitive)",
    )
    p.add_argument(
        "--debug",
        action="store_true",
        default=os.getenv("DIAG_DEBUG", "").lower() in ("1", "true", "yes"),
        help="Send debug=true and capture debugExtraction payload",
    )
    return p.parse_args()


async def amain():
    args = parse_args()
    print(f"Loading rows…")
    rows = load_rows(args)

    sem = asyncio.Semaphore(args.concurrency)
    print(
        f"Diagnosing {len(rows)} URLs against {args.api_url} "
        f"(conc={args.concurrency}, debug={args.debug})"
    )

    out_path = args.output
    os.makedirs(os.path.dirname(out_path) or ".", exist_ok=True)
    with open(out_path, "w") as f:
        async with aiohttp.ClientSession() as session:
            tasks = [diagnose(session, r, sem, args) for r in rows]
            done = 0
            for coro in asyncio.as_completed(tasks):
                rec = await coro
                f.write(json.dumps(rec, ensure_ascii=False) + "\n")
                done += 1
                if done % 25 == 0 or done == len(rows):
                    print(f"  [{done}/{len(rows)}]")
    print(f"\nSaved {out_path}")


if __name__ == "__main__":
    asyncio.run(amain())
