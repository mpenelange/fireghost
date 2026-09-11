# Hermes Web Retrieval

A self-hosted, local-first Firecrawl-compatible search and scrape appliance. The
public deployment runs a Go router, CRW, Camofox, and LightPanda behind an
existing private Traefik installation. Runtime images are pinned by digest and
the public Compose path performs no builds or host-port publishing.

## Quickstart

Prerequisites: Linux, Docker Engine with Compose v2, an existing Traefik Docker
provider and external network, private DNS for the API hostname, and registry
read access to `git.firewire.cc`. The published images are tested on amd64;
verify every pinned image supports your architecture before deploying elsewhere.

```sh
git clone <repository-url> hermes-web-retrieval
cd hermes-web-retrieval
cp .env.example .env
# Edit .env: replace ROUTER_API_KEY and verify the Traefik settings/CIDRs.
docker compose pull
docker compose up -d --wait
```

This is the single end-user deployment route. Docker Compose automatically finds
[`docker-compose.yml`](docker-compose.yml) and `.env` at the repository root.
Do not commit `.env`. `ROUTER_API_KEY` authenticates clients to this appliance;
`FIRECRAWL_CLOUD_API_KEY` is a separate backend credential and is blank by
default.

The compatible defaults expose only `POST /v2/search` and `POST /v2/scrape` at
`https://api.firewire.cc/web/api`. Set `API_HOST` and `API_PATH_PREFIX` in `.env`
to change them. The prefix must begin with `/` and should not end with `/`.
Health, metrics, CRW, and both browsers remain private with no published ports.

## Traefik prerequisites

The root stack expects the external `TRAEFIK_NETWORK`, TLS `TRAEFIK_ENTRYPOINT`,
and `TRAEFIK_CERTRESOLVER` named in `.env`. DNS must reach a private Traefik
listener. `ALLOWED_CIDRS` must contain only trusted direct LAN or Tailscale client
networks; review forwarded-client IP trust before placing another proxy in front.
The Traefik entrypoint needs a response/write timeout above the router's 90-second
upstream timeout (120 seconds is a reasonable starting point).

The route has priority **100** and claims exactly
`$API_PATH_PREFIX/v2/search` and `$API_PATH_PREFIX/v2/scrape`; ensure no existing
catch-all route wins or also claims that prefix. If Traefik uses a default TLS
certificate instead of ACME, remove the cert-resolver label locally.

## Architecture and cloud policy

The authenticated router checks its persistent cache, calls local CRW first,
and may fall back to Firecrawl Cloud. CRW selects direct retrieval, LightPanda,
or Camofox. The router cache and budget ledger use `router-data`; browser profiles
use `camofox-profiles`.

Paid fallback is off by default. `FIRECRAWL_ENABLED=false` makes the startup
wrapper remove the cloud key even if one is present. Enabling it requires literal
`true` plus a cloud key. The defaults allow 1,500 monthly credits with a 20%
reserve, so the enforced cap is `floor(1500 × 0.80) = 1200`, resetting on day 3
at 00:00 UTC. A persistent token bucket holds 20 credits and continuously refills
20 per day; it has no midnight daily quota. Pacing can prevent reaching the
monthly ceiling. Maximum distinct in-flight retrievals is 64.

Optional cloud use remains disabled until explicitly enabled and does not require
building new images. Before enabling it on a migrated host, preserve the existing
ledger: new volumes start with no cache, budget history, or browser profiles.
Never run two paid instances with independent ledgers against one allowance, copy
live browser state, or overwrite a live ledger. Retain old volumes and pins for
rollback; avoid `docker compose down -v` when state matters.

## Repository and deployment tracks

- `docker-compose.yml`, `.env.example`, and `deployment/` are the supported
  pull-only public deployment.
- `deploy/compose.yaml` is the legacy developer Compose contract used by Make and
  running production workflows; it builds the router and binds loopback port
  33000. It is intentionally not the public quickstart.
- `deploy/compose.staging.yaml` is the developer staging override on loopback port
  33010.
- `crw/docker-compose.yml` belongs to the preserved upstream CRW component, not
  the assembled appliance.

Run `make help` for development commands and `make check` for the complete local
gate. Architecture, migration, updater, rollback, and regression details remain
under [`docs/`](docs/). Upstream CRW source, history, and licensing are preserved
under [`crw/`](crw/); bundled images and dependencies retain their own licenses.

This repository does not claim that Git secret history has been publicly audited
or that DNS, certificates, registry access, routing, and retrieval have been
validated on your destination host. Perform those checks before production use.
