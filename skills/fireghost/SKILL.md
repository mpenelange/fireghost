---
name: fireghost
description: Web search, page reading, site mapping, crawling, and structured data extraction through the user's self-hosted Fireghost appliance (Firecrawl-compatible), via the `fireghost` command. Use it instead of WebSearch and WebFetch for any live-web task — looking something up, reading a page or docs, checking prices or listings, pulling data off a site — because it renders JavaScript in a real browser from the user's home connection and returns full markdown, not a summary.
---

# Fireghost

`fireghost` is the official Firecrawl CLI pointed at the user's own Fireghost
appliance (`https://web-scrape.firewire.cc`). Local retrieval is free; the
appliance falls back to Firecrawl Cloud by itself only when a page blocks it,
within a credit budget. Results come from the user's home IP, so location-aware
sites (stores, local search) answer for the Chicago area.

Run `fireghost <command> --help` for every option. Commands not listed here are
either Firecrawl Cloud-only or unsupported; see the last section.

## Output

Write results to files and read them in bounded pieces — pages are large.

```bash
mkdir -p "${TMPDIR:-/tmp}/fireghost"
fireghost scrape "https://example.com/pricing" -o "${TMPDIR:-/tmp}/fireghost/pricing.md"
wc -l "${TMPDIR:-/tmp}/fireghost/pricing.md"; grep -n -i "price" "${TMPDIR:-/tmp}/fireghost/pricing.md" | head
```

Always quote URLs (`?` and `&` are shell syntax). Multiple formats or `--json`
produce JSON; use `jq` or Python to pick fields.

## Commands

| Need | Command |
| --- | --- |
| Find pages | `fireghost search "query" --limit 5 --json -o FILE` |
| Find pages and read them in one step | `fireghost search "query" --limit 3 --scrape --json -o FILE` |
| Read one page | `fireghost scrape "URL" -o FILE` (add `--only-main-content` to drop navigation) |
| Pull structured data | `fireghost scrape "URL" --format json --schema '{"type":"object","properties":{...}}' -o FILE` |
| Find URLs on a site | `fireghost map "URL" --limit 200` (add `--search term` to filter) |
| Read a section of a site | `fireghost crawl "URL" --limit 20 --include-paths /docs --wait -o FILE` |
| Parse a local PDF | `fireghost parse ./file.pdf -o FILE` |
| Check the account | `fireghost credit-usage` |

- **search**: `--sources web` is added automatically. `--tbs qdr:d|w|m|y`
  limits by recency; `--sources news` searches news. With `--scrape`, reuse the
  returned page content instead of scraping those URLs again.
- **scrape**: JavaScript-heavy pages render automatically; add `--wait-for 3000`
  if content loads late. Several URLs can be passed at once.
- **structured data** (`--format json --schema …`): the appliance's own LLM
  (Fireworks gpt-oss-120b) fills the schema from the page — about $0.002 per
  page on the user's Fireworks account. Prefer it for long or repetitive pages
  (product grids, tables, listings); for a few facts, scrape and read instead.
- **crawl**: always pass `--limit`; use `--wait` to block until done.

## Limits and costs

- Sign-in walls (e.g. Reddit) come back as `login_required`; don't retry them.
- Some retailer pages load deals or ads from third-party widgets that never
  reach the page text (e.g. Jewel-Osco's weekly ad). Say so rather than guess.
- `agent`, `interact`, `research`, and `developer` run only on Firecrawl Cloud
  and spend the user's credits; the wrapper refuses them unless
  `FIREGHOST_ALLOW_CLOUD=1` is set. Ask the user before using them.
- `monitor`, `alexandria`, `find-tools`, provider `scrape provider/capability`
  calls, and feedback commands are not available through Fireghost.
- Never put secrets or personal data in URLs or search queries.
