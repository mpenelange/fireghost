# Google search correction and isolated validation

Final source candidate: `a1f0baf6e73412272dcfac4cf64fe0001765aa7a`, following the [browser-upgrade audit](../2026-09-30-browser-upgrades/README.md). Google search now has explicit failure handling, corrected primary-result extraction, and navigation freshness protection. **Strict live production equivalence remains FAIL; this audit does not approve promotion.** All compilation, test execution, and runtime services were contained in Docker in the `docker-testing` KVM behind docker0; guest Python orchestrated HTTP diagnostics and evidence collection. Production was not changed.

## Confirmed defects and correction

- Recognized Google challenge/consent URLs previously returned a successful empty array. They now produce a typed blocked-search error; a Google-only blocked search returns HTTP `502` with `error_code: "search_blocked"`. If every requested engine fails, the existing last-error policy applies, so a later engine timeout can produce `timeout` instead.
- Failure to reach the requested query, malformed/missing/truncated evaluation replies, and late navigations could also become empty successes or return rows from a moving tab. Search now validates the destination before and after extraction, propagates typed errors, and retains the original deadline and bounded stale-tab recovery. A correctly decoded empty array remains a warned success. Healthy engines still contribute results when Google is blocked.
- `/v2/search` discarded both enrichment warnings and engine warnings. Optional `warning` and `warnings` fields now preserve them without changing the result envelope.
- The Google extractor paired the first link in a broad wrapper with its first heading. It now pairs each heading with its own ancestor or single child link, validates and unwraps destinations, deduplicates URLs, and keeps snippets within a single-heading result card. Fixtures protect legitimate Google documentation and Maps results.
- Root `check-crw` and `test-crw` now run both DOM suites through `test-browser-dom`, using a pinned Node image, read-only source, and disposable dependency installation. The imported CRW workflows remain historical; the existing Forgejo root gate picks up the new target.

The main corrections are `468e558` and `69cc7e7`. The first live comparison exposed two further defects: promoting sitelinks as primary results, and waiting unnecessarily for browser navigation lifecycle completion. The primary-result correction is committed as `1ae3d3e`. `a1f0baf` schedules Google navigation and requires a strictly newer, finite positive `performance.timeOrigin`, exact query, and atomic `{url,timeOrigin,rows}` snapshot before acceptance. Old same-query pages and old challenges remain pending; fresh challenges produce typed errors. Other engines retain direct navigation. The post-evaluation URL guard, original deadline, and bounded timeout cleanup remain. No paid API, outbound proxy service, fingerprint rotation, or CAPTCHA automation was added.

## Deterministic evidence

Witnessed failures preceded the corresponding fixes:

| Gate | RED | Final GREEN |
| --- | --- | --- |
| Google DOM extraction | Original extractor: 1/6 passed; first fix compatibility extension: 5/8 passed | 8/8 passed |
| Initial blocked/stale/malformed/truncated search cases | 0/4 passed | Included in full search suite |
| HTTP blocked/partial/valid-empty contracts | 0/3 passed | 3/3 passed |
| Late navigation before extraction | 0/3 passed | Included in full search suite |
| Nonempty evaluation changes destination | 3 earlier cases passed; 2 acceptance cases failed | Included in full search suite |
| v2 warnings and blocked error mapping | Warning serialization and both blocked mapping tests failed | Included in server unit suite |
| Bare `/sorry` route | Exact Google challenge path was not recognized | Exact route and slash variants recognized; similarly named and unrelated-host routes rejected |

Final navigation contracts witnessed five failures with the Wikipedia control passing before the implementation. The expanded suite now covers 11 cases, including old nonempty/empty documents, old versus fresh challenges, malformed/truncated acknowledgments and frames, fresh empty results, wrong queries, and unchanged Wikipedia transport.

