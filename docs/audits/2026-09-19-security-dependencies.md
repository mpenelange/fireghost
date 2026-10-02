# Dependency security audit — 2026-09-19

## Scope and evidence

Read-only inventory/advisory audit of the checked-out source and recorded container baseline. No dependency changes, deployments, live service probes, or exploit execution. Versions below are **locked/source-build versions**, not independently verified installed production versions. Source review and upgrades are covered in companion reports.

Queried OSV `/v1/querybatch` for all **523 crates.io package/version entries** in `crw/Cargo.lock`, `Go:stdlib@1.24.6` from the router Dockerfile, and the declared lower bound `npm:marked@18.0.0`. Retrieved all **74 distinct matching advisory records**, including aliases, through `/v1/vulns/{id}`. The Cargo results contain **18 distinct RustSec advisories across 14 package/version entries** (rand appears twice); **two are maintenance notices**, not vulnerabilities. The Go result contains **45 version-level matches**, not 45 proven reachable vulnerabilities. The npm result is a possible resolution, not an observed installation.

Native `cargo`, `cargo-audit`, `go`, `govulncheck`, `docker`, `trivy`, `grype`, and `osv-scanner` were absent from PATH. Python 3.9.6 and curl were available. Initial sandbox network lookup failed with curl exit 6 (DNS); an approved network escalation succeeded, with exit 0 and 525 response entries. Full OSV records were downloaded successfully. Source-only dependency graph inspection does not resolve enabled Cargo features or perform Go call-graph analysis.

## Priority findings

### P1 — PDF input can reach a known process-aborting parser

