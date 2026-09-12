# Fireghost

Fireghost is a self-hosted, local-first retrieval platform with
Firecrawl-compatible search and scrape APIs. It runs a Go router, CRW, Camofox,
and LightPanda behind an existing private Traefik installation, and is designed
for clients that need private retrieval infrastructure rather than for one
specific assistant. Future client integrations may include MCP, but Fireghost
does not currently provide an MCP server. Owned router and CRW images default
to the newest tested stable release; third-party browser images remain pinned
by digest. The public Compose path performs no builds or host-port publishing.

## Quickstart

Prerequisites: Linux, Docker Engine with Compose v2, an existing Traefik Docker
provider and external network, private DNS for the API hostname, and registry
read access to `git.firewire.cc`. The published images are tested on amd64;
verify every pinned image supports your architecture before deploying elsewhere.

```sh
git clone https://git.firewire.cc/michael/fireghost.git
cd fireghost
cp .env.example .env
# Edit .env: replace ROUTER_API_KEY and verify the Traefik settings/CIDRs.
docker compose pull
docker compose up -d --wait
```

To upgrade to the newest tested stable appliance images:

```sh
git pull --ff-only
docker compose up -d --pull always --wait
```

`latest` is a mutable convenience tag and does not update a running container
by itself. For a reproducible deployment or rollback, set `ROUTER_IMAGE` and
`CRW_IMAGE` in `.env` to the same immutable release version, such as `0.1.0`,
then run the command above. Camofox and LightPanda stay digest-pinned.

Existing deployments and API endpoints remain compatible: the Compose project,
service and volume identities, routes, mount paths, environment variables, and
`/v2/search` and `/v2/scrape` contracts are unchanged. Old immutable
`hermes-web-retrieval-*` images remain historical rollback artifacts and are not
overwritten or deleted. To adopt the new `fireghost-router` and `fireghost-crw`
packages, update the checkout and explicitly pull/recreate the services with
`docker compose up -d --pull always --wait`.

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
- `dev/compose.yaml` is the legacy developer Compose contract used by Make and
  running production workflows; it builds the router and binds loopback port
  33000. It is intentionally not the public quickstart.
- `dev/compose.staging.yaml` is the developer staging override on loopback port
  33010.
- `crw/docker-compose.yml` belongs to the preserved upstream CRW component, not
  the assembled appliance.

Run `make help` for development commands and `make check` for the complete local
gate. Architecture, migration, updater, rollback, and regression details remain
under [`docs/`](docs/). Upstream CRW source, history, and licensing are preserved
under [`crw/`](crw/); bundled images and dependencies retain their own licenses.

## Publishing a stable appliance release

When Forgejo Actions is enabled, Forgejo publishes the owned router and CRW
images when a stable appliance tag is pushed. Configure repository Actions
secrets `REGISTRY_USERNAME` and `REGISTRY_TOKEN`, where the token can write
packages, then create a tag using the separate appliance namespace so imported
component tags are never reused. The workflow remains dormant while Actions is
disabled; see [`docs/ci.md`](docs/ci.md).

```sh
git tag -a appliance-v0.1.0 -m "Appliance 0.1.0"
git push origin appliance-v0.1.0
```

The release workflow validates the exact `appliance-vMAJOR.MINOR.PATCH` shape,
runs `make check`, builds run-unique image candidates, exercises the isolated
candidate appliance with cloud fallback disabled, and only then promotes both
owned images to `0.1.0`, `0.1`, and `latest`. Full-version tags are
workflow-enforced immutable rollback references. Serialized releases move minor
and `latest` aliases only forward. The CRW image retains its own component
version in OCI metadata while also recording the appliance version.

Forgejo's registry cannot update two package aliases in one transaction. The
workflow retries paired alias updates and restores prior aliases when possible,
but operators requiring atomic reproducibility should deploy matching exact
version tags rather than mutable aliases.

This repository does not claim that Git secret history has been publicly audited
or that DNS, certificates, registry access, routing, and retrieval have been
validated on your destination host. Perform those checks before production use.
