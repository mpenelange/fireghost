# Upgrade implementation and validation — 2026-09-19

Implemented against source baseline `b6bcd7d29bbe59769f07a844da86eb389a75151b`. During this local-validation stage, production was not contacted or changed. Subsequent read-only baseline discovery and isolated amd64/Hermes tests are recorded in the [remote validation report](2026-09-20-remote-validation.md). Historical baseline manifests, default browser pins, and imported Git histories remain intact.

## Implemented upgrades

| Area | Previous | Candidate |
| --- | --- | --- |
| HTML-to-Markdown | htmd 0.5.0 | 0.5.5 |
| PDF conversion | pdf-inspector 0.1.0 / lopdf 0.41.0 | pdf-inspector 1.21.0 / lopdf 0.45.0 |
| Metrics serialization | prometheus 0.13.4 / protobuf 2.28.0 | prometheus 0.14.0 / protobuf 3.7.2 |
| TLS | rustls 0.23.37 / webpki 0.103.9 | rustls 0.23.45 / webpki 0.103.15 |
| HTTP/QUIC | h2 0.4.13 / quinn-proto 0.11.13 | h2 0.4.19 / quinn-proto 0.11.18 |
| Main HTTP client / runtime | reqwest 0.13.2 / tokio 1.50.0 | reqwest 0.13.5 / tokio 1.53.1 |
| Go builder | 1.24.6 | 1.27.1, immutable image index |
| Alpine runtime / backup helper | 3.22.1 | 3.22.6, immutable image index |
| Rust CI / image builder | CI 1.93.1; image 1.93 | 1.98.1, immutable image index |
| CRW runtime image base | mutable bookworm-slim | verified immutable Bookworm index |

Cargo resolved the remaining compatible dependency updates together and recorded checksums in `Cargo.lock`. Major migrations unrelated to these candidates were not forced. The Prometheus migration removes the vulnerable protobuf 2.x resolution. CRW's container build now uses `--locked`. Exact base-image digests and architectures are recorded in [registry evidence](2026-09-19-candidate-image-pins.json).

The PDF adapter keeps its public error surface and encrypted-document rejection, including documents the newer parser can automatically decrypt with an empty password. It bounds structural decompression before extraction, includes preflight in the panic boundary, and limits the page-selection range to actual document pages. OCR remains disabled. Regression fixtures cover encryption, metadata, excessive page counts and compressed object streams. Markdown fixtures cover Unicode numbered text and preserved math delimiters/subscripts.

A selective adaptation of the CRW fork's stale-tab recovery propagates missing-tab errors out of search extraction polling, allowing the existing one-retry recovery to replace the dead tab. Its mock-server test failed before the fix and passed afterward. Broader upstream source replacement, new Byparr services, and additional caching behavior were not adopted without their own contract review.

## Browser candidates

The separate [candidate Compose override](../../dev/compose.upgrades.yaml) pins Camofox 2.4.7 and a verified immutable Lightpanda image index, isolates project state/port, and disables cloud spending. Camofox's candidate is amd64-only; Lightpanda's image is not claimed to correspond to the unavailable 0.2.6 registry tag. [Candidate provenance and commands](2026-09-19-upgrade-implementation.md) explain the constraints. These pins have not replaced the default deployment pins.

Both images passed isolated health checks. Camofox additionally passed tab creation, navigation, JavaScript evaluation and deletion under amd64 emulation. Lightpanda's native-arm64 DevTools endpoint responded successfully and identified `1.0.0-nightly.9608+b1ffc164`. The isolated test containers were removed. These smoke checks do not establish full appliance equivalence. [Browser validation details](2026-09-19-browser-upgrade-validation.md).

## Completed validation

- Original extraction suite passed on the isolated Rust toolchain before the parser migration.
- Updated Rust workspace: **1,160 passed, 0 failed, 18 explicitly ignored** across 66 test targets including doctests.
- Additional release-feature server run (`--features cdp,camofox`): **145 passed, 0 failed, 4 explicitly ignored**. These overlap the workspace tests and are not added to its count.
- `cargo fmt --all -- --check` and `cargo clippy --locked --offline --workspace --all-targets -- -D warnings`: passed.
- PDF adapter integration: 10 passed, including the new decompression/encryption fixtures. Markdown integration: 8 passed.
- PDF-disabled extraction library compiled successfully; its normal dependency tree excludes `pdf-inspector`, `lopdf`, and `ttf-parser`. The PDF-only test filter selects no tests in this configuration, so this is a build/dependency-boundary check, not additional runtime test coverage.
- Go formatting, vet, unit tests, race tests, pinned-image build and non-root runtime health smoke: passed under the installed Apple container runtime. [Detailed Go evidence](2026-09-19-router-upgrade-validation.md).
- Official Go source vulnerability scan: no vulnerabilities found. Rust OSV query of all **513 locked registry entries**: **zero vulnerability advisories**, two unmaintained-package notices (`number_prefix`, `ttf-parser`). These checks do not establish that application logic or deployed images are vulnerability-free. [Rust scan evidence](2026-09-19-upgrade-dependency-validation.md).
- Root appliance suite: 60 passed; backup/restore integration could not run because Docker is absent. Root local regression fixtures: 21 passed. Deployment stack-lock check: passed.
- Standalone Docker Compose parsed and merged the final browser override successfully. Assertions verified the distinct candidate router image tag, isolated project-scoped volumes, only loopback port 33020 published, no browser host ports, and disabled cloud credentials.
- CRW process-exit guard, internal dependency version guard, release configuration audit, 8 release-guard regressions and 11 compatibility-script fixtures: passed.

The host Python 3.9 lacks `tomllib`; script checks were run with the existing Python 3.13 interpreter and its bin directory on PATH. Rust tools and caches were installed under `/tmp`; no global toolchain configuration was changed.

## Remaining release gates and security work

Linux amd64 release builds, Docker Compose backup/restore integration, production-equivalence browser/Hermes gates and actual runtime image vulnerability inventories are not implied by the local checks above. Only the Go candidate container was built locally at this point; Rust compilation/testing used native macOS arm64. The [subsequent remote run](2026-09-20-remote-validation.md) completes the builds, Docker integration and image scans, and records both passing and failing regression checks. Promote browser/default deployment pins only after isolated candidate regression approval. No release images were published.

The application audit's SSRF paths, streaming allocation limits, request admission, cloud-credit accounting and other source findings remain separate remediation work. Dependency upgrades do not close those application defects.
