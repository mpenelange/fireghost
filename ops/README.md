# Operations and compatibility

`hermes-production.lock.json` is the captured, known-good Hermes retrieval
stack. Image digests are authoritative; tags and reported semantic versions are
descriptive only. Update one component at a time and retain the previous lock
file until the candidate passes the live compatibility gate.

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
  python3 scripts/live_compatibility.py --phase all \
  --output live-compatibility.json
```

Every transport timeout, HTTP 5xx, malformed response, and unexpected empty
result is fatal. Google challenge/consent pages and Bing's known empty baseline
are accepted only when the API completes normally. The gate runs cold, warm,
and concurrent phases so tab reuse and worker isolation are exercised.

Do not combine a CRW candidate with a Camofox or Lightpanda candidate. A browser
upgrade gets its own run against the last passing CRW digest.
