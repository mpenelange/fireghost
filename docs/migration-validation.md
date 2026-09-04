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

## Durable Hermes regression gate

Keep the frozen production router on `http://127.0.0.1:33000` and the isolated
candidate on `http://127.0.0.1:33010`. Run the deterministic offline tests first,
then choose a new explicit timestamped artifact path for the live comparison.
The gate creates artifacts exclusively and refuses to overwrite an existing file
or follow an existing symlink:

```sh
make check
mkdir -p artifacts
make hermes-regression \
  OUTPUT="artifacts/hermes-regression-$(date -u +%Y%m%dT%H%M%SZ).json"
```

`FIRECRAWL_API_KEY` may be supplied in the parent shell when the routers require
authentication. For the installed production profile, source it only in that
shell (`set -a; . /root/.hermes/.env; set +a`); the gate never reads or edits that
profile. Each probe gets a separate temporary `HOME` and `HERMES_HOME` containing
an ephemeral direct-Firecrawl selection. Managed-gateway and provider-selection
environment variables and unrelated parent credentials are excluded by a minimal
allowlist; only basic process values, isolated homes, query, direct key, and endpoint
are passed to the installed provider subprocess. The key is never printed or written
to the artifact. Override the interpreter reproducibly with
`HERMES_PYTHON=/path/to/python make hermes-regression ...` or the script's
`--hermes-python` option.

The default query can be replaced with `--search-query` (or
`HERMES_REGRESSION_SEARCH_QUERY=... make hermes-regression`) to perform a
fresh-query comparison; choose a query not already present in either router
cache when cold-path evidence is required. A bounded, secret-redacted query and
cache/local/cloud deltas are retained in the artifact.

The gate searches for `Python programming language official documentation` with
Hermes's normal `query` plus `limit` call (without custom engines) and makes a
markdown extraction call for `https://example.com/`. It compares
provider availability, success, bounded nonempty result counts, required result
fields, the representative extraction title and content size, and a 120-second
absolute candidate latency ceiling. Candidate search titles must overlap at least
half of the production titles, and candidate extracted content must retain at least
90% of the production content size. Production and candidate latency are recorded
but are not treated as statistically comparable because their persistent caches
may differ and the search engine is live. Google redirect-token URL identity is
intentionally not compared.

For both routers, the gate records request, cache-hit, local-attempt, and
cloud-attempt counter snapshots and deltas, bracketed immediately around each probe.
A successful search and scrape must each produce exactly one 2xx request-counter
increase on the intended router; extra concurrent traffic fails closed rather than
being mistaken for probe evidence. The CLI rejects swapped, duplicate, or noncanonical
production/candidate endpoints. Cache hits are
allowed; both sides need not be cache misses. Production cloud activity is recorded
as the behavioral baseline; any candidate cloud search or scrape increase fails.
Every required metric family and label is fail-closed when absent.

The ignored JSON artifact contains UTC time, repo HEAD, endpoint and installed
Python/provider identities, bounded and secret-redacted probe inputs and summaries
(never full extracted page bodies), metrics snapshots and deltas, thresholds, and
all pass/fail reasons. Timeouts and subprocess, JSON, HTTP, or metric failures also
write a sanitized failing artifact without stderr, secrets, tokens, or config
content. Do not merge, publish, update `deploy/stack.lock.json`, or
cut production over until it passes. Production and its immutable CRW digest remain
the rollback authority.
