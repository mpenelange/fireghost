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
content. Do not merge, publish, update `dev/stack.lock.json`, or
cut production over until it passes. Production and its immutable CRW digest remain
the rollback authority.

## Browser regression matrix

Before advancing either browser digest, compare the frozen production appliance
with an isolated candidate using the checked-in matrix. Change only one browser
image at a time; keep CRW, the router, configuration, and the other browser at
their production versions so a failure has a single plausible cause. Neither the
matrix nor its runner changes an image pin or deployment.

```sh
mkdir -p artifacts
python3 scripts/browser-regression-gate.py \
  --output "artifacts/browser-regression-$(date -u +%Y%m%dT%H%M%SZ).json"
```

The defaults target the frozen router at `127.0.0.1:33000`, staging at
`127.0.0.1:33010`, and
`tests/fixtures/browser-regression-matrix.json`. Supply `FIRECRAWL_API_KEY` in
the invoking shell if authentication is enabled; it is sent as a bearer token
and is never stored. The output path is mandatory, created exclusively, and
must be new for every run.

Every case pins either `lightpanda` or `camofox` in the `/v2/scrape` request.
The gate requires a successful response, origin status 200, matching
`metadata.renderedWith`, an absolute content floor, required/forbidden markers,
and a candidate-to-production markdown-size ratio. It records only bounded
metadata, lengths, hashes, latency, thresholds, and sanitized URLs; extracted
page bodies, URL queries, and credentials are omitted. The default cases cover
static HTML on both engines, JavaScript DOM execution on both engines, redirect
handling, and the existing Camofox real-world Reddit workload. Because these
are live sites, production is tested immediately before the candidate and acts
as the behavioral control; retain the artifact with the update review.

Artifacts and CLI output report production, candidate, and comparison checks
separately. A production failure leaves the gate failed and marks the relative
comparison as lacking a valid baseline, even when candidate checks pass or the
size ratio meets its threshold. Request failures retain evidence from the other
side and mark the comparison unavailable. All original failure reasons and
thresholds remain enforced; a failed control cannot establish a browser regression.
The JavaScript cases require two rendered authors, Albert Einstein and
J.K. Rowling. Pagination text such as `Next` belongs to navigation removed by
the default main-content extraction and is not a content marker.

For an upstream release, run the matrix once with a cold candidate and again
after the first pass for warm/profile-reuse evidence. Set `ROUTER_SCRAPE_TTL=0s`
on both isolated validation routers before these runs: the default 24-hour scrape
cache would otherwise serve cached responses, and request fields such as
`storeInCache` or `maxAge` do not bypass the router cache. This setting belongs
only to the isolated validation stacks; preserve the frozen production deployment.
The browser gate does not itself prove cache bypass. In CI, `validation.yaml`
applies these settings and runs both passes; see `docs/ci.md`. Then inspect container
restart, OOM, memory, and PID data separately. A pass is required in addition
to the Hermes gate, staging smoke/live-contract checks, component tests, and
appliance tests. Any browser-specific incident URL should first be added as a
new declarative case with non-secret markers and reviewed thresholds.

The Reddit case currently has no valid relative baseline. Production CRW 1.2.0
with Camofox 2.4.6 returns success containing Camofox's 52-character truncation
placeholder, and repaired CRW correctly rejects that oversized result; Firecrawl
Cloud refuses Reddit, so fallback cannot supply one either. Until the matrix gains
a replacement heavy real-world case, a reviewer may judge Reddit on the candidate's
absolute checks only, and must record the missing baseline in the review. See
`docs/audits/2026-10-01-camofox-248-validation/`.
