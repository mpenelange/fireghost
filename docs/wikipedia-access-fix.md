# Wikipedia local extraction reliability

## Outcome

Wikipedia extraction works through the isolated no-cloud Hermes router after fixing the router's response-body context lifetime. Fresh end-to-end requests returned complete marker-checked markdown for Chicago (271,899 characters), Alan Turing (129,182), and Katherine Johnson (39,288). Router metrics recorded zero cloud attempts.

No production or existing staging service was changed, restarted, or probed through a path capable of cloud spend. Browser images were not upgraded.

## Diagnosis and supplemental-comparison correction

The supplemental Compose request was materially equivalent to deployment for this failure: it used `POST /v2/scrape`, the same local `http://crw:3000` route, the same CRW configuration, production browser pins, no cloud key, a non-routable cloud URL, and zero budgets. It omitted several explicit production size settings, but the router defaults are the same 16 MiB response/cache-entry limits; the Chicago CRW response is only about 278 KiB. The omission did not cause the 502.

The supplemental reporting harness did have an observability defect: `semantic()` removed the raw `body` and retained only derived fields. All three 43-byte router errors were therefore reduced to `http_error`, hiding the useful body `{"error":"local upstream response failed"}`. The old/new/old conclusion that both router versions exhibited the symptom remains valid, but it did not establish a CRW/Wikipedia retrieval failure.

Direct CRW and Go-client probes returned complete Wikipedia bodies with correct `Content-Length`. The router alone returned 502 for larger or streamed bodies. `postWithAttemptTimeout` created an attempt context, called `http.Client.Do`, and deferred cancellation inside that helper. `Do` returns after response headers arrive, so the helper canceled the request before `readEntry` consumed the body. Whether a request worked depended on how much of the body was already buffered, explaining the inconsistent size correlation and the same Hacker News failure.

## Source provenance

The successful responses are local CRW responses, not cloud responses:

- The isolated router had a blank cloud key, `http://127.0.0.1:9` as cloud URL, and daily, burst, refill, and monthly budgets all set to zero.
- Router metrics stayed at `web_retrieval_cloud_attempts_total{endpoint="scrape"} 0` while local attempts increased.
- CRW's v2 adapter synthesizes `metadata.proxyUsed`: `basic` means the resolved local proxy tier when stealth was not requested. It is not evidence of paid-cloud proxying.
- CRW similarly emits `creditsUsed` as compatibility/accounting metadata, defaulting local engine cost to one. It is not evidence of Firecrawl Cloud billing.
- `cacheState: "miss"` is also synthesized by CRW because CRW has no read-through cache. Router cache hits bypass CRW and preserve the original document metadata.

## Strict TDD record

Router RED:

```text
TestScrapeKeepsAttemptContextAliveWhileReadingStreamingResponse
response = (502, "{\"error\":\"local upstream response failed\"}\n"), want 200
FAIL
```

Router GREEN after wrapping the response body so closing it cancels the attempt context:

```text
TestScrapeKeepsAttemptContextAliveWhileReadingStreamingResponse ... PASS
```

The deadline still covers the entire body read. A request error cancels immediately, and every successful response path closes the body.

The invalid-article control exposed a second issue: a genuine target 404 with a long rendered error page was reported as `success:true`. The focused CRW test `rich_not_found_page_is_still_a_truthful_http_error` failed, then passed after making terminal 401/404/410 statuses errors regardless of body length. Other rich error statuses retain the existing SPA-recovery behavior.

## Exact changes

- `router/internal/router/router.go`: keep each attempt context alive through response-body consumption with a cancel-on-close body wrapper.
- `router/internal/router/router_test.go`: add a deterministic delayed streaming-response regression test.
- `crw/crates/crw-server/src/routes/v2/scrape.rs`: classify terminal 401/404/410 target statuses truthfully even when the rendered error page is long, with a focused unit test.
- `artifacts/wiki-fix/`: bounded semantic probe summary and full public failure responses.

No Wikipedia special case was added. SSRF and robots behavior was not weakened. The NASA PDF control still fails truthfully because the resolved address is blocked by the existing URL-safety policy; that is independent of the response-body bug.

## Verification

- Router: `go vet ./...`, `go test ./...`, and `go test -race ./...` all pass in `golang:1.24-bookworm`.
- CRW: `cargo fmt --all -- --check` and all 47 `crw-server` library tests pass.
- Isolated appliance: Chicago, Alan Turing, Katherine Johnson, and the earlier HN control return full local content after the router fix; the invalid Wikipedia title returns `success:false`, `error: "Target returned 404 Not Found"`, and `metadata.statusCode: 404` after the CRW fix.
- Evidence: [probe summary](../artifacts/wiki-fix/probe-summary.json), [invalid-title full response](../artifacts/wiki-fix/invalid-wikipedia-response.json), and [PDF full response](../artifacts/wiki-fix/public-pdf-response.json).

## Remaining deployment steps

Parent review is required before deployment. After approval: build and publish versioned router and CRW images with the reviewed monorepo revision/component versions, update immutable deployment pins, run the appliance contract/regression gate in a new disposable environment, then follow the normal staged rollout. Live production and existing-staging Hermes integration remains intentionally unverified in this task.
