# Historical fw.2 operations and compatibility

`hermes-production-fw2.lock.json` is the superseded, known-good Hermes retrieval
stack from before the `1.2.0-fw.3` rollout. It is retained only as migration and
rollback evidence. The current deployment authority is `dev/stack.lock.json`.

## Build an identifiable CRW candidate

```bash
CRW_REVISION="$(git rev-parse HEAD)"
CRW_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CRW_VERSION="1.2.0-fw.candidate"
docker compose build \
  --build-arg CRW_VERSION="$CRW_VERSION" \
  --build-arg CRW_REVISION="$CRW_REVISION" \
  --build-arg CRW_BUILD_DATE="$CRW_BUILD_DATE" crw
```

Verify both identity surfaces before testing:

```bash
docker compose run --rm crw crw-server --version
docker compose run --rm crw crw-server version
```

## Live compatibility gate

Bring up an isolated candidate stack; do not replace the production Compose
project. Then run:

```bash
CRW_API_URL=http://127.0.0.1:3000 \
  python3 crw/scripts/live_compatibility.py --phase all \
  --output live-compatibility.json

CRW_API_URL=http://127.0.0.1:3000 \
  python3 crw/scripts/scrape_compatibility.py \
  --output scrape-compatibility.json
```

Every transport timeout, HTTP 5xx, malformed response, and unexpected empty
result is fatal. Google challenge/consent pages and Bing's known empty baseline
are accepted only when the API completes normally. The gate runs cold, warm,
and concurrent phases so tab reuse and worker isolation are exercised.
The scrape gate covers static HTML, a dynamic page, redirects, PDFs, an
anti-bot interstitial, and origin 404 preservation across browser escalation.

After a full failure, isolate the affected search engines without rerunning the
whole matrix:

```bash
python3 crw/scripts/live_compatibility.py \
  --phase cold --engines youtube,reddit,amazon \
  --output live-compatibility-focused.json
```

The known-good Camofox 2.4.6 container was observed at roughly 1.62 GiB and
about 540 PIDs during the Hermes smoke run. Do not add a 512-PID container cap;
if a PID limit is required, establish it from a fresh cold/warm/concurrent run
with explicit headroom (at least 768 for this captured workload).

Do not combine a CRW candidate with a Camofox or Lightpanda candidate. A browser
upgrade gets its own run against the last passing CRW digest.
