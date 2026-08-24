# Architecture

Identical cacheable requests are coalesced by key. Unique in-flight work is bounded by `ROUTER_MAX_INFLIGHT` (default `64`); capacity waits honor caller cancellation, while work already started continues independently until completion and releases its slot only then.

The appliance publishes one loopback endpoint: the Go router at `127.0.0.1:33000`. The router accepts the Firecrawl-compatible `/v2/search` and `/v2/scrape` APIs, plus `/health` and `/metrics`.

Requests go to CRW first. CRW uses direct HTTP, then LightPanda at `ws://lightpanda:9222/`, then Camofox at `http://camofox:9377`. Those three services have no host ports. The single `appliance` bridge permits outbound retrieval while service names provide internal discovery.

Successful router responses and the cloud-credit ledger live in `router-data`. Browser profiles live in `camofox-profiles`. Router, CRW, and LightPanda roots are read-only with writable scratch space in tmpfs. Camofox is the deliberate exception: Camoufox creates a Firefox `glxtest` helper and font caches beneath `/home/node/.cache` at runtime, so that container keeps an ephemeral writable root while still dropping all capabilities, using `no-new-privileges`, and exposing no host port.

The router is independent of CRW internals and communicates only through HTTP. Replacing CRW requires preserving that contract.

## Component and release boundary

The monorepo owns the deployable appliance: the Go router, Compose topology, runtime policy, and end-to-end contract tests. The deployment consumes CRW as a container image rather than reaching into its private implementation.

The `crw/` component owns the Rust application and publishes versioned images. A component release crosses into `deploy/` only when its registry digest is recorded in `CRW_IMAGE` and the complete appliance passes validation. The immutable digest is the deployment authority; the human-readable image tag and Git tag identify the source release but are never used as mutable runtime pins.

The handoff is therefore one-way:

1. `crw/` tests and publishes a versioned image.
2. The registry returns the image's immutable digest.
3. `deploy/` pins that digest and validates the assembled appliance.
4. Production adopts the validated digest while retaining the previous digest for rollback.