`crw/crates/crw-extract/Cargo.toml:17` enables PDF support by default, and line 26 specifies lopdf 0.41. Both the direct dependency and `pdf-inspector 0.1.0` resolve to **lopdf 0.41.0**. `crw/crates/crw-extract/src/pdf.rs:236` calls `Document::load_mem` in the decompression preflight. That call precedes the later `catch_unwind` wrapper; even wrapping it would not catch the stack-overflow abort described by upstream. Small deeply nested PDFs can therefore terminate the process despite byte/decompression limits. Reachability is strongly supported by source, but not reproduced here. **Upgrade every lopdf resolution to >=0.42.0**, coordinating the pdf-inspector dependency; confirm no 0.41 copy remains and add an isolated subprocess regression test. [RustSec RUSTSEC-2026-0187](https://rustsec.org/advisories/RUSTSEC-2026-0187.html), [upstream report](https://github.com/J-F-Liu/lopdf/issues/502).

### P1 — Router rebuilds embed an obsolete vulnerable Go standard library

`router/Dockerfile:2` pins Go **1.24.6**, and its CGO-disabled binary embeds that standard library. Updating Alpine alone does not fix it. The router calls `http.Client.Do` at `router/internal/upstream/client.go:42`; HTTP/TLS advisory paths are relevant to upstream connections, with specific preconditions still requiring review. For example **GO-2026-4918** concerns a malicious HTTP/2 peer causing an infinite write loop (fixed in 1.25.10/1.26.3), and **GO-2026-6090** addresses excessive post-handshake TLS messages (fixed in 1.25.13/1.26.6). Other matches concern APIs such as templates, archives, or Windows-only behavior and should not automatically be attributed to this Linux router. [Go HTTP/2 advisory](https://pkg.go.dev/vuln/GO-2026-4918), [TLS advisory](https://pkg.go.dev/vuln/GO-2026-6090).

Move the builder to the latest patch of a supported release (Go **1.27.1** was available during verification), pin its digest, rebuild, and run govulncheck against the actual source/binary plus the mandated Docker Go tests/vet/race checks. Go supports the two newest major releases. [Go downloads](https://go.dev/dl/), [security policy](https://go.dev/doc/security/).

### P2 — Rust transport and supporting dependencies need a security refresh

Prioritize rustls **0.23.37 -> >=0.23.45**, rustls-webpki **0.103.9 -> >=0.103.13**, and h2 **0.4.13 -> >=0.4.16**. Rustls explicitly preserves handshake transcript authentication: its advisory is not evidence of an arbitrary MITM handshake forgery. h2's issue requires queued empty frames on streams that are not drained. Certificate-name constraints and CRL configuration affect webpki exposure. [Rustls advisory](https://rustsec.org/advisories/RUSTSEC-2026-0285.html), [h2 advisory](https://rustsec.org/advisories/RUSTSEC-2026-0258.html).

The complete Rust table below also includes conditional findings: AWS-LC's X.509/CRL APIs are not necessarily called by rustls, which uses webpki verification; quinn is in the lockfile but HTTP/3 is not explicitly enabled in the workspace reqwest feature list; protobuf comes from prometheus, while inspected metrics code uses text encoding rather than untrusted protobuf decoding; anyhow is referenced by test/WASM-related packages; rand requires a reentrant custom logging configuration; crossbeam and event-listener require particular API usage. These are reasons to prioritize accurately, not to suppress the advisory without feature/call-graph evidence.

### P2 — JavaScript/Python dependencies lack a reproducible audited resolution

`crw/package.json` allows marked **^18.0.0**, without a committed npm lockfile. Versions 18.0.0 and 18.0.1 have a parser denial of service fixed in **18.0.2**. The inspected use is documentation generation (`crw/scripts/build-docs-pages.mjs:208`), not the Rust retrieval runtime. A fresh install can already resolve a fixed version, so this audit does **not** establish that a vulnerable marked is installed. Raise the floor to >=18.0.2, commit a lockfile, and use npm ci for that workflow. [Maintainer advisory GHSA-6v9c-7cg6-27q7](https://github.com/markedjs/marked/security/advisories/GHSA-6v9c-7cg6-27q7).

`crw/conformance/pyproject.toml` declares `firecrawl-py>=4.0` with no resolved Python lockfile. There is insufficient evidence to assert installed SDK or transitive package versions. Resolve and lock the conformance environment, then run pip-audit/OSV against that resolution. The documentation indexing workflow also installs dependencies dynamically and requires a resolved audit.

### P1 verification gap — Browser engine and container package inventories

`dev/stack.lock.json` records Camofox 2.4.6, Node 22.22.3, camoufox-js 0.8.5, playwright-core 1.58.1, and **Camoufox/Firefox 135.0.1-beta.24**. It is an August baseline, not a fresh container inspection. Mozilla shipped additional security fixes in Firefox 136 and subsequent releases; that old engine identity requires upstream backport/provenance verification and an actual image scan. Do not assume changing the Camofox wrapper or Playwright updates the bundled browser. [Mozilla Firefox 136 advisory](https://www.mozilla.org/en-US/security/advisories/mfsa2025-14/).

No Docker daemon/scanner or image SBOM was available, so this audit makes **no confirmed OS/container CVE count**. Scan all immutable deployed/candidate digests, including Camofox, LightPanda, and owned images; inspect actual browser binary versions and distro package inventories. The router runtime pins Alpine 3.22.1; CRW uses mutable rust:1.93-bookworm and debian:bookworm-slim bases and installs apt packages without an immutable package snapshot. Pin candidate bases and scan resulting artifacts without modifying the preserved production baseline.

## All RustSec matches

Fixed versions are advisory minimums, not a guarantee that every newer version is free of all advisories. Maintenance notices have no patched version. Duplicate GHSA aliases are omitted.

| Locked package | Advisory | Description | Minimum fixed version(s) |
| --- | --- | --- | --- |
| anyhow 1.0.102 | [RUSTSEC-2026-0190](https://rustsec.org/advisories/RUSTSEC-2026-0190.html) | Unsoundness in `Error::downcast_mut()` | 1.0.103 |
| aws-lc-sys 0.38.0 | [RUSTSEC-2026-0044](https://rustsec.org/advisories/RUSTSEC-2026-0044.html) | AWS-LC X.509 Name Constraints Bypass via Wildcard/Unicode CN | 0.39.0 |
| aws-lc-sys 0.38.0 | [RUSTSEC-2026-0048](https://rustsec.org/advisories/RUSTSEC-2026-0048.html) | CRL Distribution Point Scope Check Logic Error in AWS-LC | 0.39.0 |
| crossbeam-epoch 0.9.18 | [RUSTSEC-2026-0204](https://rustsec.org/advisories/RUSTSEC-2026-0204.html) | Invalid pointer dereference in `fmt::Pointer` impl for `Atomic` and `Shared` when the underlying pointer is invalid | 0.9.20 |
| event-listener 5.4.1 | [RUSTSEC-2026-0221](https://rustsec.org/advisories/RUSTSEC-2026-0221.html) | `event-listener` allows `!Send` tags to cross thread boundaries via `StackSlot` | 5.4.2 |
| h2 0.4.13 | [RUSTSEC-2026-0258](https://rustsec.org/advisories/RUSTSEC-2026-0258.html) | h2 unbounded empty DATA frames | 0.4.16 |
| lopdf 0.41.0 | [RUSTSEC-2026-0187](https://rustsec.org/advisories/RUSTSEC-2026-0187.html) | Stack overflow in lopdf via deeply nested PDF objects | 0.42.0 |
| number_prefix 0.4.0 | [RUSTSEC-2025-0119](https://rustsec.org/advisories/RUSTSEC-2025-0119.html) | number_prefix crate is unmaintained | Unmaintained; evaluate replacement |
| protobuf 2.28.0 | [RUSTSEC-2024-0437](https://rustsec.org/advisories/RUSTSEC-2024-0437.html) | Crash due to uncontrolled recursion in protobuf crate | 3.7.2 |
| quinn-proto 0.11.13 | [RUSTSEC-2026-0037](https://rustsec.org/advisories/RUSTSEC-2026-0037.html) | Denial of service in Quinn endpoints | 0.11.14 |
| quinn-proto 0.11.13 | [RUSTSEC-2026-0185](https://rustsec.org/advisories/RUSTSEC-2026-0185.html) | Remote memory exhaustion in quinn-proto from unbounded out-of-order stream reassembly | 0.11.15 |
| rand 0.8.5 | [RUSTSEC-2026-0097](https://rustsec.org/advisories/RUSTSEC-2026-0097.html) | Rand is unsound with a custom logger using `rand::rng()` | 0.8.6, 0.9.3, 0.10.1 |
| rand 0.9.2 | [RUSTSEC-2026-0097](https://rustsec.org/advisories/RUSTSEC-2026-0097.html) | Rand is unsound with a custom logger using `rand::rng()` | 0.8.6, 0.9.3, 0.10.1 |
| rustls 0.23.37 | [RUSTSEC-2026-0285](https://rustsec.org/advisories/RUSTSEC-2026-0285.html) | TLS 1.3 handshake messages incorrectly accepted across encryption level boundaries | 0.23.45 |
| rustls-webpki 0.103.9 | [RUSTSEC-2026-0049](https://rustsec.org/advisories/RUSTSEC-2026-0049.html) | CRLs not considered authoritative by Distribution Point due to faulty matching logic | 0.103.10 |
| rustls-webpki 0.103.9 | [RUSTSEC-2026-0098](https://rustsec.org/advisories/RUSTSEC-2026-0098.html) | Name constraints for URI names were incorrectly accepted | 0.103.12, 0.104.0-alpha.6 |
| rustls-webpki 0.103.9 | [RUSTSEC-2026-0099](https://rustsec.org/advisories/RUSTSEC-2026-0099.html) | Name constraints were accepted for certificates asserting a wildcard name | 0.103.12, 0.104.0-alpha.6 |
| rustls-webpki 0.103.9 | [RUSTSEC-2026-0104](https://rustsec.org/advisories/RUSTSEC-2026-0104.html) | Reachable panic in certificate revocation list parsing | 0.103.13, 0.104.0-alpha.7 |
| ttf-parser 0.25.1 | [RUSTSEC-2026-0192](https://rustsec.org/advisories/RUSTSEC-2026-0192.html) | `ttf-parser` is unmaintained | Unmaintained; evaluate replacement |

## All Go standard-library version matches

These are candidate affected APIs, **not reachability findings**. Platform, enabled protocol, parsing configuration, and input trust determine exposure. Toolchain versions shown are the advisory fixes across release branches; prefer a currently supported patched branch.

| Advisory | Description | Fixed Go versions |
| --- | --- | --- |
| [GO-2025-4006](https://pkg.go.dev/vuln/GO-2025-4006) | Excessive CPU consumption in ParseAddress in net/mail | 1.24.8, 1.25.2 |
| [GO-2025-4007](https://pkg.go.dev/vuln/GO-2025-4007) | Quadratic complexity when checking name constraints in crypto/x509 | 1.24.9, 1.25.3 |
| [GO-2025-4008](https://pkg.go.dev/vuln/GO-2025-4008) | ALPN negotiation error contains attacker controlled information in crypto/tls | 1.24.8, 1.25.2 |
| [GO-2025-4009](https://pkg.go.dev/vuln/GO-2025-4009) | Quadratic complexity when parsing some invalid inputs in encoding/pem | 1.24.8, 1.25.2 |
| [GO-2025-4010](https://pkg.go.dev/vuln/GO-2025-4010) | Insufficient validation of bracketed IPv6 hostnames in net/url | 1.24.8, 1.25.2 |
| [GO-2025-4011](https://pkg.go.dev/vuln/GO-2025-4011) | Parsing DER payload can cause memory exhaustion in encoding/asn1 | 1.24.8, 1.25.2 |
| [GO-2025-4012](https://pkg.go.dev/vuln/GO-2025-4012) | Lack of limit when parsing cookies can cause memory exhaustion in net/http | 1.24.8, 1.25.2 |
| [GO-2025-4013](https://pkg.go.dev/vuln/GO-2025-4013) | Panic when validating certificates with DSA public keys in crypto/x509 | 1.24.8, 1.25.2 |
| [GO-2025-4014](https://pkg.go.dev/vuln/GO-2025-4014) | Unbounded allocation when parsing GNU sparse map in archive/tar | 1.24.8, 1.25.2 |
| [GO-2025-4015](https://pkg.go.dev/vuln/GO-2025-4015) | Excessive CPU consumption in Reader.ReadResponse in net/textproto | 1.24.8, 1.25.2 |
| [GO-2025-4155](https://pkg.go.dev/vuln/GO-2025-4155) | Excessive resource consumption when printing error string for host certificate validation in crypto/x509 | 1.24.11, 1.25.5 |
| [GO-2025-4175](https://pkg.go.dev/vuln/GO-2025-4175) | Improper application of excluded DNS name constraints when verifying wildcard names in crypto/x509 | 1.24.11, 1.25.5 |
| [GO-2026-4337](https://pkg.go.dev/vuln/GO-2026-4337) | Unexpected session resumption in crypto/tls | 1.24.13, 1.25.7, 1.26.0-rc.3 |
| [GO-2026-4340](https://pkg.go.dev/vuln/GO-2026-4340) | Handshake messages may be processed at the incorrect encryption level in crypto/tls | 1.24.12, 1.25.6 |
| [GO-2026-4341](https://pkg.go.dev/vuln/GO-2026-4341) | Memory exhaustion in query parameter parsing in net/url | 1.24.12, 1.25.6 |
| [GO-2026-4342](https://pkg.go.dev/vuln/GO-2026-4342) | Excessive CPU consumption when building archive index in archive/zip | 1.24.12, 1.25.6 |
| [GO-2026-4601](https://pkg.go.dev/vuln/GO-2026-4601) | Incorrect parsing of IPv6 host literals in net/url | 1.25.8, 1.26.1 |
| [GO-2026-4602](https://pkg.go.dev/vuln/GO-2026-4602) | FileInfo can escape from a Root in os | 1.25.8, 1.26.1 |
| [GO-2026-4603](https://pkg.go.dev/vuln/GO-2026-4603) | URLs in meta content attribute actions are not escaped in html/template | 1.25.8, 1.26.1 |
| [GO-2026-4864](https://pkg.go.dev/vuln/GO-2026-4864) | TOCTOU permits root escape on Linux via Root.Chmod in os in internal/syscall/unix | 1.25.9, 1.26.2 |
| [GO-2026-4865](https://pkg.go.dev/vuln/GO-2026-4865) | JsBraceDepth Context Tracking Bugs (XSS) in html/template | 1.25.9, 1.26.2 |
| [GO-2026-4869](https://pkg.go.dev/vuln/GO-2026-4869) | Unbounded allocation for old GNU sparse in archive/tar | 1.25.9, 1.26.2 |
| [GO-2026-4870](https://pkg.go.dev/vuln/GO-2026-4870) | Unauthenticated TLS 1.3 KeyUpdate record can cause persistent connection retention and DoS in crypto/tls | 1.25.9, 1.26.2 |
| [GO-2026-4918](https://pkg.go.dev/vuln/GO-2026-4918) | Infinite loop in HTTP/2 transport when given bad SETTINGS_MAX_FRAME_SIZE in net/http/internal/http2 in golang.org/x/net | 1.25.10, 1.26.3 |
| [GO-2026-4946](https://pkg.go.dev/vuln/GO-2026-4946) | Inefficient policy validation in crypto/x509 | 1.25.9, 1.26.2 |
| [GO-2026-4947](https://pkg.go.dev/vuln/GO-2026-4947) | Unexpected work during chain building in crypto/x509 | 1.25.9, 1.26.2 |
| [GO-2026-4970](https://pkg.go.dev/vuln/GO-2026-4970) | Root escape via symlink plus trailing slash in os | 1.25.12, 1.26.5, 1.27.0-rc.2 |
| [GO-2026-4971](https://pkg.go.dev/vuln/GO-2026-4971) | Panic in Dial and LookupPort when handling NUL byte on Windows in net | 1.25.10, 1.26.3 |
| [GO-2026-4976](https://pkg.go.dev/vuln/GO-2026-4976) | ReverseProxy forwards queries with more than urlmaxqueryparams parameters in net/http/httputil | 1.25.10, 1.26.3 |
| [GO-2026-4977](https://pkg.go.dev/vuln/GO-2026-4977) | Quadratic string concatenation in consumePhrase in net/mail | 1.25.10, 1.26.3 |
| [GO-2026-4980](https://pkg.go.dev/vuln/GO-2026-4980) | Escaper bypass leads to XSS in html/template | 1.25.10, 1.26.3 |
| [GO-2026-4981](https://pkg.go.dev/vuln/GO-2026-4981) | Crash when handling long CNAME response in net | 1.25.10, 1.26.3 |
| [GO-2026-4982](https://pkg.go.dev/vuln/GO-2026-4982) | Bypass of meta content URL escaping causes XSS in html/template | 1.25.10, 1.26.3 |
| [GO-2026-4986](https://pkg.go.dev/vuln/GO-2026-4986) | Quadratic string concatentation in consumeComment in net/mail | 1.25.10, 1.26.3 |
| [GO-2026-5026](https://pkg.go.dev/vuln/GO-2026-5026) | Invoking failure to reject ASCII-only Punycode-encoded labels in golang.org/x/net/idna | 1.25.13, 1.26.6, 1.27.0-rc.3 |
| [GO-2026-5037](https://pkg.go.dev/vuln/GO-2026-5037) | Inefficient candidate hostname parsing in crypto/x509 | 1.25.11, 1.26.4 |
| [GO-2026-5038](https://pkg.go.dev/vuln/GO-2026-5038) | Quadratic complexity in WordDecoder.DecodeHeader in mime | 1.25.11, 1.26.4 |
| [GO-2026-5039](https://pkg.go.dev/vuln/GO-2026-5039) | Arbitrary inputs are included in errors without any escaping in net/textproto | 1.25.11, 1.26.4 |
| [GO-2026-5856](https://pkg.go.dev/vuln/GO-2026-5856) | Invoking Encrypted Client Hello privacy leak in crypto/tls | 1.25.12, 1.26.5, 1.27.0-rc.2 |
| [GO-2026-5972](https://pkg.go.dev/vuln/GO-2026-5972) | Enforce maximum recursion depth in encoding/asn1 | 1.25.13, 1.26.6, 1.27.0-rc.3 |
| [GO-2026-6088](https://pkg.go.dev/vuln/GO-2026-6088) | Add recursion depth guard during decode in encoding/xml | 1.25.13, 1.26.6, 1.27.0-rc.3 |
| [GO-2026-6089](https://pkg.go.dev/vuln/GO-2026-6089) | Apply ReadHeaderTimeout when doing unencrypted HTTP/2 check in net/http | 1.25.13, 1.26.6, 1.27.0-rc.3 |
| [GO-2026-6090](https://pkg.go.dev/vuln/GO-2026-6090) | Limit handshake messages we are willing to accept post-handshake in crypto/tls | 1.25.13, 1.26.6, 1.27.0-rc.3 |
| [GO-2026-6091](https://pkg.go.dev/vuln/GO-2026-6091) | Fix Javascript regexp context tracking in html/template | 1.25.13, 1.26.6, 1.27.0-rc.3 |
| [GO-2026-6218](https://pkg.go.dev/vuln/GO-2026-6218) | Avoid quadratic complexity in resolvePath in net/url | 1.25.13, 1.26.6, 1.27.0-rc.3 |

## Reproduction and completion gaps

1. Enumerate every registry package name/version in `crw/Cargo.lock`; POST `{"queries":[{"package":{"name":"PACKAGE","ecosystem":"crates.io"},"version":"VERSION"}]}` to `https://api.osv.dev/v1/querybatch`. Query Go stdlib version from the Docker builder, not just the go.mod language directive.
2. Fetch full records from `https://api.osv.dev/v1/vulns/ID`, deduplicate aliases, and distinguish informational notices from vulnerabilities.
3. On an equipped isolated builder, run cargo audit and feature-aware cargo tree for the actual production target/features; run govulncheck with the actual pinned Go toolchain. Run the existing component and appliance regression suites after candidate updates.
4. Audit resolved npm/Python environments and immutable runtime SBOMs. No clean bill of health is implied for those currently unscanned artifacts.
5. Preserve baseline and rollout restrictions: stage reviewed candidates and seek equivalence/regression approval before production changes.

A compact machine-readable match snapshot is retained in [security-dependency-matches.json](2026-09-19-security-dependency-matches.json). The temporary query/results/full advisory JSON files remain under `/tmp/fireghost-osv-*` for this session. No credentials or private source were sent; the API query contained public package coordinates only.

Audit source revision: `b6bcd7d29bbe59769f07a844da86eb389a75151b`. Cargo.lock SHA-256: `f0cbd5694f04ad2569efeee55dce72817cbbf0c00a443f5b6c53a0783eadfd9a`.
