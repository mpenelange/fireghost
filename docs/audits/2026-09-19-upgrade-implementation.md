# Isolated browser candidates and selective CRW reliability update

## Browser candidate stack

`dev/compose.upgrades.yaml` overlays the existing developer stack with separate project-scoped state and a distinct loopback router port (`33020`). It disables cloud credentials and all credit allocations, so candidate validation cannot incur Firecrawl usage. The reviewed developer/default/production pins remain untouched.

Prepare a candidate CRW image through the normal component build, then inspect the merged configuration before starting the isolated stack:

```sh
export MONOREPO_REVISION=$(git rev-parse HEAD)
CRW_IMAGE=<candidate-image-with-immutable-digest> docker compose \
  -p fireghost-upgrades -f dev/compose.yaml -f dev/compose.upgrades.yaml config
CRW_IMAGE=<candidate-image-with-immutable-digest> docker compose \
  -p fireghost-upgrades -f dev/compose.yaml -f dev/compose.upgrades.yaml up -d --build
```

Use Compose 2.24.4+ for the `!override` port replacement. Do not substitute the existing production/developer project name. The base file's named volumes and network become scoped to `fireghost-upgrades`, protecting the baseline browser profiles, cache and ledger. Do not copy production profile state into this test stack.

The router builds under the separate `fireghost-router:upgrade-candidate` tag with candidate version/revision labels, preserving the legacy `web-retrieval-router:dev` tag. Use a candidate source revision that includes these changes when producing release evidence; local uncommitted smoke builds are not release artifacts. `CRW_IMAGE` must identify the separately built CRW candidate, not the baseline image in `dev/.env.example`.

Camofox `2.4.7` was resolved directly at the public registry and pinned to `sha256:afaaf9795af8793f3e6353e9e5dd5b03713b6ffed6e80c1b0a179575322bcff0`. Its config identifies **Linux amd64 only**. The override makes that platform explicit; native ARM64 testing cannot be claimed. ARM hosts require compatible emulation.

Lightpanda `0.2.6` and `v0.2.6` image tags both returned 404. The override therefore pins the observed `latest` index `sha256:8af7584500145dda4beb1a2e1d92a0e4d36c39c690b1bd8b1e2b174c09b363e0`, without associating it with a release tag. Its index contains:

- Linux amd64: `sha256:49e7794b489f95c718224b8d767faf280cd58f84d43af7f4fa15364269995065`
- Linux arm64: `sha256:0dec6fe0cb0ccc99f0c644c926180374005596134e5f2dfb5e31e24ff3f50e22`

Both image versions retain all inherited resource limits, non-published browser ports, security options and healthchecks. Actual compatibility with these inherited settings must pass the existing browser regression matrix before default pins change.

## Upstream review and bounded reliability change

Reviewed [upstream commit 01c6619b](https://github.com/adambenhassen/crw-camofox/commit/01c6619b6da23383c153fdf8376007c4bd9e753c), which restores stale-tab recovery when Camofox evaluation fails. The local implementation has diverged from that patch: it already checks evaluation status through `api_response` and uses a worker pool and non-blocking extraction polling. Applying the upstream patch verbatim would duplicate or conflict with local code.

The remaining local gap is in extraction polling: `run_search` discards every `evaluate_rows` error, including a missing tab. That consumes the polling budget and reports an empty successful search instead of invoking the existing bounded recovery in `fetch`.

The targeted adaptation propagates only errors recognized by the existing `is_stale_tab` predicate (404, server faults, transport loss). `fetch` already retries once with a newly created tab. Empty extraction results and transient parse/timeout errors retain the current polling behavior. A local mock-server regression preloads a warm tab, successfully observes its search URL, loses the tab during evaluation, then requires exactly one replacement tab and a successful result.

No source tree was replaced, no imported history was rewritten, and no Cargo or Go manifest was changed by this work. Larger upstream features (Byparr, clearance reuse and broad render changes) remain outside this bounded backport.

## Validation record

- Read-only public registry requests verified candidate digests and architectures; no production deployment was contacted.
- Standalone Docker Compose successfully validated the final merged candidate configuration: isolated project/volumes, a separate router build tag, only loopback port 33020 published, no browser ports, and blank cloud credentials. Browser execution is recorded separately; Compose parsing does not establish full appliance runtime compatibility.
- RED confirmed: `cargo test --locked -p crw-search fetch_recovers_tab_lost_during_result_evaluation` failed with zero results instead of one before the production fix (log `/tmp/fireghost-search-red.log`). The three-line propagation change was then applied. GREEN confirmed in the upgraded workspace run: the recovery test passed, and the workspace completed with 1,160 tests passed, zero failures, and 18 explicitly ignored tests.
- `rustfmt` and `git diff --check` passed for the change.
