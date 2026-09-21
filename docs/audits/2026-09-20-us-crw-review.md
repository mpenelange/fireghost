# Upstream CRW assessment — 2026-09-20

`us/crw` is the foundation already recorded in `upstreams.json`; our imported component comes through `adambenhassen/crw-camofox`. It is an alternative source baseline for the retrieval engine, not a substitute browser binary.

Reviewed main commit: `63762e724b115915316bda2fa4944a3ce185a443`. GitHub's latest release API reports [v0.36.0](https://github.com/us/crw/releases/tag/v0.36.0), published 2026-09-19. Main includes work beyond that release; an eventual trial must choose and pin one revision explicitly. No builds, deployments or runtime tests were performed in this review.

## What makes it relevant

- Its current [Camoufox renderer](https://github.com/us/crw/blob/63762e724b115915316bda2fa4944a3ce185a443/crates/crw-renderer/src/camoufox.rs) explicitly targets **jo-inc/camofox-browser**. It creates a fresh session per scrape, bounds requests with deadlines, polls browser challenges and performs bounded session cleanup on error as well as success. It still requires the external browser server.
- Recent releases add HTTP browser-fingerprint impersonation, improve browser fallback and challenge handling, and make failure reporting more accurate. The 0.36.0 release adds fetch caching and fixes non-HTML Markdown, crawl/robots handling, CDP address retries, cache tenant scoping and local-mode behavior. These are upstream capabilities, not a verified list of changes missing from our locally modified fork.
- The familiar Firecrawl-compatible HTTP surface makes it feasible to trial behind our existing Go router while preserving component boundaries. Endpoint naming alone does not establish Hermes or appliance equivalence.

## Migration differences

1. **Search:** upstream self-hosted search uses SearXNG; its server reports search disabled without an available client. Our deployment uses the fork's Camofox search backend and warm search tabs. Replacing CRW unchanged would lose that backend. Either trial SearXNG with its own service/configuration or retain and port the fork's search implementation.
2. **Browser build/configuration:** upstream uses the optional `camoufox` feature and `[renderer.camoufox]`, whereas our fork uses `camofox`. Upstream's Dockerfile defaults to `cdp,impersonated`, not `camoufox`. Merely swapping a stock image and preserving our config does not enable the Jo integration. The configured upstream Camoufox tier stays outside automatic fallback unless `include_in_auto=true`.
3. **Caching:** upstream fetch caching overlaps with our router's cache. Cache freshness, isolation and metric expectations need explicit regression coverage.
4. **Local work:** preserve source histories, local PDF/dependency changes, deployment labels, runtime hardening and current HTTP behavior. Do not overwrite the imported tree with upstream master as a mechanical update.

## Recommendation

For the immediate search timeout, the smaller experiment is the Jo browser with our current CRW. Upstream CRW is a worthwhile separate migration experiment if the goal is to reduce fork maintenance or adopt its newer rendering behavior. Compare it behind the unchanged router with explicit search infrastructure, the chosen browser features and the actual Hermes provider. The project's advertised benchmarks are not evidence of improvement on our workload.

Sources: [renderer](https://github.com/us/crw/blob/63762e724b115915316bda2fa4944a3ce185a443/crates/crw-renderer/src/camoufox.rs), [configuration](https://github.com/us/crw/blob/63762e724b115915316bda2fa4944a3ce185a443/config.default.toml), [Dockerfile](https://github.com/us/crw/blob/63762e724b115915316bda2fa4944a3ce185a443/Dockerfile), [changelog](https://github.com/us/crw/blob/63762e724b115915316bda2fa4944a3ce185a443/CHANGELOG.md), local `upstreams.json` and `deployment/crw.toml`.
