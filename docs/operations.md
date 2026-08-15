# Operations

Start with `docker compose up -d --build`, then inspect `docker compose ps` and run `./scripts/smoke-test.sh`. Logs are available through `docker compose logs -f router crw camofox lightpanda`; metrics are at `http://127.0.0.1:33000/metrics`.

Back up both persistent volumes with `./scripts/backup.sh backups/appliance.tar.gz`. The script briefly stops only the services that were running, creates a quiescent archive, and starts those same services again. Restore is deliberately explicit: `./scripts/restore.sh --force backups/appliance.tar.gz`. Restore stops the stack and replaces both volumes' contents; validate the archive and retain the previous backup first.

Budget limits are enforced before cloud calls. An empty `FIRECRAWL_CLOUD_API_KEY` disables cloud fallback. Keep `.env` mode 0600 and never place credentials in Compose or source control.

The persistent response cache is capped at 1 GiB by default. Set `ROUTER_CACHE_MAX_BYTES` to a positive byte count to change the total on-disk limit; `ROUTER_CACHE_MAX_ENTRY_BYTES` remains the separate per-response body limit. On writes, expired and corrupt entries are removed and the oldest cache files are evicted until the new encoded entry fits.

`ROUTER_MAX_INFLIGHT` defaults to `64` and must be positive. It caps concurrent unique cacheable upstream operations. Same-key callers coalesce without taking another slot, and callers canceled while waiting for unique capacity return promptly without starting retained work.

`ROUTER_SERVER_READ_TIMEOUT` defaults to `30s` and must be a positive Go duration. It bounds the complete inbound request read, including the body, so increase it only when legitimate clients cannot upload their bounded request bodies within that window. `ROUTER_HTTP_TIMEOUT` remains the separate upstream-call bound.

For rollback, retain the prior `.env`, source revision, router image, CRW reference, and renderer digests. Stop the stack, restore those files/pins, rebuild the router, restore a compatible volume backup if necessary, and start plus smoke-test.
