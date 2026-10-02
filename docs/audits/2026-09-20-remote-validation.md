# Isolated amd64 and Hermes validation — 2026-09-20

**Disposition:** upgrade work is parked at the operator's request. Keep the current working production deployment; retain these changes and reports for future review. No promotion or release is authorized by this validation record.

Promotion remains blocked pending regression results and review. No production deployment or Hermes profile was changed. The earlier [local validation report](2026-09-19-upgrade-results.md) describes implementation and local tests; this report records the subsequent remote checks.

| Isolated candidate | Live contract | Installed Hermes provider | Browser gate |
| --- | --- | --- | --- |
| Application upgrades, existing browsers | Pass | Pass | Fails shared baseline assertions |
| Lightpanda upgrade only | Pass | Pass | Fails shared baseline assertions |
| Camofox 2.4.7 only | Search timeout | Search timeout | Fails; baseline limitations also present |
| All upgrades together | Search timeout | Search timeout | Fails; native Reddit candidate also unsuccessful |

Recommendation: retain the application dependency changes for review, withhold the Camofox pin, and resolve browser-gate provenance/fixture issues before approving promotion. Lightpanda passed its contract and Hermes checks but has not passed the full browser gate. Runtime image security findings require separate triage.

## Preserved baseline and candidate

The running appliance is the `web-retrieval` Compose project on `michael@docker0`. Hermes runs separately in QEMU, reachable at `root@hermes`. Its installed provider was exercised with `/usr/local/lib/hermes-agent/venv/bin/python3` (Python 3.11.15, Hermes 0.21.3, firecrawl-py 4.17.0).

Before testing, production image identities, configuration hashes, container identities, start times and restart counts were captured under `/home/michael/fireghost-validation/20260920-upgrade-01/baseline`. Full container inspection is private because it can contain credentials. Existing images received retention tags. Production's revision is `c7a58561821496d33f7d46497665db63002022cd`; it predates local source HEAD `b6bcd7d29bbe59769f07a844da86eb389a75151b`. Consequently the application comparison includes intervening repository changes as well as this dependency upgrade work.

The candidate source was copied without ignored credentials into a full-history remote clone and committed on its private validation branch as `d1c1acbbcf0edf8a7b5820d1121bf78074cd6c16`. No branch or image was published. Both amd64 image builds succeeded with component-version and monorepo-revision labels:

| Component | Immutable local image ID | Component version |
| --- | --- | --- |
| Router | `sha256:10c32ef9cabea4432af579485a73715a0d98a97b38e701436bce3d1434517a67` | `1.0.0-upgrade.20260920` |
| CRW | `sha256:38e1148065ed927986ff660fa353cb9e19726307d821fd9077f6845ed1f556b8` | `1.2.0-upgrade.20260920` |

The frozen clone (`fireghost-validation-baseline-20260920`) exposes only loopback port 33000. The candidate (`fireghost-validation-candidate-20260920`) exposes only loopback port 33010. Each uses fresh project-specific volumes and a separate private network, without the production proxy network. Synthetic credentials and disabled cloud access prevent spending. Resource limits follow the deployed configuration. Candidate state is recreated between phases.

Temporary loopback SSH forwards allow the installed Hermes provider to reach those two isolated routers. The provider probe uses temporary HOME/HERMES_HOME directories. In gate artifacts the key `production` means **the frozen isolated clone**, not the live production endpoint.

## Completed checks

- Docker-backed appliance suite: **61 passed**, including backup/restore integration.
- Go formatting, vet, unit and race tests in the pinned Linux builder: passed.
- Router and CRW Linux amd64 builds: passed, images loaded locally on docker0.
- Frozen baseline live contract: passed.
- Runtime image inventories: completed with Trivy; see [image scan report](2026-09-20-image-validation.md). Dependency-only scans do not cover these OS findings.

## Camofox-only phase

New Camofox 2.4.7 was paired with the frozen router, CRW and Lightpanda images.

