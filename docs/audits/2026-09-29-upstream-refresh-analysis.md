# Upstream refresh analysis (2026-09-29)

Branch: `chore/upstream-refresh-review` (no merge committed; toolchain-blocked).

## Discovery (`make check-updates`)

| Component | Reviewed | Observed | State |
|-----------|----------|----------|-------|
| crwVendor | 84f12bb | 796d8c9 | changed (163 commits, 1.2.0 -> 1.5.0) |
| crwFoundation | aac7999 | ba9fcac | changed |
| camofoxBrowser | digest | n/a | unavailable (no docker on host) |
| lightpandaBrowser | digest | n/a | unavailable (no docker on host) |

## Why `prepare-crw` reports `conflict`

Its `crw-fork.patch` is `git diff reviewed..candidate` applied with `git apply`.
The monorepo imported the full CRW history (`605827b`), so the fork's commits
sit on top of `84f12bb` as real Git ancestry, and ~100 files were touched by
both sides. A flat patch cannot rebase that; a 3-way merge can.

## Correct method: subtree 3-way merge

`crw/` is the upstream repo root. Build a synthetic commit whose tree equals
`main:crw`, parented on the reviewed base, then `git merge-tree` the candidate:

```
MT=$(git rev-parse "origin/main:crw"); MT=$(git rev-parse "$MT^{tree}")
FAKE=$(git commit-tree "$MT" -p 84f12bb -m synthetic)
git merge-tree --write-tree "$FAKE" 796d8c9
```

Conflicts against `main` (the active baseline): 6 files.
Against the feature branch `feat/fireghost-mcp`: 8 files.
The 2 extra are the parked `0489c3a` upgrade, which is **not** on `main`.

## Conflict triage (against `main`)

| File | Resolution |
|------|------------|
| `Cargo.toml` | take upstream 1.5.0 internal pins |
| `crates/crw-crawl/src/single.rs` | take upstream (superset of fork escalation fix) |
| `crates/crw-server/src/routes/v2/scrape.rs` | take upstream `data.http_error()` (superset of fork `is_target_http_error`) |
| `crates/crw-search/src/camofox_search.rs` | manual: upstream restructured tab lifecycle; reconcile fork's stale-tab/timeout handling |
| `crates/crw-renderer/src/lib.rs` | manual: keep fork `preserve_unobserved_origin_status` call sites under upstream's new flow |
| `docker-compose.yml` | manual: keep fork LightPanda digest pin + upstream telemetry/private-network flags |

## Contract safety

Router depends on: `success`, `data.web[]` (search) and `data.markdown` (scrape).
All three are unchanged in 796d8c9. `/health` and `/metrics` unchanged.

## Blocker

Host has no `docker`, `cargo`, `go`, or `timeout`. `make check*`,
`make test*`, and image-digest probes cannot run here. Resolution and
regression require a Docker host.
