# Hermes web-retrieval appliance

This repository packages a local-first, Firecrawl-compatible search and scrape router for Hermes Agent. Only the router is exposed, on host loopback; CRW, Camofox, and LightPanda remain private to the Compose network.

## Install and run

```sh
git clone <this-repository-url> web-retrieval
cd web-retrieval
cp .env.example .env
chmod 600 .env
```

Edit `.env`. Set a strong `ROUTER_API_KEY`. Leave `FIRECRAWL_CLOUD_API_KEY` empty to disable cloud fallback, or provide it with the conservative 20/day and 200/month limits. Authenticate Docker to `git.firewire.cc` if the configured CRW image is private.

Monthly cloud usage resets at 00:00 UTC on day 1 by default. Set `ROUTER_MONTHLY_RESET_DAY` to a day from 1 through 28 to align the ledger with the Cloud billing cycle; before that day, usage remains in the prior billing period.

The persistent response cache defaults to a 1 GiB total on-disk cap via `ROUTER_CACHE_MAX_BYTES`; the existing `ROUTER_CACHE_MAX_ENTRY_BYTES` independently limits each response body.

The router executes at most 64 unique cacheable requests concurrently by default. Set `ROUTER_MAX_INFLIGHT` to a positive integer to tune this bound; callers for an already-running request still share that work without consuming another slot.

Inbound request headers and bodies must be read within `ROUTER_SERVER_READ_TIMEOUT`, which defaults to `30s` and must be positive. `ROUTER_HTTP_TIMEOUT` separately bounds each upstream request; the server write deadline includes additional time to return that bounded result.

```sh
docker compose config --quiet
docker compose up -d --build
docker compose ps
./scripts/smoke-test.sh
```

Do not publish CRW, renderer, or MCP ports. The supported host endpoint is `http://127.0.0.1:33000`.

## Hermes configuration

Configure Hermes's Firecrawl base URL as `http://127.0.0.1:33000` and its API key as the value of `ROUTER_API_KEY`. Hermes can call `POST /v2/search` and `POST /v2/scrape`; health and Prometheus-style metrics are available at `/health` and `/metrics`.

## Validation

```sh
make test
make fmt vet race
make compose-config
make build
make live-contract
```

The smoke and live-contract scripts make real local retrieval requests; the latter also verifies persistent caching, representative scrapes, and four-way search concurrency. CI substitutes a deterministic fake upstream and never supplies or calls a paid Firecrawl account.

## Operations and rollback

See [architecture](docs/architecture.md), [operations](docs/operations.md), [updating](docs/updating.md), and the exact [fallback policy](docs/fallback-policy.md). Before an upgrade, run `./scripts/backup.sh backups/pre-upgrade.tar.gz`. Roll back by restoring the previous source and image pins, rebuilding, and, only if data compatibility requires it, running `./scripts/restore.sh --force backups/pre-upgrade.tar.gz`.

## Licensing boundaries

The Go router and this packaging are governed by this repository's license. CRW, Camofox, LightPanda, Alpine, and Go images are separately distributed works under their respective upstream licenses and terms. Their inclusion as runtime images does not relicense them. Operators are responsible for registry access, license compliance, website terms, robots policies, and lawful retrieval.
