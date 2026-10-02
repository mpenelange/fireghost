# Router dependency upgrade validation — 2026-09-19

The candidate upgrades the Go builder/test image from 1.24.6 to 1.27.1 (Bookworm) and the router/backup/restore Alpine image from 3.22.1 to 3.22.6. Both use verified immutable multi-platform index digests. [Registry evidence](2026-09-19-candidate-image-pins.json) also records the Rust and Debian candidate digests supplied to the CRW workstream.

Changed files: `router/Dockerfile`, root `Makefile`, `scripts/backup.sh`, `scripts/restore.sh`, and the matching Alpine constant in `tests/appliance/backup_restore_test.py`. The router CI workflow already calls `make check-router`, so it inherits the new image without a workflow edit. `router/go.mod` retains its existing language compatibility floor. No Go source behavior, historical baselines, production containers, or production credentials were changed.

## Executed checks

Docker was absent. The installed Apple `container` 1.4.1 service was already running; checks ran in isolated Linux containers using the exact candidate Go image, with no published ports. This follows the instruction's isolated Go-toolchain intent but does not claim validation under the Docker engine itself.

| Check | Result |
| --- | --- |
| Pinned Go image `go version` | `go version go1.27.1 linux/arm64` |
| Existing `make check-router` with `GO_DOCKER` override | PASS |
| gofmt check | PASS; no Go source formatting changes |
| `go vet ./...` | PASS |
| `go test ./...` | PASS, seven packages; upstream package has no tests |
| `go test -race ./...` | PASS, seven packages |
| Official `govulncheck` source scan | PASS: `No vulnerabilities found.` |
| Build unchanged router Dockerfile structure with new image pins | PASS, Linux arm64 |
| Candidate runtime UID | PASS: 10001 |
| Candidate internal `/health` HTTP request | PASS: `{"status":"ok"}` |
| `sh -n scripts/backup.sh scripts/restore.sh` | PASS |
| `git diff --check` | PASS |

Reproduce component checks:

```sh
make check-router GO_DOCKER='container run --rm -m 2G -v /Users/michael/development/fireghost/router:/src -w /src docker.io/library/golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195'
```

The scanner command was `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` inside the same image; it resolved to **golang.org/x/vuln v1.8.0**. For repeatability use `@v1.8.0`. The original 45 Go 1.24.6 version-level audit matches and the candidate clean source scan use different analysis depth; they should not be presented as 45 confirmed reachable vulnerabilities repaired.

The local-only smoke image is `fireghost-router:audit-20260919`; its generated OCI index digest is `sha256:87363dbc7afac7b6219a43242ff866764c00240acc2e38e135f10e119abb58cf`. This is a validation artifact built with default development version labels, not a release artifact or production deployment.

## Remaining limits

The backup/restore integration test requires Docker Compose and named-volume semantics; it was not emulated with an incomplete shim. It still requires the existing Docker CI environment. Only Linux arm64 was built/run locally; Linux amd64 remains a release/CI check. OS-package CVE scanning and full appliance/browser/live upstream regression gates are separate from the successful Go scanner and health check. Production rollout still requires equivalence and regression approval.