The final committed source passed 122 search tests and three real server HTTP contracts. Workspace clippy with all targets and `crw-server/camofox` passed with warnings denied; formatting passed. The actual root `make test-browser-dom` recipe passed all 12 renderer and nine Google fixtures on the committed checkout. Earlier correction checks also passed 83 core tests, 53 server unit tests, 11 API tests, 13 v2 API tests, 66 root appliance tests and 64 regression-tool tests. Together, the validated scopes contain 285 distinct passing Rust tests, with one pre-existing ignored test. These earlier unchanged scopes were not rerun merely to inflate the final check count.

## Live scope and limits

The first counterbalanced diagnostic made four navigations across owned 2.4.6 and 2.4.8 profiles. Both versions returned Google rows. A scheduled 2.4.8 snapshot was taken before its body hydrated; this diagnostic snapshot is not a search-gate failure or proof of empty results. A separate same-DOM extractor comparison found usable Google pages on both versions. These observations show that access is variable; they do not explain all earlier challenge responses.

The frozen baseline is production revision `c7a58561821496d33f7d46497665db63002022cd`, CRW image `sha256:9d1428de2942e57309f1d359b974c9d23f218f3bc14506e3894544e7809f0c9b` and router image `sha256:4a391dbf71c012430abc5b9ed0c260ec0bd302fef5b6d2d5e306f2429a31d13a`. The images were copied read-only into the test VM. Google validation uses isolated copies, fresh router caches with TTL zero, no cloud key/credits, and immutable browser images. No query/result bodies or profile contents are retained in evidence.

Browser pins: `ghcr.io/redf0x1/camofox-browser@sha256:41e79fb61d50f0a8292b2a51c81ebcb0a2be24d89e9eac970edd12613006ced7` (2.4.6) and `ghcr.io/redf0x1/camofox-browser@sha256:1c1370acdd17f7d0336b64aff4ba4cf31e2b41cc90b23ddcf6135aee81380a55` (2.4.8). The [initial runtime probe](artifacts/google-browser-probe.json) records their actual reported versions and image IDs.

The candidate server is an optimized binary compiled with the Dockerfile's thin-LTO/16-codegen-unit settings and embedded candidate source/version. It is copied into a Google-only validation image based on the frozen CRW runtime; that image identifies the candidate revision and component version in OCI labels. Other bundled binaries and default configuration remain baseline, so this is not a full appliance release. Both test routers use the frozen production router image, so the live scope is the Google retrieval HTTP contract, not qualification of every feature-branch router change. The single-worker configuration allows baseline and candidate requests to share the same owned 2.4.6 user profile, reducing fingerprint/locale differences. The additional candidate uses its own 2.4.8 browser.

The existing Google gate's `minimumResults=1`, `minimumTitleOverlap=0.5`, and 125-second request ceiling remain unchanged. Empty or invalid baseline responses cannot establish equivalence.

The first optimized candidate (`69cc7e7`) returned five results in all four comparisons, but failed title equivalence in three:

| Browser / phase | Baseline seconds | Candidate seconds | Title overlap | Gate |
| --- | ---: | ---: | ---: | --- |
| 2.4.6 cold | 4.746 | 19.510 | 0.0 | FAIL |
| 2.4.6 warm | 1.604 | 20.202 | 0.5 | PASS |
| 2.4.8 cold | 1.385 | 8.030 | 0.0 | FAIL |
| 2.4.8 warm | 2.775 | 5.806 | 0.0 | FAIL |

Read-only replay of both exact extractors on the existing warm documents identified a primary card with five sitelink headings: the candidate expanded six primary results into eleven. A synthetic fixture failed with those five unwanted results and a missing primary snippet (8/9 passed). The corrected selector retains each result card's first heading, deduplicates shared heading identity, and counts only primary headings for snippet containment; all nine fixtures pass. Actual `CamofoxSearchClient` score mapping/merge and `transform_flat(limit=5)` replay on the captured arrays now gives same-document title overlaps of 1.0, 0.8, and 1.0. This isolates extraction correctness; separate live documents can still contain different results.

