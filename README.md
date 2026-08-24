# Hermes Web Retrieval

This repository is the migration target for the Hermes local-first web retrieval appliance. It will combine the CRW/Camofox application, Firecrawl-compatible router, deployment topology, and appliance-level compatibility gates while preserving component boundaries.

## Migration status

Migration is in progress. This repository is not yet the production source of truth. The existing `michael/crw-camofox` and `michael/web-retrieval` repositories and the deployed `1.2.0-fw.3` appliance remain canonical until the equivalence and Hermes regression gates pass.

See [the migration plan](docs/migration.md) for the frozen baseline, target structure, acceptance criteria, and rollback policy.
