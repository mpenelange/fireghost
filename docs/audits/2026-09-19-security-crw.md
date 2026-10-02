# CRW application security review — 2026-09-19

Scope: source review of CRW HTTP/MCP routing, URL validation, HTTP and CDP renderers, LLM dispatch, and PDF parsing controls. No source behavior was changed and no production requests were made. Findings below are confirmed control-flow defects from source; runtime exploitation was not performed. Rust/Cargo and an existing CRW binary were unavailable in the audit environment. Dependency advisories and appliance/router deployment posture are covered separately.

Severity assumes an untrusted caller can reach the affected API (with an API key if configured), or persuade a legitimate user to scrape attacker-controlled content. It does not imply the affected CRW port is publicly exposed in the deployed system.

## Findings

### CRW-01 — High: caller-controlled LLM endpoint bypasses SSRF protection

Evidence:

- `crw/crates/crw-core/src/types.rs:214` accepts `llmApiKey`, provider and `baseUrl` in scrape requests.
- `crw/crates/crw-crawl/src/single.rs:933` builds an LLM configuration from any supplied key and copies the caller's base URL; `:461` selects it for JSON/summary operations.
- `crw/crates/crw-extract/src/llm.rs:38` creates an ordinary client with no destination/redirect policy; `:318` uses an Anthropic base URL verbatim and `:331` POSTs to it. OpenAI-compatible dispatch similarly accepts a caller-selected endpoint (`:386`).
- `crw/crates/crw-server/src/routes/scrape.rs:19` validates only the scraped page URL, not the LLM endpoint. V2 maps the same fields at `routes/v2/scrape.rs:149`.

An authorized caller can supply a public page, `formats:["summary"]`, `llmApiKey:"arbitrary-nonempty-value"`, `llmProvider:"anthropic"`, and an internal `baseUrl`. A real provider key is unnecessary: CRW submits the POST before any provider validates it. This reaches loopback/private services with an attacker-influenced JSON body. Anthropic allows an exact path; the OpenAI path generally appends `/chat/completions`. Arbitrary internal response disclosure is constrained by provider response parsing, but the internal request occurs even when parsing fails. Network policy and reachability bound impact. Custom redirects also have no SSRF policy.

Remediation: distinguish operator-configured trusted private LLM endpoints from untrusted per-request overrides. Validate and constrain every caller override, including redirect hops, through a transport that pins validated destinations; prefer configured provider IDs or an explicit allowlist.

Safe validation to add: a loopback mock LLM listener, public-page fetch fixture, and API request with the above fields must result in rejection without any request reaching the mock. Do not use actual internal production services.

### CRW-02 — High: browser redirects and subrequests escape URL safety

Evidence:

- Initial URLs are checked at `crw/crates/crw-server/src/routes/scrape.rs:19`.
- `crw/crates/crw-renderer/src/cdp.rs:819` processes intercepted requests, but at `:859` only calls the advertisement/resource blocklist and at `:882` continues everything else.
- `crw/crates/crw-renderer/src/blocklist.rs:103` checks resource types and host substrings, not address safety. `Document`, `Fetch`, and `XHR` to private addresses are not categorically denied.
- CDP navigates using `Page.navigate` at `crw/crates/crw-renderer/src/cdp.rs:2083`. Interception is optional (`:2050`); there is no subsequent shared URL validator in this module.

A page whose initial hostname resolves publicly can navigate/redirect to an internal address after the initial check, or initiate internal subrequests. A top-level navigation is particularly relevant because it avoids depending on CORS-readable cross-origin fetches. Native browser private-network protections and actual renderer configuration may limit some request forms; they are not an application-enforced boundary here. Extraction after navigation can return internal document contents. A user need only scrape malicious content with a CDP tier enabled; a separate malicious API account is unnecessary.

Remediation: enforce destination safety for all browser traffic, ideally at an isolated network/egress proxy boundary that cannot be bypassed by browser DNS, redirects, WebSockets, or alternate schemes. Request interception should fail closed and include all redirect/subresource destinations. Test each supported renderer separately; Camofox's remote implementation was not audited here.

Safe validation to add: scrape a public fixture that navigates to an isolated private test listener and verify that neither a request nor extracted private content occurs. Test HTTP redirects, script navigation, iframes, and fetch requests independently.

### CRW-03 — High: upstream response cap is enforced after unbounded buffering

Evidence: `crw/crates/crw-renderer/src/http_only.rs:646` checks a supplied Content-Length against the 50 MiB cap, then `:681` calls `resp.bytes().await`; the actual byte count is checked only at `:686` after the complete body has been allocated.

An attacker-controlled origin can omit Content-Length (chunked transfer) and send far more than 50 MiB within the request time budget. Compressed responses can also make a wire-size check insufficient. The eventual error does not prevent peak allocation or OOM. One fast response or several concurrent scrapes can exhaust a worker/container even though the inbound API body is small. The limit is a result acceptance check, not a memory bound.