A counterbalanced transport diagnostic used the same extractor and required a new `performance.timeOrigin` plus the exact query before accepting rows. It found nine rows in every case: 2.4.6 scheduled 1.967 seconds versus direct 10.445 seconds; 2.4.8 scheduled 1.011 versus direct 7.831 seconds. These measurements support removing the blocking lifecycle wait. They do not alone qualify the replacement application's freshness/error behavior; separate deterministic contracts and a final optimized live comparison are required.

The final optimized image is `sha256:60dfe8b9919dbfdb4e57562cbbcb12805cba681311fbacf1fa5b7cce1d60ce4a`, with server binary SHA256 `858893dcab63f38066b8f414ef764c1ba882705135dd913486dade9d609f9d74`. Binary output, image labels, running health responses, and clean checkout all identify `a1f0baf6e73412272dcfac4cf64fe0001765aa7a` / `1.2.0-google-candidate`. An initial build command with incorrect revision metadata was stopped before packaging; the verified final build completed in 3m42s.

| Final browser / phase | Baseline seconds | Candidate seconds | Title overlap | Gate |
| --- | ---: | ---: | ---: | --- |
| 2.4.6 cold | 5.074 | 10.284 | 1.0 | PASS |
| 2.4.6 warm | 4.484 | 9.104 | 0.0 | FAIL |
| 2.4.8 cold | 7.713 | 2.934 | 0.0 | FAIL |
| 2.4.8 warm | 4.972 | 0.810 | 0.0 | FAIL |

Every final request returned HTTP 200 and five valid rows; both version comparisons remain FAIL. Later read-only replay of the final complete documents gave old/new same-document top-five title overlaps of 1.0, 0.75, and 1.0. Both URL and form query matched the fixture, with document language `en` and navigator `en-US`. The **identical production extractor** produced zero title and URL overlap across those different documents, including the two 2.4.6 tabs sharing one profile. This demonstrates served-page variation independently of the changed extractor; it does not prove the cause of every earlier REST mismatch. The control is the first enumerated 2.4.6 document; service ownership is not inferred from tab order.

A temporary transparent proxy inside the test Docker network timed two actual optimized candidate searches on 2.4.6: HTTP 200 / five results in 2.135 and 1.710 seconds. Scheduler replies took 0.225/0.401 seconds, fresh matching snapshot replies 0.569/0.613, and URL checks 0.285/0.185. This did not reproduce the earlier 9–10 second latency; no fixed multi-second wait or transport regression was established. The proxy was diagnostic only and was removed. Results and latency remain variable, so these successful samples do not supersede the failed strict gate or establish availability/performance guarantees.

Inherited limitations remain: worker checkout precedes the per-engine attempt deadline, regional Google domains are not supported by the exact-query matcher, and non-timeout invalid replies do not trigger tab replacement. These need separately scoped contracts; they are not qualifications granted by this audit. The Google changes add no dependency or vendor fork: extractor policy stays in one Rust-owned constant, the document frame is private, and pinned browser upgrades remain subject to the existing HTTP/DOM/live gates.

## Cleanup and evidence

Ten owned containers, five owned volumes, the test network, and introduced images were removed. Docker image and volume inventories returned exactly to their initial 10 images and 14 volumes. All 12 pre-existing containers were preserved, including eight running services, with identical image IDs, running states, and restart counts. Shared BuildKit cache remains 81 records / 5.985 GB and was not pruned. Docker automatically removed digest aliases and an unreferenced base manifest while deleting task images; cleanup resumed safely and the final inventory check passed.

The task's guest checkout, Cargo registry/targets, binaries, browser profiles and archives are removed after transferring the selected sanitized [evidence](artifacts/). Local task archives/scripts are also removed; no binaries or compiled targets were copied to the Mac. [SHA256SUMS](artifacts/SHA256SUMS) covers the evidence files. Source work remains committed on `feat/browser-pipeline`; the original checkout's unrelated changes were preserved.
