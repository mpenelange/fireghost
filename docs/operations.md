# Legacy developer and production operations

These commands operate the preserved `dev/compose.yaml` contract, not the
root public Compose stack. Start with `make up`, then inspect `make ps` and run
`make smoke`. Logs are available through `make logs SERVICES="router crw camofox lightpanda"`; metrics are at `http://127.0.0.1:33000/metrics`.

`MCP_ENABLED` defaults to literal `false`. When set to `true`, the router serves
stateless Streamable HTTP MCP at `/mcp` and requires a nonempty
`ROUTER_API_KEY`; clients send it as `Authorization: Bearer <key>`. In the public
Traefik deployment the external URL is `$API_PATH_PREFIX/mcp` on `API_HOST`,
protected by the same CIDR allowlist as REST. MCP calls use the same cached,
coalesced, local-first search and scrape operations, including cloud budgets and
the 16 MiB upstream response ceiling. MCP request counts and durations use the
`endpoint="mcp"` metrics label; cache and upstream metrics retain their
`search` or `scrape` endpoint labels.

`ROUTER_BROWSER_PIPELINE_ENABLED=true` opts into `POST /v2/browser/scrape`
and, when MCP is enabled, the `browser_scrape` tool. CRW must be built with
the `camofox` feature and have `[renderer.camofox]` configured, pointing to
Camofox 2.4.8 or newer with private network targets disabled. The pipeline
checks the browser version before opening a tab. The deployed 2.4.6 baseline
pin remains unchanged; enable this feature only with a separately validated
candidate browser and CRW build. This operation
uses only the local CRW service, does not cache results, and does not spend
cloud credits. Existing scrape and search behavior is unchanged.

The request accepts `url`, `profile` (`article` or `redditThread`), `timeout`
in milliseconds, `maxRounds`, `maxItems`, and `maxBytes`. Defaults are
`article`, 30000, 20, 200, and 196608 respectively; upper bounds are 60000,
100, 1000, and 262144. Every budget must be positive. Scripts and arbitrary
browser actions are not accepted. Article extraction runs Mozilla Readability
on a cloned document. Reddit extraction collects comments between bounded
expansion steps, retaining earlier comments when the page replaces DOM nodes.

Results include Markdown, structured Reddit comments when requested, warnings,
and metadata identifying `browser-v1`, the stop reason, and collected item
count. Reddit's reported comment count and absence of expansion controls do
not prove that every comment was retrieved. Consumers must inspect
`metadata.complete` and warnings; a partial thread is a usable bounded result.
`maxBytes` limits content rather than the complete HTTP envelope. Browser result
truncation, blocked pages, and a different Reddit thread are errors. Tab cleanup
is bounded and best effort; a subsequent request must successfully clear its
own profile's stale tabs before opening another one.

For the isolated KVM test guest on docker0, use
`ssh -J michael@docker0 dev@192.168.153.10`. Its setup files are under
`/home/michael/docker-testing-setup` on the host. The static NAT guest already
has Docker and permits public HTTPS; normal development needs no firewall or
SSH recovery changes. Fireghost test resources must use their own names and
directories and must not remove the existing Messages containers or volumes.

## Isolated candidate

Run `make staging-up` to create a separate Compose project, network, cache, and browser-profile volume. Its router binds only to `127.0.0.1:33010`, and its router image uses the distinct `monorepo-staging` tag so it cannot replace the production router tag. Validate with `make staging-smoke staging-live-contract`, inspect with `make staging-ps`, and remove its containers and network with `make staging-down`. Production remains on port `33000` throughout.

Back up both persistent volumes with `./scripts/backup.sh backups/appliance.tar.gz`. The script briefly stops only the services that were running, creates a quiescent archive, and starts those same services again. Restore is deliberately explicit: `./scripts/restore.sh --force backups/appliance.tar.gz`. Restore stops the stack and replaces both volumes' contents; validate the archive and retain the previous backup first.

Budget limits are enforced before cloud calls. An empty `FIRECRAWL_CLOUD_API_KEY` disables cloud fallback. `ROUTER_MONTHLY_RESET_DAY` defaults to `1` and accepts `1` through `28`; the monthly ledger rolls at 00:00 UTC on that day, with earlier days assigned to the prior billing period. For example, reset day `3` keeps September 1 and 2 in the August period and starts September at September 3 00:00 UTC.

Set `ROUTER_CLOUD_BURST_CREDITS` and `ROUTER_CLOUD_REFILL_CREDITS_PER_DAY` together: both zero disables the guard, while positive values set its capacity and refill credits per 24 hours. State is stored in the existing atomic 0600 ledger using integer credit-nanosecond accounting. The bucket starts full for new and pre-bucket ledgers, survives restarts, clamps at capacity, and resets full on the billing boundary. The monthly cap is checked first and remains the hard limit. `ROUTER_DAILY_CLOUD_CREDITS` is retained for legacy configurations and defaults to zero. Budget denials, from any enabled guard, appear in `web_retrieval_cloud_budget_denied_total`. Keep `.env` mode 0600 and never place credentials in Compose or source control.

The persistent response cache is capped at 1 GiB by default. Set `ROUTER_CACHE_MAX_BYTES` to a positive byte count to change the total on-disk limit; `ROUTER_CACHE_MAX_ENTRY_BYTES` remains the separate per-response body limit. On writes, expired and corrupt entries are removed and the oldest cache files are evicted until the new encoded entry fits.

`ROUTER_MAX_INFLIGHT` defaults to `64` and must be positive. It caps concurrent unique cacheable upstream operations. Same-key callers coalesce without taking another slot, and callers canceled while waiting for unique capacity return promptly without starting retained work.

`ROUTER_SERVER_READ_TIMEOUT` defaults to `30s` and must be a positive Go duration. It bounds the complete inbound request read, including the body, so increase it only when legitimate clients cannot upload their bounded request bodies within that window. `ROUTER_HTTP_TIMEOUT` remains the separate upstream-call bound.

For rollback, retain the prior `.env`, source revision, router image, CRW reference, and renderer digests. Stop the stack, restore those files/pins, rebuild the router, restore a compatible volume backup if necessary, and start plus smoke-test.
