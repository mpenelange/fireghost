# Hermes web-retrieval appliance

This repository packages a local-first, Firecrawl-compatible search and scrape router for Hermes Agent. Only the router is exposed, on host loopback; CRW, Camofox, and LightPanda remain private to the Compose network.

CRW application source lives in the separate `michael/crw-camofox` repository. That repository produces an immutable container image; this repository pins its digest, integrates it with the browsers and router, and owns appliance-level validation. The exact [release boundary and handoff](docs/architecture.md#repository-and-release-boundary) are documented explicitly.

## Install and run

```sh
git clone <this-repository-url> hermes-web-retrieval
cd hermes-web-retrieval
cp deploy/.env.example deploy/.env
chmod 600 deploy/.env
```

Edit `deploy/.env`. Set a strong `ROUTER_API_KEY`. Leave `FIRECRAWL_CLOUD_API_KEY` empty to disable cloud fallback, or provide it with the conservative 20-credit burst guard and 200-credit monthly hard cap. Authenticate Docker to `git.firewire.cc` if the configured CRW image is private.

Monthly cloud usage resets at 00:00 UTC on day 1 by default. Set `ROUTER_MONTHLY_RESET_DAY` to a day from 1 through 28 to align the ledger with the Cloud billing cycle; before that day, usage remains in the prior billing period.

The optional burst guard is configured by setting both `ROUTER_CLOUD_BURST_CREDITS` and `ROUTER_CLOUD_REFILL_CREDITS_PER_DAY` to positive integers; setting both to zero disables it. The bucket starts full, refills continuously, persists across restarts, never exceeds its burst capacity, and resets full with the monthly billing cycle. `ROUTER_DAILY_CLOUD_CREDITS` remains available for backward compatibility, but defaults to zero in the shipped deployment.

The persistent response cache defaults to a 1 GiB total on-disk cap via `ROUTER_CACHE_MAX_BYTES`; the existing `ROUTER_CACHE_MAX_ENTRY_BYTES` independently limits each response body.

The router executes at most 64 unique cacheable requests concurrently by default. Set `ROUTER_MAX_INFLIGHT` to a positive integer to tune this bound; callers for an already-running request still share that work without consuming another slot.

Inbound request headers and bodies must be read within `ROUTER_SERVER_READ_TIMEOUT`, which defaults to `30s` and must be positive. `ROUTER_HTTP_TIMEOUT` separately bounds each upstream request; the server write deadline includes additional time to return that bounded result.

```sh
make compose-config
make up
make ps
make smoke
```

Do not publish CRW, renderer, or MCP ports. The supported host endpoint is `http://127.0.0.1:33000`.

## Hermes configuration

Configure Hermes's Firecrawl base URL as `http://127.0.0.1:33000` and its API key as the value of `ROUTER_API_KEY`. Hermes can call `POST /v2/search` and `POST /v2/scrape`; health and Prometheus-style metrics are available at `/health` and `/metrics`.

## Validation

```sh
make test
make check
make compose-config
make build-images
make live-contract
```

The smoke and live-contract scripts make real local retrieval requests; the latter also verifies persistent caching, representative scrapes, and four-way search concurrency. CI substitutes a deterministic fake upstream and never supplies or calls a paid Firecrawl account.

## Operations and rollback

See [architecture](docs/architecture.md), [operations](docs/operations.md), [updating](docs/updating.md), and the exact [fallback policy](docs/fallback-policy.md). Before an upgrade, run `./scripts/backup.sh backups/pre-upgrade.tar.gz`. Roll back by restoring the previous source and image pins, rebuilding, and, only if data compatibility requires it, running `./scripts/restore.sh --force backups/pre-upgrade.tar.gz`.

## Licensing boundaries

The Go router and this packaging are governed by this repository's license. CRW, Camofox, LightPanda, Alpine, and Go images are separately distributed works under their respective upstream licenses and terms. Their inclusion as runtime images does not relicense them. Operators are responsible for registry access, license compliance, website terms, robots policies, and lawful retrieval.
