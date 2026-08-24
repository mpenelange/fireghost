# Monorepo migration

## Objective

Consolidate the tightly coupled CRW application and Hermes web-retrieval appliance into one repository without changing the behavior of the currently working system. The monorepo removes the manual cross-repository digest handoff while retaining the HTTP and container boundaries that make the components independently testable.

## Frozen production baseline

The migration reference is the appliance validated and deployed on 2026-08-24:

| Component | Immutable reference |
| --- | --- |
| CRW source | `michael/crw-camofox@94f792a197433ce722ab78c2182b7fb988175502` |
| CRW source tag | `1.2.0-fw.3` |
| CRW image | `git.firewire.cc/michael/crw-camofox@sha256:3898cae0970787b095d4348578c0219c9b2ff2461bd8b419e67ef2b47d701115` |
| Appliance source | `michael/web-retrieval@9f142ce549d58ee50a6f7a8320c84f5404c09a74` |
| Router endpoint | `http://127.0.0.1:33000` |

The prior `1.2.0-fw.2` image and the production `.env` backup remain the operational rollback path.

## History-preserving import

The first migration checkpoint imports the complete histories as two independent merge commits:

- `michael/crw-camofox` canonical branch into `crw/`.
- `michael/web-retrieval` `main` branch into `appliance/`.

Original release tags are retained. The existing repositories remain available and unchanged throughout migration.

## Target structure

The imports deliberately precede mechanical reorganization. The intended final structure is:

```text
crw/                 Rust CRW/Camofox application
router/              Go Firecrawl-compatible router
deploy/              Compose topology and runtime configuration
tests/live/           Whole-appliance compatibility and regression gates
scripts/              Root developer and release commands
docs/                 Architecture, operations, and migration records
```

The temporary `appliance/` import has been eliminated. Its contents moved to `router/`, `deploy/`, and the root test/documentation locations in small, behavior-neutral commits.

## Release ownership

One monorepo commit describes the tested appliance. Components keep their own versions, tests, and images, while `deploy/stack.lock.json` records the source provenance and exact external container digests assembled by Compose. The imported router baseline remains a documented legacy local build; the first monorepo release will replace it with a published immutable router digest. Production consumes only immutable registry digests once that transition is complete.

## Migration gates

1. Import both histories and verify ancestry and baseline trees.
2. Move paths mechanically and prove that rendered Compose configuration and component sources remain equivalent.
3. Add path-aware Rust and Go CI plus one mandatory appliance gate.
4. Build isolated images labeled with the monorepo revision and validate cold start, search, scrape, caching, concurrency, limits, and rollback behavior.
5. Have Hermes compare the candidate against the frozen regression baseline.
6. Cut over only after explicit approval; tag the first monorepo release and retain the old repositories read-only during stabilization.

Any failed gate leaves production on the frozen baseline. The legacy repositories are archived only after the monorepo has completed a stabilization window; they are never deleted as part of migration.