- Live contract failed: the Wikipedia search timed out after 60 seconds.
- Installed Hermes provider failed search after approximately 60 seconds; baseline search and both extraction probes succeeded. No candidate cloud requests were recorded. [Hermes artifact](2026-09-20-validation/camofox-hermes.json).
- Logs show tab creation and navigation evaluation completing, followed by an evaluation request that does not finish. Read-only inspection suggests an unbounded navigation-cleanup evaluation in Camofox may strand the request even though the primary evaluation has a timeout. This is a diagnosis to investigate, not a proven root cause.
- A final isolated direct-browser diagnostic created a fresh tab, scheduled the same Wikipedia navigation and observed its URL. Both `document.readyState` and the exact Wikipedia extraction expression then exceeded their separate three-second client deadlines. This does not isolate the issue to the extraction expression or establish the server-side root cause. [Diagnostic](2026-09-20-validation/camofox-evaluate-diagnostic.jsonl).
- Native browser comparison retrieved identical Markdown hashes for static pages, JavaScript quote pages and the redirect on both sides. Candidate Reddit returned substantial content while baseline Reddit did not meet the content threshold. The gate still failed its declared assertions. [Native browser artifact](2026-09-20-validation/camofox-native-browser.json).

## Application-only phase

The upgraded router and CRW images, paired with the frozen browser images, passed the live contract and the installed Hermes provider gate. Both search and extraction succeeded, each gained the expected successful router request, and candidate cloud-attempt deltas were zero. Candidate provider calls completed in 1.81 seconds total; the baseline took 0.39 seconds. These are single probe timings with existing caches, not a controlled performance benchmark. [Hermes artifact](2026-09-20-validation/apps-hermes.json).

The original browser gate remains red because of the existing v2 provenance omission. The native comparison also remains red on the quote required-text assertions and insufficient Reddit content on both sides. Its five static/JavaScript/redirect cases produced matching baseline/candidate Markdown hashes. This phase supports keeping the application upgrades independently of new browser pins, but does not constitute full regression approval. [Native browser artifact](2026-09-20-validation/apps-native-browser.json).

## Lightpanda-only phase

The Lightpanda-only phase passed the live contract and the installed Hermes provider gate, including zero cloud-attempt deltas. Its native browser comparison failed the same quote/Reddit baseline content assertions as application-only testing. [Hermes artifact](2026-09-20-validation/lightpanda-hermes.json), [native browser artifact](2026-09-20-validation/lightpanda-native-browser.json). A duplicate native gate invocation encountered the artifact's exclusive-create guard; the first completed JSON result is retained and is not treated as a second independent result.

## Combined phase

Upgraded router, CRW, Camofox and Lightpanda together reproduced the 60-second search timeout in both the live contract and the installed Hermes provider gate. Extraction succeeded and candidate cloud-attempt deltas remained zero. The application upgrades therefore do not resolve the observed Camofox search failure. [Hermes artifact](2026-09-20-validation/combined-hermes.json).

The supplementary native browser comparison also failed: in addition to the shared quote assertions and weak baseline Reddit content, candidate Reddit scraping was unsuccessful. [Native browser artifact](2026-09-20-validation/combined-native-browser.json).

## Gate limitations discovered by live testing

The checked-in browser gate calls `/v2/scrape` and requires `metadata.renderedWith`. Both the frozen and candidate CRW v2 adapters omit that field. Therefore that gate cannot establish renderer provenance for either image and its original failures are retained.

A supplementary run uses the same matrix and thresholds through CRW's native `/v1/scrape` inside the isolated containers, which preserves renderer provenance. It does not replace router contract coverage. Both versions also fail a required-text assertion on the JavaScript quote fixture despite identical Markdown output; baseline Reddit content is unreliable. These baseline failures prevent claiming full equivalence from a green aggregate gate. Assertions have not been relaxed to obtain a pass.

## Evidence location

The final read-only production check found all four original container IDs and image IDs unchanged and all services healthy. Router, CRW and Lightpanda retained their start times and zero restarts. The existing production Camofox restarted during the window: its restart count increased from 20 to 21. No production restart action was issued; the cause was not established by this run. Do not interpret the absence of deployment changes as proof of uninterrupted production runtime. [Identity comparison](2026-09-20-validation/production-after.json).

Remote source, helper scripts, Compose phase definitions, build/test logs and scan reports remain under `/home/michael/fireghost-validation/20260920-upgrade-01`. Hermes probe artifacts are initially recorded under `/tmp/fireghost-validation-20260920/artifacts`. Sanitized comparison results are retained alongside this report. Private environment and full-inspection files must not be committed.

After testing, both isolated Compose projects and the dedicated validation builder were stopped. Their images, volumes and evidence were retained. Temporary SSH forwards were closed and the temporary synthetic credential was removed from Hermes and the local transfer file. No production cutover was performed.
