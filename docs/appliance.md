# Appliance deployment tracks

The supported end-user installation is the root quickstart in the
[`README`](../README.md): copy the root `.env.example`, then run root-level
`docker compose pull` and `docker compose up -d --wait`.

The files under `deploy/` are retained contracts for existing production,
development, Make targets, staging, backups, regression gates, and the immutable
stack lock. `deploy/compose.yaml` builds local source and publishes only the
router on loopback port 33000. `deploy/compose.staging.yaml` isolates a candidate
on loopback port 33010. They are not alternative public installation paths.

CRW source and its upstream history remain under `crw/`. Its component-local
Compose file is upstream developer tooling, not the assembled appliance. See
[`architecture.md`](architecture.md), [`operations.md`](operations.md), and
[`updating.md`](updating.md) for component boundaries and maintenance contracts.
