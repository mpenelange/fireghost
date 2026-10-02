# Fallback policy

Local CRW is always attempted first. Search may use Firecrawl Cloud only after transport failure, timeout, any non-2xx response, `success:false`, or zero web results, whether or not the response includes a warning. Empty search results are not cached. Scrape may fall back after transport failure, timeout, eligible non-2xx, retryable anti-bot/timeout failure, or missing requested markdown. For scrape only, deterministic client/URL errors, robots denials, 404, and 410 do not fall back.

Cloud use requires a key and sufficient credits. The monthly hard cap defaults to 200 credits and remains authoritative. Monthly usage resets at 00:00 UTC on `ROUTER_MONTHLY_RESET_DAY` (default `1`, valid range `1..28`); days before the configured reset remain in the prior billing period.

The shipped deployment replaces the calendar-daily cap with an optional persisted token bucket: capacity 20 credits, continuously refilled at 20 credits per 24 hours. `ROUTER_CLOUD_BURST_CREDITS` and `ROUTER_CLOUD_REFILL_CREDITS_PER_DAY` must both be zero (disabled) or both be positive. A new or legacy ledger starts full, the balance survives restart, refill never exceeds capacity, and a new billing cycle resets it full. The legacy `ROUTER_DAILY_CLOUD_CREDITS` limit is still supported and can be combined with the bucket, but now defaults to zero. A denied reservation consumes neither calendar nor monthly usage. When any budget guard prevents fallback, the router returns the truthful local result with an explicit warning where possible and increments `web_retrieval_cloud_budget_denied_total`. Cache hits and coalesced requests avoid duplicate upstream work.

## Routing table

The router serves the Firecrawl v2 API surface in three tiers. Routes outside all three return 404 before any upstream is contacted.

| Tier | Routes | Upstream | Fallback |
| --- | --- | --- | --- |
| Retrieval | `POST /v2/search`, `POST /v2/scrape` | CRW, then Firecrawl Cloud | As described above; cached |
| CRW-implemented | `POST /v2/map`; `/v2/crawl`, `/v2/crawl/active`, `/v2/crawl/{id}`, `/v2/crawl/{id}/errors`; `/v2/batch/scrape`, `/v2/batch/scrape/{id}`, `/v2/batch/scrape/{id}/errors`; `/v2/extract`, `/v2/extract/{id}`; `POST /v2/parse`; `GET /v2/scrape/{jobId}` | CRW only | Never; CRW's response or a 502 is returned |
| Cloud-only | `/v2/agent/**`, `/v2/interact/**`, `/v2/scrape/{jobId}/interact`, `POST /v2/crawl/params-preview`, `/v2/search/research/papers/**`, `/v2/search/developer`, and read-only `GET /v2/team/{credit-usage,token-usage}[/historical]`, `/v2/team/queue-status`, `/v2/team/activity` | Firecrawl Cloud with the configured key | Not applicable |

Excluded on purpose: `/v2/monitor/**` (it schedules recurring billing that the router never sees), `PUT /v2/team/threat-protection` (account settings), feedback routes (jobs mostly run on CRW, not Cloud), `/v2/support/*`, and `GET /v2/parse/formats` (parse runs on CRW). CRW-implemented and cloud-only requests are streamed, not cached. `POST /v2/parse` accepts bodies up to `ROUTER_MAX_PARSE_BYTES` (default 50 MiB); every other route uses `ROUTER_MAX_REQUEST_BYTES`. Path parameters must match `[A-Za-z0-9_-]+`, so encoded slashes or dot segments cannot reach a different upstream route.

## Account credit floor

Every billable cloud request (any method other than GET, HEAD, or DELETE, including search and scrape fallback) first reads `remainingCredits` from `GET /v2/team/credit-usage`. When the balance is at or below `ROUTER_CLOUD_CREDIT_FLOOR` (default `50`), the request is refused with 503 (or, for fallback, the local result is returned with the budget warning) and `web_retrieval_cloud_budget_denied_total` increments. The balance is never cached, so a variable-cost job such as an agent run is reflected by the next check; one such job can cross the floor, but nothing after it starts. If the balance cannot be read, cloud requests fail closed. Reads and cancellations always pass so already-billed results stay reachable. The floor precedes the local ledger, so a floor denial consumes no local budget. Cloud-only routes are bounded by the floor alone; the monthly cap and token bucket continue to apply to search and scrape fallback only. `FIRECRAWL_ENABLED=false` disables every cloud route as well as fallback.
