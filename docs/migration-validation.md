# Migration validation

## Candidate `a2cf20c`

Validated on Hermes on 2026-08-24 without changing the production project at
`/root/web-retrieval` or its `127.0.0.1:33000` endpoint.

| Item | Candidate |
| --- | --- |
| Monorepo revision | `a2cf20c5732e572feb03824acb999eb8c7caf850` |
| Branch | `migration/structure` |
| CRW image | `hermes-web-retrieval-crw:1.2.0-monorepo.a2cf20c` |
| CRW image ID | `sha256:f976987d6af3c9f02da4ca13701680507d6dec6e955432801d95078c25f94f14` |
| Staging project | `hermes-web-retrieval-staging` |
| Staging endpoint | `http://127.0.0.1:33010` |

The image labels and `crw-server version` output agree on the monorepo source,
version, complete revision, and UTC build timestamp. The identity gate caught an
initial empty source label before staging; commit `a2cf20c` fixes the Docker
runtime-stage argument scope and strengthens the test so the defect cannot
recur silently.

## Completed gates

- Imported CRW and appliance histories retain their original ancestry and tags.
- Imported component trees equal their source repositories at the frozen
  revisions.
- The normalized rendered Compose configuration retained the baseline hash
  `6ae552468be2c15205480e13b93f580b1ce9f3eeee000e98693222cd32404845`.
- Router formatting, vet, unit, and race tests pass.
- The complete CRW workspace formatting, clippy, unit, integration, and doctest
  gates pass with bounded build artifacts.
- The Hermes appliance suite passes 19/19, including Docker volume
  backup/restore, topology, security, provenance, and stack-lock tests.
- Staging smoke and router live-contract gates pass: search returns five
  results, cached latency is 0.001 seconds, the example and Reddit scrapes
  return 167 and 11,113 characters, and all four concurrent searches pass.
- The direct CRW engine matrix passes 20/20: eight engines cold, eight warm,
  and four concurrently.
- The direct scrape matrix passes 6/6: static HTML, dynamic Reddit, redirect,
  PDF, explicit anti-bot handling, and origin-404 preservation.
- All four staging services are healthy with zero restarts and zero OOM kills.
  A post-gate log scan found no panic, fatal, OOM, HTTP 500, or HTTP 504 lines.
  Camofox reached 705 PIDs under the browser matrix, below its 1,024 PID limit.

Forgejo Actions is disabled on the current Firewire server. The checked-in
workflows describe the intended gates but are not counted as executed evidence.

## Remaining gate

Run the Hermes regression suite against `http://127.0.0.1:33010` and compare it
with the frozen `1.2.0-fw.3` production baseline. Do not merge, publish, update
`deploy/stack.lock.json`, or cut production over until that comparison passes.
The production stack and its immutable CRW digest remain the rollback authority.