Remediation: consume decoded body chunks into a bounded buffer and abort before accumulating more than the cap; enforce limits consistently in sitemap, LLM, and remote-renderer response readers as well. Preserve concurrency and request-time limits as additional controls.

Safe validation to add: serve a modest stream exceeding a deliberately lowered test cap without Content-Length and assert the reader terminates near the cap; avoid an actual OOM test.

### CRW-04 — Medium: breaker reset administrative action is unauthenticated

Evidence: `crw/crates/crw-server/src/app.rs:42` applies API-key authentication only to `api_routes`, while `:83` registers `POST /admin/breakers/reset` on the outer router. `crw/crates/crw-server/src/routes/breakers.rs:19` resets every global/per-host breaker without another authorization check.

Anyone who can reach the CRW listener can reset circuit breakers even with `[auth].api_keys` configured. Repeated resets can defeat cooldown/load shedding during a renderer incident. Associated breaker/metrics endpoints are public too, though their intended disclosure policy is not explicit. This is conditional on CRW listener exposure; a front router may not forward this route.

Remediation: place the reset endpoint behind explicit operator authentication/authorization, preferably on a management-only listener. Do not implicitly authorize administration merely because scrape API access is allowed.

Safe validation to add: create an app with an API key, POST to the reset route without credentials, and require 401/403. Test the authorized management path separately.

### CRW-05 — Medium: BYOK guard is inconsistent across equivalent APIs

Evidence:

- `crw/crates/crw-core/src/config.rs:1279` documents `require_byok_header` as restricting access to LLM features.
- `crw/crates/crw-server/src/routes/scrape.rs:38` rejects summary/JSON without a per-request key when that option is set.
- `crw/crates/crw-server/src/routes/v2/scrape.rs:176` only checks whether any LLM configuration exists; it then passes the server configuration directly to scraping at `:200`.
- `crw/crates/crw-server/src/routes/mcp.rs:34` similarly supplies the server LLM config without this guard. `routes/v2/parse.rs:173` invokes LLM formats using the server configuration.
- A repository search for `require_byok_header` finds no shared middleware enforcing the documented header condition.

On a deployment that configures both a server LLM credential and this guard, a caller rejected by the v1 scrape handler can request equivalent LLM work through v2 or MCP and consume the server's provider allowance. Normal API authentication still applies. The v1 check also never actually inspects the named header, so the documented trusted-header exception is not implemented. This finding is about a configured policy bypass, not a claim that server-funded LLM features are always forbidden.

Remediation: centralize the LLM authorization rule before all entry points, including parse, extract, batch, crawl, and MCP, and make the documented header semantics explicit. Tests should use a mock provider and assert denied requests never dispatch.

### CRW-06 — Medium: DNS validation is detached from connection establishment

Evidence: `crw/crates/crw-core/src/url_safety.rs:105` resolves and checks addresses, then discards them. Redirect validation uses a separate blocking resolver (`:112`). `crw/crates/crw-renderer/src/http_only.rs:186` creates a standard reqwest client and `:318` calls `get(url)` without a pinned validated resolution.

An attacker controlling DNS can return a public address during validation and a private address when the HTTP client resolves the same hostname for the connection. Proxy-side resolution introduces another separate resolver. The source therefore does not guarantee the peer actually contacted is the peer validated. This is a TOCTOU SSRF gap; successful exploitation depends on resolver caching, timing, and network reachability and was not demonstrated in this audit. CRW-01 is a more deterministic SSRF path and should be prioritized first.

Remediation: make resolution, IP validation and connection establishment one operation (including redirects), or enforce private-address denial in the actual egress transport/proxy. Preserve hostname/SNI checks when pinning addresses.

## Additional observations and limits

- Native Chrome is launched with `--no-sandbox` in `crw/crates/crw-renderer/src/browser.rs:445`. This weakens containment if a browser vulnerability is exploited; it is not itself proof of code execution. Prefer sandboxed browsers in isolated, nonprivileged containers with no secrets or host mounts.
- `CRW_HTTP_TLS_RELAXED_FALLBACK` deliberately disables certificate/hostname verification on a retry, but is opt-in and defaults false (`http_only.rs:125`). Review deployment settings if content integrity or request headers are sensitive; no default-TLS defect is asserted.
- Positive controls: HTTP/MCP APIs share API-key middleware when configured; literal private/loopback addresses and ordinary HTTP redirect targets are checked; inbound JSON bodies are bounded; PDF parsing has decompression and concurrency controls and a subprocess option. These controls do not cover the gaps above.
- No credential contents were read or printed. No claim of a complete secret-history audit, deployed penetration test, PDF parser exploit review, browser-engine audit, or formal verification is made.
- Existing security tests cover literal SSRF targets and basic error handling, but the identified cross-surface authorization, response-stream, browser navigation, custom LLM endpoint and DNS-pinning scenarios need regression coverage.
