# Jo Camofox alternative — source and registry review

Reviewed 2026-09-20. Recommendation: a worthwhile isolated replacement candidate, not yet a proven fix or approved production image. No dependencies installed, upstream scripts executed, containers started, or deployment pins changed during this review.

## Identity and distribution

- Repository: https://github.com/jo-inc/camofox-browser
- Latest published release: [v1.16.0](https://github.com/jo-inc/camofox-browser/releases/tag/v1.16.0), published 2026-09-14.
- Reviewed source: `79d425be26743883a06613eaa3be5e38e7ab5409`, current master and release commit at review time.
- Verified public OCI index for `ghcr.io/jo-inc/camofox-browser:1.16.0`: `sha256:3cc29763d3c784ae71a7d56853a12c43e100ff2f13da1e2eb88322b08e12d6a0`.
- Linux amd64 manifest: `sha256:4a5cdb3fe36a320220971f9ec1bd6138d29d483bd2f97a24b91b9d6e859157cb`; arm64 manifest: `sha256:a2be3d619c1dab0981e933fe565a71169065c1f6c61e280e352b001ed59f00e9`.
- Jo's 1.x and redf0x1's 2.x are separate release lines; comparing their version numbers does not establish which implementation is newer. GitHub's API marks both repositories as non-forks, so GitHub parent metadata alone does not establish their ancestry.

## Why it merits a trial

The [evaluation handler](https://github.com/jo-inc/camofox-browser/blob/79d425be26743883a06613eaa3be5e38e7ab5409/server.js#L5404) accepts the `userId`/`expression` request and `{ok,result}` response used by CRW. Tab creation, listing, wait and deletion endpoints also exist with the expected core fields. This is source-level compatibility, not an appliance pass.

Evaluation uses per-user admission and a per-tab mutex, an operation timeout, and destruction of a timed-out tab. There is no `finishTrackedAction` step in this source, unlike the path implicated in the redf0x1 2.4.7 investigation. That is relevant evidence for testing an alternative, but neither proves the previous diagnosis nor guarantees this implementation cannot hang. Admission, lock waiting, operation execution and awaited cleanup have separate budgets; the default 30-second operation timeout is not a total HTTP deadline.

The release Dockerfile selects Node 22 on Debian Trixie, Camoufox 152.0.4 beta.28 and camoufox-js 0.11.5. The release workflow publishes amd64 and arm64 images. CI defines both ordinary unit/plugin checks and browser-dependent security, timeout continuation, operational failure and tab-lifecycle checks. The browser tests excluded from its fast unit job are run in its browser job. This review did not execute or verify a particular CI run.

## Integration and security caveats

- **HTTP 410 recovery:** timed-out tab destruction returns `tab_timeout` with HTTP 410. CRW's `is_stale_tab` currently recognizes 404, 5xx and transport errors, but not 410. Its extraction polling can therefore lose time retrying a destroyed tab until a later 404. A focused recovery test and explicit handling of the documented 410 response should accompany integration. Navigation-related 409 responses also deserve coverage.
- **Telemetry defaults on:** the source configuration enables crash/hang reporting unless `CAMOFOX_CRASH_REPORT_ENABLED=false`. Disable it in an isolated trial so testing does not submit reports to an external service.
- **Container compatibility still needs testing:** the CI image defaults to root and stores bundled browser files under `/root/.cache/camoufox`. Preserve private networking, resource limits and writable temporary/profile mounts; verify compatibility with the appliance's read-only filesystem setup.
- **Security is not established by newer versions:** the image has not been scanned in this review. Its Dockerfile uses a mutable base tag and does not checksum the Camoufox ZIP download. Pin the final OCI digest and scan that exact image before promotion.

## Proposed acceptance sequence

Use the retained isolated appliance baseline, change only Camofox to the verified Jo digest, and disable telemetry. First repeat the exact Wikipedia navigation/evaluation diagnostic and live search contract, then the actual Hermes provider search/extraction gate. Check warm-tab reuse, forced 410 recovery and parallel requests. Finally run renderer-provenance browser comparisons and an image vulnerability scan. Existing v2 provenance and external-content fixture failures must be distinguished from candidate regressions; do not relax them to manufacture a pass.

Relevant source: [timeout and mutex implementation](https://github.com/jo-inc/camofox-browser/blob/79d425be26743883a06613eaa3be5e38e7ab5409/server.js#L512), [Dockerfile](https://github.com/jo-inc/camofox-browser/blob/79d425be26743883a06613eaa3be5e38e7ab5409/Dockerfile.ci), [CI](https://github.com/jo-inc/camofox-browser/blob/79d425be26743883a06613eaa3be5e38e7ab5409/.github/workflows/ci.yml), [configuration](https://github.com/jo-inc/camofox-browser/blob/79d425be26743883a06613eaa3be5e38e7ab5409/lib/config.js).
