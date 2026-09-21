# Router and deployment security review — 2026-09-19

Reviewed revision: `b6bcd7d29bbe59769f07a844da86eb389a75151b`. Source/configuration review only; no production access or changes. Severity assumes the root Compose installation, which requires a bearer key and a Traefik IP allowlist. Findings requiring an API caller therefore require both access controls to be satisfied. Runtime exploitability has not been demonstrated.

## R1 — High when cloud fallback is enabled: fixed estimates do not enforce actual credit ceilings

`router/internal/router/router.go:367` and the other search fallback branches reserve `SearchEstimatedCredits` (default 2), while scrape branches reserve `ScrapeEstimatedCredits` (default 1). The original request is then forwarded verbatim. Neither request-dependent cost calculation nor reconciliation of actual usage exists in the ledger interface (`router/internal/budget/budget.go:19`).

An authorized caller can request more than ten search results, add search `scrapeOptions`, or request premium scrape formats. When local retrieval fails and cloud fallback runs, the local ledger can undercount the actual charge. This breaks the promised hard monetary/credit bound even though reservation persistence and concurrency locking work. Cloud fallback is disabled by default, which removes this exposure in the default installation.

[Firecrawl's official search documentation](https://www.firecrawl.dev/search) states that search is billed per ten results, with separate per-page charges when scraping results. [Official billing documentation](https://github.com/firecrawl/firecrawl-docs/blob/main/billing.mdx) describes additional option charges. These are current upstream claims, not a measurement of this account's billing.

Remediation: define a supported cloud option policy, reserve a conservative upper bound based on the complete request, and reject requests whose maximum charge cannot be bounded. Reconciliation can release surplus reservations but cannot replace advance upper-bound reservation. Add local mock-upstream tests showing that high-result-count and premium-option requests cannot exceed the configured actual-cost ceiling. Do not verify this by spending production credits.

## R2 — Medium: admission control does not bound retained HTTP requests

`router/internal/router/router.go:86` reads the complete request (up to 2 MiB by default) before admission. `router/internal/singleflight/singleflight.go:55` waits for slots without a bound on waiting callers; duplicate callers also wait without a participant limit. The handler retains its body while waiting. Invalid JSON bypasses the flight group entirely (`router/internal/router/router.go:164` and `:171`, with parsing at `:492`).

The 64-flight limit therefore bounds executing valid unique operations, not total memory, pending handlers, or all upstream requests. Enough concurrent authorized requests can exhaust the router's 256 MiB container allocation. Even 64 responses near the 16 MiB response cap exceed that allocation before accounting for copies; these are capacity estimates, not measured OOM thresholds. Server read/write timeouts do not provide a global admission limit.

Remediation: acquire a bounded handler permit after authentication and before body allocation, reject excess work with 429/503, bound queue time and duplicate waiters, and apply limits to malformed requests too. Choose aggregate memory/concurrency limits together. Validate with local blocking mock upstreams; avoid load testing production.

## R3 — Medium: cache insertion performs a full, exclusive scan

Each `FileCache.Set` takes the cache mutex and calls `scanLocked` (`router/internal/cache/cache.go:172`). That scan reads and JSON-decodes every existing cache body (`:236` onward). Cache reads use the same exclusive mutex (`:121`). Authorized clients generating distinct cacheable queries can grow the cache toward 1 GiB and force repeated whole-cache disk reads and JSON allocation on each insertion, delaying unrelated hits. The byte cap limits storage but does not bound insertion cost or entry count.

Remediation: maintain bounded eviction metadata instead of reading every response on each insert, cap entry count, and perform expiry cleanup incrementally. Benchmark with a populated temporary cache and concurrent hits; no production benchmark was run.

## R4 — Low, dependent on host permissions: backup confidentiality inherits the operator's umask

`scripts/backup.sh:42` redirects the archive directly to the requested path without setting `umask 077` or securing an existing destination. The archive includes cached retrieval responses and browser profiles. Under a permissive umask and a traversable destination directory, other local users can read the archive; profiles may contain session material.

Remediation: use a mode-0600 temporary file in a private destination directory and atomically rename it, accounting for pre-existing paths and symlinks. Restrict archive storage access. No real backups or profile contents were accessed.

## Deployment observations

- Root Compose requires a router key, applies a Traefik CIDR allowlist and TLS, and exposes only the selected retrieval/MCP paths through Traefik. Browser/CRW services have no host port publications. Containers have dropped capabilities, no-new-privileges, and resource limits; most have read-only roots.
- The appliance network is an ordinary bridge with no explicit destination-based egress restriction. Private-network SSRF in CRW therefore lacks a repository-defined network enforcement backstop. Camofox authentication is explicitly disabled; assess together with the CRW report. Live host firewall rules were not inspected.
- Router startup permits an empty API key when MCP is disabled (`router/internal/config/config.go:219`); the root installer prevents this with required interpolation, while standalone/dev use may be unauthenticated. Treat this as a deployment-dependent hardening opportunity, not an auth bypass of root Compose.
- Release jobs use mutable action refs (`actions/checkout@v4`, `actions/cache@v4`, `docker/setup-buildx-action@v3`, `dtolnay/rust-toolchain@stable`) in a pipeline that has registry credentials. Pin reviewed full commit SHAs and automate review of updates. This is supply-chain exposure, not evidence of a compromised action.
- Owned images using `latest` in the root installer are an explicit documented policy exception. Use paired immutable release references for audited deployments; this review does not classify the permitted aliases themselves as a defect.
- Tracked environment examples contain placeholders. A redacted scan of 497 tracked files found no matches for private-key headers, GitHub token formats, or AWS access-key IDs. This narrow pattern scan does not cover arbitrary API keys, ignored runtime files, or Git history; no claim of secret-free history is made.

## Validation limits

The host has no Docker or Go executable, so Go tests, vet/race checks, Compose runtime inspection, image scans, and local Go exploit harnesses could not run. No production endpoint was probed. Findings above are source-supported with explicit preconditions; performance and exploit thresholds need isolated runtime confirmation. The consolidated report records appliance/Python test results.
