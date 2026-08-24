# Operations

Start with `docker compose --project-directory deploy -f deploy/compose.yaml up -d --build`, then inspect `docker compose --project-directory deploy -f deploy/compose.yaml ps` and run `./scripts/smoke-test.sh`. Logs are available through `docker compose --project-directory deploy -f deploy/compose.yaml logs -f router crw camofox lightpanda`; metrics are at `http://127.0.0.1:33000/metrics`.

Back up both persistent volumes with `./scripts/backup.sh backups/appliance.tar.gz`. The script briefly stops only the services that were running, creates a quiescent archive, and starts those same services again. Restore is deliberately explicit: `./scripts/restore.sh --force backups/appliance.tar.gz`. Restore stops the stack and replaces both volumes' contents; validate the archive and retain the previous backup first.

Budget limits are enforced before cloud calls. An empty `FIRECRAWL_CLOUD_API_KEY` disables cloud fallback. `ROUTER_MONTHLY_RESET_DAY` defaults to `1` and accepts `1` through `28`; the monthly ledger rolls at 00:00 UTC on that day, with earlier days assigned to the prior billing period. For example, reset day `3` keeps September 1 and 2 in the August period and starts September at September 3 00:00 UTC.

Set `ROUTER_CLOUD_BURST_CREDITS` and `ROUTER_CLOUD_REFILL_CREDITS_PER_DAY` together: both zero disables the guard, while positive values set its capacity and refill credits per 24 hours. State is stored in the existing atomic 0600 ledger using integer credit-nanosecond accounting. The bucket starts full for new and pre-bucket ledgers, survives restarts, clamps at capacity, and resets full on the billing boundary. The monthly cap is checked first and remains the hard limit. `ROUTER_DAILY_CLOUD_CREDITS` is retained for legacy configurations and defaults to zero. Budget denials, from any enabled guard, appear in `web_retrieval_cloud_budget_denied_total`. Keep `.env` mode 0600 and never place credentials in Compose or source control.

The persistent response cache is capped at 1 GiB by default. Set `ROUTER_CACHE_MAX_BYTES` to a positive byte count to change the total on-disk limit; `ROUTER_CACHE_MAX_ENTRY_BYTES` remains the separate per-response body limit. On writes, expired and corrupt entries are removed and the oldest cache files are evicted until the new encoded entry fits.

`ROUTER_MAX_INFLIGHT` defaults to `64` and must be positive. It caps concurrent unique cacheable upstream operations. Same-key callers coalesce without taking another slot, and callers canceled while waiting for unique capacity return promptly without starting retained work.

`ROUTER_SERVER_READ_TIMEOUT` defaults to `30s` and must be a positive Go duration. It bounds the complete inbound request read, including the body, so increase it only when legitimate clients cannot upload their bounded request bodies within that window. `ROUTER_HTTP_TIMEOUT` remains the separate upstream-call bound.

For rollback, retain the prior `.env`, source revision, router image, CRW reference, and renderer digests. Stop the stack, restore those files/pins, rebuild the router, restore a compatible volume backup if necessary, and start plus smoke-test.
