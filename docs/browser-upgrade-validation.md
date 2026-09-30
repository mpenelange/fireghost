# Camofox upgrade validation

A Camofox image update must preserve the browser HTTP contract and the appliance's
retrieval behavior. Run the direct contract gate first, then the existing browser
regression matrix and Hermes checks. A contract pass establishes API compatibility;
it does not establish content equivalence or approve deployment.

## Freeze the runtime inputs

Create a disposable candidate in the Docker test VM. Keep the frozen baseline and
production deployment intact. Change one browser image at a time, identified by
an immutable registry digest. Record the actual image ID, registry digest, CPU
architecture, container limits, runtime configuration, CRW revision and component
version, router revision and component version, and the fixture revision. Capture
only reviewed configuration fields; never archive credentials or an unrestricted
container environment dump.

The direct gate records its script, shared transport, and fixture hashes, source revision, expected image digest,
expected version, observed browser health version, budgets, and timestamp. The
image reference is supplied by the operator: HTTP health cannot prove which image
digest is running. Verify it against the test container's image and retain the
sanitized image inspection beside the artifact. A release tag alone is insufficient.

Camofox 2.4.8 or newer is required by the browser pipeline. Its destination guard
checks browser navigation and subrequests. Keep private-network access disabled;
the contract uses the public `https://example.com/` control and never supplies
network bypass flags. The default deployed browser pin remains an independent
deployment decision.

## Use a separate pipeline browser

The optional CRW configuration block selects a dedicated browser for
`/v2/browser/scrape`:

```toml
[renderer.browser_pipeline]
base_url = "http://camofox-pipeline:9377"
```

When this block is absent, the pipeline uses `renderer.camofox` and still requires
its guarded version floor. A dedicated endpoint lets the pipeline use Camofox
2.4.8 while legacy search and scrape retain their baseline browser. The Go
router separately requires `ROUTER_BROWSER_PIPELINE_ENABLED=true` to expose the
REST endpoint and MCP tool; its default remains disabled.

The optional [Compose overlay](../dev/compose.browser-pipeline.yaml) mounts
[dedicated CRW configuration](../deployment/crw.browser-pipeline.toml), runs
`camofox-pipeline` with an immutable 2.4.8 pin and its own profile volume, and
keeps the base Camofox service separate. The candidate project defaults to
`fireghost-browser-pipeline`; its router binds only to `127.0.0.1:33030`, enables
MCP and the pipeline, and disables cloud fallback. Candidate CRW and router
image tags are separate from baseline images.

Inside the VM, supply the existing base environment, a candidate
`MONOREPO_REVISION`, and `ROUTER_API_KEY`. The base `CRW_IMAGE` variable is still
required during Compose interpolation even though the overlay supplies the
candidate image. Compose 2.24.4 or newer is required for its explicit port
override. Validate without printing credentials:

```sh
docker compose --project-directory dev --env-file dev/.env \
  -f dev/compose.yaml -f dev/compose.browser-pipeline.yaml config --quiet
```

The overlay accepts `BROWSER_PIPELINE_ROUTER_VERSION`,
`BROWSER_PIPELINE_CRW_VERSION`, `BROWSER_PIPELINE_BUILD_DATE`, and
`BROWSER_PIPELINE_ROUTER_HOST_PORT`. Adding `dev/compose.staging.yaml` last uses
that overlay's `ROUTER_HOST_PORT` setting instead. Keep all builds and validation
runs inside the isolated test VM.

## Run the direct HTTP contract

Run this command inside the isolated VM against its candidate browser. Supply the
exact expected version and independently verified immutable image reference.

```sh
python3 scripts/browser-http-contract-gate.py \
  --browser-url http://127.0.0.1:33777 \
  --expected-version 2.4.8 \
  --image-reference 'ghcr.io/redf0x1/camofox-browser@sha256:REPLACE_WITH_VERIFIED_DIGEST' \
  --repo-revision REPLACE_WITH_TESTED_REVISION \
  --output artifacts/camofox-http-contract.json
```

Set `CAMOFOX_API_KEY` in the invoking environment when browser authentication is
enabled. The gate sends it as a bearer token and never stores it. The output file
must be new. Its exit status is nonzero if any contract or cleanup check fails.

The declarative [contract fixture](../tests/fixtures/browser-http-contract.json)
defines the version floor, public target, required text, evaluation expressions,
consecutive cycles, and budgets. The gate verifies health, blank creation with the
URL omitted, navigation and readiness, both object and JSON-string evaluation
results, scoped tab listing, tab deletion, and awaited session deletion. Two
consecutive cycles reuse one unique profile. A completed create response with
HTTP 5xx permits one reset of that owned session and one retry; a timeout or
transport failure permits no creation retry. The artifact reports this recovery
count so an upstream lifecycle change remains visible.

Requests and responses have explicit time and size bounds, HTTP redirects are not
followed, and administrative calls remain scoped to the gate's random user ID.
The final cleanup has a separate bounded grace period and its result affects the
gate. An interrupted process cannot guarantee cleanup. If an artifact or browser
log supplies its scope ID, remove only that session; otherwise remove the disposable
test stack. Retain failure
artifacts as evidence. They contain check identifiers, status codes, lengths,
hashes, latency, and sanitized runtime inputs rather than page bodies, arbitrary
upstream errors, authorization headers, or URL queries.

## Check retrieval behavior

Run [the legacy REST compatibility gate](../scripts/appliance-compatibility-gate.py)
against isolated copies of the actual deployed baseline and the candidate. Its
[matrix](../tests/fixtures/appliance-compatibility-matrix.json) checks static,
JavaScript, redirect, PDF, and search content, repeated calls, and concurrent
searches. It tolerates additive fields and older responses without renderer
metadata. It compares content and search title overlap, requires valid baseline
responses, and verifies zero cloud attempts; it does not prove renderer identity.

```sh
python3 scripts/appliance-compatibility-gate.py \
  --baseline-url http://127.0.0.1:33000 \
  --candidate-url http://127.0.0.1:33030 \
  --output artifacts/appliance-compatibility.json
```

Supply `ROUTER_API_KEY` when authentication is enabled. Set both
`ROUTER_SEARCH_TTL=0s` and `ROUTER_SCRAPE_TTL=0s` on the isolated comparison
routers when the cold, warm, and concurrent phases are intended to exercise
retrieval. Repeated requests alone do not prove cache bypass. The gate retains
failures without recording search queries or extracted bodies.

Use the unchanged [browser regression matrix](migration-validation.md#browser-regression-matrix)
for baseline and candidate content comparisons through `/v2/scrape`. It already
checks renderer provenance, required and forbidden text, content floors, and
relative Markdown size. Run it cold and warm with `ROUTER_SCRAPE_TTL=0s` on the
isolated routers so both runs reach the browser. Preserve its thresholds and
distinguish a failed baseline from a candidate regression.

For the opt-in `/v2/browser/scrape` and MCP `browser_scrape` surface, also run the
component, appliance, and live pipeline checks with the candidate image. Keep
the router feature enabled only in the validation stack. Inspect partial Reddit
results, identity and parent links, stop reasons, and cleanup; loaded comments
must not be reported as the complete public thread. The public example control
in the HTTP gate deliberately leaves Reddit extraction to those existing tests.

Retain the direct contract artifact, legacy REST comparison, cold and warm browser comparison artifacts,
Hermes/provider evidence, component test logs, and sanitized runtime manifest with
the upgrade review. Inspect restarts, OOM events, memory, PID counts, and leftover
test profiles and volumes before removing the disposable resources. Update an
image pin only after the migration's equivalence and regression approval.
