# Fallback policy

Local CRW is always attempted first. Search may use Firecrawl Cloud only after transport failure, timeout, any non-2xx response, `success:false`, or zero web results, whether or not the response includes a warning. Empty search results are not cached. Scrape may fall back after transport failure, timeout, eligible non-2xx, retryable anti-bot/timeout failure, or missing requested markdown. For scrape only, deterministic client/URL errors, robots denials, 404, and 410 do not fall back.

Cloud use requires a key and sufficient daily and monthly credits. Defaults are 20 credits/day and 200/month. When the budget prevents fallback, the router returns the truthful local result with an explicit warning where possible. Cache hits and coalesced requests avoid duplicate upstream work.
