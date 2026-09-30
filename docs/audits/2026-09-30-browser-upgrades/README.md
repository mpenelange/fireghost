# Isolated browser upgrade validation

The maintainability work is implemented on `feat/browser-pipeline`. The browser
HTTP adapter is private to CRW and separate from extraction. An optional dedicated
pipeline endpoint and Compose service let Camofox upgrades be tested independently
of legacy retrieval. Browser images use immutable pins; HTTP, content, lifecycle,
and partial-thread checks are reusable declarative gates.

The final release images were built from `edabf2b0f16b4004f7e531fa27f565fd0b23aad9`
using the actual component Dockerfiles in the Docker test KVM. The root layout
guard correction is `28fe53c`; it changes test scanning only. Runtime source labels,
component versions, actual image IDs, architecture, limits, private browser ports,
configuration hashes, disabled caches, and zero cloud credentials are recorded in
[the final runtime manifest](runtime-manifest-final.json). That manifest, rather
than a gate checkout revision alone, identifies the applications under test.

The frozen baseline is the actual deployed September 12 revision
`c7a58561821496d33f7d46497665db63002022cd`, copied read-only into the VM. Its reviewed
renderer/search settings match the comparison configuration. Legacy browser
retrieval retains Camofox 2.4.6; the opt-in pipeline uses separately pinned 2.4.8.
No production deployment or image pin was changed.

## Results

| Check | Result |
| --- | --- |
| Validation-tool unit tests | 64 passed |
| Root appliance and rendered Compose tests | 66 passed |
| Rust renderer suite, six server pipeline adapters, formatting, clippy | Passed |
| Direct Camofox 2.4.8 HTTP/lifecycle contract | Passed; one scoped 5xx recovery; cleanup passed |
| Article extraction and MCP structured output on final release | Passed; about 45 KiB of Markdown |
| Small, large, and older modern Reddit threads on final release | Passed partial-result contract: 16/24, 31/299, 26/45 comments |
| Legacy REST comparison on final release | 24/26 pairs passed; Google equivalence failed |

[The final pipeline artifact](browser-pipeline-live-fixed-release.json) validates
thread identity, unique IDs, parent/depth relationships, budgets, partial metadata,
and article/MCP content. Reddit expansion stopped after progress stalled; these
results do not establish full-thread completeness. A live continuation initially
produced a child depth of two under a retained parent depth of four. The committed
fix resolves depths from the merged parent graph before every JSON byte-budget
check. Four regressions witnessed RED, then passed: continuation-relative depth,
parents loaded later, cycles, and excessive logical depth.

[The final legacy comparison](appliance-compatibility-fixed-release.json) preserves
the failed Google cases. The baseline returned five results while the candidate
returned none in the first phase. Both returned five in the repeat, but title
overlap was 0.4, below the unchanged 0.5 threshold. An earlier comparison failed
Google on both sides. [A counterbalanced navigation diagnostic](google-navigation-counterbalance.json)
subsequently encountered challenge pages with both production-style scheduled
navigation and candidate direct navigation in two owned 2.4.6 profiles. Google
selectors are unchanged from the baseline. This does not establish a code-level
cause or eliminate a regression; Google equivalence remains unresolved. The gate
compares content, not renderer identity or latency guarantees. Its repeat phases
used routers with both cache TTLs set to zero and zero cloud attempts throughout.

The public example control also exposed a stale fixture: the current body lacks
its page-title heading. The corrected contract independently requires the exact
page title and current documentation text. Slow DNS, header, chunk-framing, and
body tests exposed deadline gaps; the shared transport now bounds the caller
across those stages and prevents delayed DNS from sending a late request.

The renderer GREEN log ends with an invalid follow-on server test-target name.
The renderer stage passed; the corrected server adapter and strict clippy run are
retained separately in `reddit-depth-adapter-clippy.log`. One long release SSH
session disconnected; completed image-build logs and the independently captured
running-image manifest establish the final artifacts.

## Remaining promotion requirements

Obtain valid Google equivalence evidence, retain upstream private-destination,
redirect, and subrequest negative tests against an owned sentinel, and complete
the unchanged browser provenance and Hermes/provider qualification. The public
HTTP contract alone does not establish the upstream destination guard. Full
Reddit expansion remains future work. No production promotion is approved by
this pass. See [the upgrade procedure](../../browser-upgrade-validation.md).

## Cleanup and production observation

[Cleanup verification](cleanup-verification.json) restores the guest's exact
initial 10 images, 12 containers, and 14 volumes. Existing container IDs, image
IDs, running states, and zero restart counts are preserved. Both task projects,
their volumes and networks, eleven introduced images, and the dedicated builder
and cache were removed. The existing 5.985 GB shared Docker build cache remains.
Task source copies, credentials, downloaded dependencies, compiled outputs, and
local transfer files are removed after preserving this sanitized evidence.

Production container and image IDs remained unchanged in read-only observations.
Its existing Camofox restart count increased from seven to nine; the other three
containers remained at zero. Retained Docker events did not explain those
restarts, and the current OOM flag is not historical proof. The production
browser's cause remains unknown. Production was not mutated by this work.
Evidence contains reviewed metadata, counts, hashes, statuses, and test logs;
runtime keys, unrestricted environment dumps, authors, and extracted bodies are
not retained.
