# Web Retrieval Engineering Rules

## Purpose
Build a self-hosted Firecrawl-compatible router for Hermes Agent. It proxies `/v2/search` and `/v2/scrape` to local crw-camofox first, caches successful responses, coalesces identical in-flight requests, and optionally falls back to Firecrawl Cloud under hard daily/monthly credit budgets.

## Required workflow
- Strict test-driven development: add one failing behavioral test, run it and record the expected RED failure, add minimum production code, rerun GREEN, then refactor.
- Never add production behavior without first witnessing its test fail for the missing behavior.
- Use Go 1.24+ and the standard library where practical.
- Tests must not contact live internet services. Use `httptest` servers for local CRW and Firecrawl Cloud contracts.
- Run Go commands through Docker because the host does not have Go installed:
  `docker run --rm -v "$PWD:/src" -w /src golang:1.24-bookworm go test ./...`
- Run `gofmt`, `go vet ./...`, `go test -race ./...`, and normal tests before considering a slice complete.
- Never commit secrets. Runtime secrets belong in `.env`, which is ignored. Provide `.env.example` with placeholders only.

## Architecture boundaries
- `cmd/router`: process startup and HTTP server wiring only.
- `internal/router`: Firecrawl-compatible HTTP endpoint behavior.
- `internal/upstream`: tolerant raw-JSON HTTP forwarding; strip inbound Authorization and inject the configured cloud key only for cloud requests.
- `internal/cache`: bounded persistent cache behind an interface; SQLite may be introduced only if justified and tested. An initial filesystem or memory implementation is acceptable if durable behavior is explicit.
- `internal/budget`: persistent daily/monthly credit ledger and hard rejection before cloud calls.
- `internal/singleflight`: request coalescing without unbounded goroutine or map growth.

## Protocol rules
- Implement `POST /v2/search`, `POST /v2/scrape`, `GET /health`, and `GET /metrics` initially.
- Preserve upstream HTTP status, JSON bodies, and unknown fields. Parse only tolerant fields needed to classify local success/failure.
- Search cloud fallback only for transport errors, timeout, non-2xx, `success:false`, or zero web results, whether or not the response includes a warning.
- Scrape cloud fallback only for transport errors, timeout, retryable non-2xx responses (400/422 are terminal unless an explicit retryable indicator or CRW's `unsupported_content_type` error code is present; 401/404/410/robots/invalid URL are terminal), `success:false` with retryable anti-bot/timeout/no-usable-content errors, or empty requested markdown (including a non-deterministic `success:false` whose target did not answer 401/404/410).
- Cloud budget exhaustion must return the truthful local response plus an explicit warning when possible; never hide that fallback was skipped.
- Expose no service except the router on host loopback. Camofox and LightPanda stay on the Compose network.

## Maintainability
- Keep router code independent of CRW private internals; HTTP contracts only.
- Keep developer baselines, release evidence, and third-party images pinned by digest. The public root installer may track the tested stable owned-image `latest` aliases; use matching appliance-version tags when reproducibility or rollback is required.
- Include a Firecrawl SDK conformance test and deterministic integration smoke tests.
