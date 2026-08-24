<div class="page-intro">
  <div class="page-kicker">Get Started</div>
  <h1>CRW Docs</h1>
  <p class="page-subtitle">Turn websites into usable data with one API. Start with a single <code>scrape</code> request, then move into <code>search</code>, <code>map</code>, <code>crawl</code>, <code>extract</code>, <code>browse</code> (interactive browser automation), or MCP only when your workflow actually needs them.</p>
  <div class="page-capabilities">
    <div class="page-capability"><strong>Fastest first win:</strong> one URL, one markdown response</div>
    <div class="page-capability"><strong>Works for:</strong> agents, ETL, RAG, structured extraction</div>
    <div class="page-capability"><strong>Deploy:</strong> cloud first, self-host when ready</div>
  </div>
  <div class="page-actions">
    <a class="page-btn primary" href="#quick-start">Make your first request</a>
    <a class="page-btn secondary" href="#self-hosting">Self-host CRW</a>
  </div>
</div>

<div class="playground-panel">
  <div class="playground-kicker">30-second example</div>
  <div class="playground-title">The shortest path to a successful response</div>
  <div class="playground-copy">If this request works, you already understand the core CRW model: known URL in, clean content out. Everything else in the docs builds on that.</div>
</div>

```bash
curl -X POST https://fastcrw.com/api/v1/scrape \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://example.com",
    "formats": ["markdown"]
  }'
```

```json
{
  "success": true,
  "data": {
    "markdown": "# Example Domain\n\nThis domain is for use in illustrative examples...",
    "metadata": {
      "title": "Example Domain",
      "sourceURL": "https://example.com",
      "statusCode": 200,
      "elapsedMs": 32
    }
  }
}
```

## Start here

:::cards
::card{icon="code" title="Scrape a page" href="#scraping" description="Use one URL and get markdown, HTML, links, or JSON back."}
::card{icon="search" title="Search the web" href="#search" description="Find URLs first, then scrape only the results you care about."}
::card{icon="cursor" title="Browse interactively" href="#mcp" description="Drive a real browser from your agent — multi-step flows, clicks, stateful sessions (v0.4.0)."}
::card{icon="plug" title="Add MCP tools" href="#mcp" description="Give Claude, Cursor, Codex, and other hosts live web access."}
:::

## Choose your path

:::cards
::card{icon="rocket" title="Cloud API" href="#quick-start" description="The fastest first run: get a key, copy one request, and move."}
::card{icon="plug" title="MCP" href="#mcp" description="Best when your agent runtime already expects MCP tools."}
::card{icon="box" title="Self-host" href="#self-hosting" description="Best when you want your own infrastructure, auth, and deployment controls."}
:::

## Why teams switch to CRW

CRW is meant to feel easy on day one without closing off the more serious use cases:

- one API surface for single-page scrape, discovery, bounded crawl, and extraction,
- Firecrawl-compatible request shapes where they matter,
- low-ops self-hosting when you need infra control,
- and a built-in MCP server for agent workflows.

## Benchmarks

Public 3-way run on [Firecrawl scrape-content-dataset-v1](https://huggingface.co/datasets/firecrawl/scrape-content-dataset-v1), full 1000 URL, canonical `diagnose_3way.py` harness (concurrency 5 / timeout 120s):

| Metric | CRW | crawl4ai | Firecrawl |
|---|---|---|---|
| **Truth-recall (522/819 labeled URLs)** | **63.74%** | 59.95% | 56.04% |
| Scrape-success (of 1000) | 877 (87.7%) | 835 (83.5%) | 897 (89.7%) |
| Thrown errors (3000 requests) | 0 | 0 | 0 |
| p50 latency | **1914ms** | 1916ms | 2305ms |
| p90 latency | 14157ms | **4754ms** | 6937ms |
| Dependencies | single binary | Python + Playwright | Node + Redis + PG + RabbitMQ |

The 63.74% denominator is **819 labeled/matchable URLs** — not 3,000 requests, not 1,000. The **87.7% scrape-success** is stated next to "0 errors" deliberately. crw's p50 beats Firecrawl; its p90 is the disclosed worst-of-three (the recovery fallback that lifts recall is also why the tail is worst). Full result: [`bench/server-runs/RESULT_3WAY_1000_FULL.md`](https://github.com/us/crw/blob/main/bench/server-runs/RESULT_3WAY_1000_FULL.md).

## What to read next

- [Quick Start](#quick-start) for the fastest first request
- [API Overview](#rest-api) for the endpoint map
- [Scrape](#scraping) for the canonical single-page flow
- [Authentication](#authentication) for key handling and self-host auth
