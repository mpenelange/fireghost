# Standalone web retrieval deployment

This bundle uses the exact image digests currently running in the Hermes VM. No source build or Hermes installation is needed. Target: Docker Compose on a Linux homelab host with an existing Traefik Docker provider. Image architecture compatibility must be checked for non-amd64 hosts.

## Files

- compose.yaml
- .env.example (copy to .env and supply your own service API key)
- config/crw.toml (required; keep the relative directory layout)

## Setup

1. Clone the repository and enter `deploy/homelab`. Run `cp .env.example .env`, then supply your own `ROUTER_API_KEY`. Keep the actual deployment on local disk. Never commit `.env` or publish it to a registry/repository.
2. Edit `.env`: set TRAEFIK_NETWORK, TRAEFIK_ENTRYPOINT and TRAEFIK_CERTRESOLVER to your existing Traefik configuration. The supplied names are examples, not discovered settings. If Traefik uses a default certificate rather than an ACME resolver, remove the `tls.certresolver` label instead.
3. Set ALLOWED_CIDRS to the actual trusted client networks. DNS for api.firewire.cc must reach your private Traefik listener. The IP allowlist assumes direct LAN/Tailscale client connections; do not blindly allow a shared proxy/tunnel network. Do not trust arbitrary X-Forwarded-For headers. Existing LLM routes should not claim /web/api; inspect route priorities if a catch-all route already exists.
4. Ensure Traefik's entrypoint response/write timeout accommodates the router's 90-second requests (for example 120 seconds). This is a Traefik static setting, not a backend label.
5. Run:

```sh
docker compose config --quiet
docker compose pull
docker compose up -d --wait
docker compose ps
```

If image pulls require authentication, use `docker login git.firewire.cc` with a registry-read credential.

## API

Base URL: https://api.firewire.cc/web/api

```sh
# Load only the generated hex service key without printing it.
export ROUTER_API_KEY="$(sed -n 's/^ROUTER_API_KEY=//p' .env)"
curl --fail-with-body https://api.firewire.cc/web/api/v2/scrape \
  -H "Authorization: Bearer $ROUTER_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://en.wikipedia.org/wiki/Chicago","formats":["markdown"]}'
```

Search uses POST /web/api/v2/search with JSON {"query":"Python documentation","limit":5}.

Only the two API routes are exposed. Health and metrics remain private; CRW and browser containers have Traefik explicitly disabled and no host ports. Requests without a valid API key should be rejected. Test the actual Firecrawl SDK with this prefixed base URL before changing Hermes.

## Migration safety

Cloud fallback is disabled: key blank and all budgets zero. This permits testing while the old deployment still runs without creating a second paid allowance. Supply your service credential locally; the repository includes no real keys.

This starts fresh volumes; it does NOT transfer old accounting, cache or browser profiles. Before enabling paid fallback, stop the old stack and perform a controlled transfer of its router data and browser profiles, preserving ownership. Do not copy live browser profile files or overwrite a live ledger. Restore the intended budget configuration only after transferring the ledger and disabling the old paid path. Keep old images/volumes for rollback. Do not use `docker compose down -v` on valuable state.

## Verification performed here

Docker Compose configuration validation passed. A structural check confirmed four pinned services, no builds or published ports, required authentication, private API route allowlisting, and disabled cloud fallback. The actual image builds passed prior application tests and production checks. This new Traefik deployment has NOT been started on the destination host; DNS, certificates, network names, prefix routing, private access and end-to-end retrieval still require destination validation. Existing production was not modified.

## Concurrency

Set `ROUTER_MAX_INFLIGHT=8` (for example) in `.env`, then run `docker compose up -d --no-deps router` to apply it. Default: **64**, matching the currently deployed router. It must be a positive integer. This is a router-wide cap on distinct in-flight retrieval operations across clients, not requests per second or a per-client quota. Identical concurrent requests coalesce; cache hits do not consume a retrieval slot. Excess operations wait for capacity and can time out/cancel while waiting. It does not cap all open HTTP connections.

Browser-driven search has a separate `camofox_pool_size = 4` in `config/crw.toml`; the router limit does not create more browser contexts. Increasing limits can increase memory usage and upstream throttling. Start with 4–8 on a modest host and tune from real workloads.

## Firecrawl Cloud switch

Keep `router-entrypoint.sh` alongside Compose (it is a required read-only mount). The wrapper enforces `FIRECRAWL_ENABLED=false` by removing the cloud key before starting the router, even if a key is saved in `.env`. Only literal `true` or `false` are accepted; invalid values or enabling with an empty key stop startup. No image rebuild is needed.

```dotenv
FIRECRAWL_ENABLED=false
FIRECRAWL_CLOUD_API_KEY=
```

To permit fallback, set the flag to `true`, supply your cloud key, and configure nonzero cloud budgets. The template deliberately retains zero budgets, so changing the flag alone does not authorize spending. For example, after completing accounting migration you could choose burst/refill 20 and monthly 1200; these are explicit spending choices, not automatically enabled defaults. Preserve the old budget ledger before enabling the new host. Apply changes with `docker compose up -d --no-deps router`.

`ROUTER_API_KEY` authenticates your clients. `FIRECRAWL_CLOUD_API_KEY` authenticates the backend to Firecrawl; they are separate secrets. The cloud key is never committed.
