# Browser pipeline and isolated VM review

The reusable browser extraction pipeline is opt-in on `feat/browser-pipeline`.
CRW owns browser execution and compaction; the Go router exposes the HTTP
contract and optional MCP tool. The production browser pins and Hermes
deployment were not changed.

The original Mozilla Readability source was imported separately in `3305154`.
The extraction implementation is `cc68a94`; live testing subsequently exposed
and fixed explicit `about:blank` rejection and a Firefox last-tab context race.
The final live-tested CRW implementation is `2bb1773`.
Generated Readability code and fixture dependencies have pinned provenance.

All new builds and tests ran in containers inside the existing `docker-testing`
KVM on docker0. Its normal login is `dev`, not `michael` or `ubuntu`. The setup
folder contains original provisioning plus historical key recovery artifacts.
The private network guards remain useful; normal testing requires neither
recovery machinery nor VM recreation. See `vm-setup-review.json`.

Validation includes component tests, appliance regressions, DOM fixtures,
formatting, Go vet/race checks, Rust clippy, and live REST/MCP calls. The live
modern Reddit thread produced 16 identified comments with parent links out of
24 reported, explicitly marked partial. Incremental collection retained
comments across a same-thread continuation navigation. The live article
produced about 45 KiB of Markdown through Readability.

The tested old Reddit URL redirected to `/login/?reason=lor2`; thread identity
validation rejected it. Old Reddit DOM extraction passes fixtures, but that is
not evidence that anonymous live old Reddit access works. Reported counts and
missing expansion controls do not establish full-thread completeness.

Only task-specific containers, images, network, credentials, and build caches
are removed after validation. Existing Messages workloads and volumes are
preserved. Detailed sanitized results and cleanup verification accompany this
report; raw page responses and runtime credentials are not retained.

Cleanup restored the guest to its original 10 images, 12 containers (eight
running), and 14 active volumes. One anonymous browser volume was identified
by its exclusive `crw-pipeline` profiles and removed by exact name. All eight
Messages container IDs and zero restart counts stayed unchanged; the existing
5.985 GB Docker build cache was preserved. The task workspace, temporary
credentials, raw responses, compiled outputs, and 24 local transfer/helper
files were removed. Source and committed verification evidence remain in the
feature worktree.
