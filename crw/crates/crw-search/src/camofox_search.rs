//! Camofox-backed web-search client.
//!
//! Search SERPs trip anti-bot / consent walls immediately, so search does NOT
//! use the renderer failover ladder — it drives the camofox-browser (Firefox)
//! tier directly: navigate a tab, observe the destination through `/tabs`, then
//! scrape result rows via `/evaluate`. The entire attempt is bounded by CRW's
//! deadline. A timed-out tab is replaced before best-effort closure, since the
//! upstream navigation may continue after the client disconnects. Multiple
//! engines requested in one call run sequentially on the warm tab and their
//! rows are merged (see [`merge_results`]).
//!
//! Concurrency: camofox-browser keys one persistent context per `userId` and
//! eagerly tears that context down when its tab count hits zero, leaving a
//! ~9s relaunch window in which `newPage` throws `window is null`. Creating
//! and deleting a tab per query raced that teardown, so concurrent or
//! rapid-sequential searches failed with empty / 5xx results. We avoid the
//! race with a bounded pool of workers. Each worker serializes access to its
//! own long-lived warm tab and persistent session identity, so the context
//! never sees concurrent use nor drops to zero tabs. The default pool size is
//! one, preserving the original behavior; larger explicitly configured pools
//! allow independent queries to overlap. If a worker's warm tab goes stale
//! (idle eviction / camofox restart), that worker recreates it and retries once.
//!
//! Rows are mapped into the existing [`SearxngResponse`] shape so the entire
//! downstream transform / rerank pipeline (`transform.rs`, `rerank.rs`) is
//! reused unchanged — this client is a drop-in alternative upstream source, not
//! a new result format.

use std::time::{Duration, Instant};

use serde::Deserialize;
use serde_json::json;

use crw_core::types::SearchEngine;

use crate::client::{
    MAX_ERROR_BODY_BYTES, SearchError, SearxngResponse, SearxngResult, read_capped,
};
use crate::params::SearxngParams;

/// Stable `userId` for the search client's camofox sessions (separate from the
/// renderer tier's so search and scrape don't share a profile).
const USER_ID: &str = "crw-search";

/// Browser-context key used by the default one-worker constructor. `/tabs`
/// requires both `userId` and `sessionKey`; larger pools suffix this key with
/// the worker index so every worker owns an independent persistent identity.
const SESSION_KEY: &str = "search";

/// Pause before recreating a stale tab, giving any in-flight upstream context
/// relaunch a moment to settle before we retry. Only hit on the rare
/// stale-tab path (idle eviction / camofox restart), not the steady state.
const RETRY_BACKOFF: Duration = Duration::from_millis(750);

/// Replacement creation and old-tab deletion have independent short budgets;
/// a wedged browser must not add another full search timeout to cleanup.
const CLEANUP_BUDGET: Duration = Duration::from_millis(250);

/// Polling is intentionally cheap and bounded. Camofox's tab-list route does
/// not wait for browser lifecycle events, so it remains responsive while a
/// destination is loading or redirecting through a challenge page.
const URL_POLL_INTERVAL: Duration = Duration::from_millis(200);
const RENDER_SETTLE: Duration = Duration::from_millis(500);

/// Budget for resolving one page's Google redirect links, all in parallel.
/// A link that is not resolved in time keeps its (working) redirect URL.
const REDIRECT_RESOLVE_BUDGET: Duration = Duration::from_secs(3);

/// JS evaluated in the Google SERP to extract result rows. Returns a JSON
/// *string* (via `JSON.stringify`) so the camofox `/evaluate` `result` field
/// comes back as a string we can parse. Selectors are intentionally broad and
/// kept in this one place — Google rewrites its SERP DOM periodically, so this
/// is the single spot to fix when extraction drifts.
const GOOGLE_SCRAPE_JS: &str = r#"JSON.stringify(Array.from(document.querySelectorAll('div.g, div.MjjYud')).map(function(el){var a=el.querySelector('a[href]');var h=el.querySelector('h3');var s=el.querySelector('.VwiC3b, [data-sncf], .st');return (a&&h)?{url:a.href,title:h.innerText,content:s?s.innerText:''}:null;}).filter(Boolean))"#;

/// Bing SERP extractor. `li.b_algo` rows; `h2 a` for title/url, `.b_caption p`
/// for the snippet. Bing wraps result links in a `bing.com/ck/a?…&u=a1<base64>`
/// click-tracker — the inline `unwrap` decodes that `u` param back to the real
/// destination (and leaves already-direct links untouched).
const BING_SCRAPE_JS: &str = r#"JSON.stringify((function(){function unwrap(u){try{var m=u.match(/[?&]u=a1([^&]+)/);if(m){var b=m[1].replace(/-/g,'+').replace(/_/g,'/');while(b.length%4)b+='=';return decodeURIComponent(escape(atob(b)));}}catch(e){}return u;}return Array.from(document.querySelectorAll('li.b_algo')).map(function(el){var a=el.querySelector('h2 a[href]');var s=el.querySelector('.b_caption p, p');return a?{url:unwrap(a.href),title:a.innerText,content:s?s.innerText:''}:null;}).filter(Boolean);})())"#;

/// DuckDuckGo SERP extractor (the `duckduckgo.com/?q=` layout). Result blocks
/// are `article[data-testid="result"]` with `h2 a` and a snippet node; the
/// `div.result` / `a.result__a` fallbacks cover the lite/html layout.
const DDG_SCRAPE_JS: &str = r#"JSON.stringify(Array.from(document.querySelectorAll('article[data-testid="result"], div.result')).map(function(el){var a=el.querySelector('h2 a[href], a.result__a[href]');var s=el.querySelector('[data-result="snippet"], .result__snippet');return a?{url:a.href,title:a.innerText,content:s?s.innerText:''}:null;}).filter(Boolean))"#;

/// Wikipedia full-text search extractor. The `Special:Search` SERP lists hits
/// as `.mw-search-result-heading a` (absolute article hrefs, no inline snippet).
const WIKIPEDIA_SCRAPE_JS: &str = r#"JSON.stringify(Array.from(document.querySelectorAll('.mw-search-result-heading a')).map(function(a){var t=(a.innerText||'').trim();return (a.href&&t)?{url:a.href,title:t,content:''}:null;}).filter(Boolean))"#;

/// YouTube search extractor. Each video result is a `ytd-video-renderer` whose
/// `a#video-title` carries the watch URL and the full title in its `title`
/// attribute (the inner text is lazy/empty until hover).
const YOUTUBE_SCRAPE_JS: &str = r#"JSON.stringify(Array.from(document.querySelectorAll('ytd-video-renderer a#video-title')).map(function(a){var t=(a.getAttribute('title')||a.innerText||'').trim();return (a.href&&t)?{url:a.href,title:t,content:''}:null;}).filter(Boolean))"#;

/// Reddit search extractor. Post links are `a[href*="/comments/"]`; Reddit
/// renders several anchors per post (thumbnail + title), so we dedupe by the
/// query-stripped permalink and keep the first non-trivial link text.
const REDDIT_SCRAPE_JS: &str = r#"JSON.stringify((function(){var seen={};var out=[];document.querySelectorAll('a[href*="/comments/"]').forEach(function(a){var u=a.href.split('?')[0];var t=(a.innerText||'').trim();if(t.length>5&&!seen[u]){seen[u]=1;out.push({url:u,title:t,content:''});}});return out;})())"#;

/// Amazon product-search extractor. Each `[data-component-type="s-search-result"]`
/// card holds the product link (`a[href*="/dp/"]`) and title (`h2 span`/`h2`);
/// dedupe by the query-stripped `/dp/` URL.
const AMAZON_SCRAPE_JS: &str = r#"JSON.stringify((function(){var seen={};var out=[];document.querySelectorAll('[data-component-type="s-search-result"]').forEach(function(el){var a=el.querySelector('a[href*="/dp/"]');var h=el.querySelector('h2 span, h2');if(a&&h){var u=a.href.split('?')[0];var t=(h.innerText||'').trim();if(t&&!seen[u]){seen[u]=1;out.push({url:u,title:t,content:''});}}});return out;})())"#;

/// The extractor JS for a browser-driven engine. Each has dedicated selectors
/// tuned against its live SERP — this is the single place to fix when a DOM
/// drifts. GitHub is *not* browser-driven (it uses the REST Search API), so it
/// never reaches here.
fn scrape_js(engine: SearchEngine) -> &'static str {
    match engine {
        SearchEngine::Google => GOOGLE_SCRAPE_JS,
        SearchEngine::Bing => BING_SCRAPE_JS,
        SearchEngine::DuckDuckGo => DDG_SCRAPE_JS,
        SearchEngine::Wikipedia => WIKIPEDIA_SCRAPE_JS,
        SearchEngine::Youtube => YOUTUBE_SCRAPE_JS,
        SearchEngine::Reddit => REDDIT_SCRAPE_JS,
        SearchEngine::Amazon => AMAZON_SCRAPE_JS,
        SearchEngine::Github => unreachable!("github uses the REST Search API, not the browser"),
    }
}

/// Direct SERP URL for a browser-driven engine. We intentionally do not use the
/// Google macro so every engine uses an explicit query-specific destination,
/// which is verified via `/tabs` before extraction.
fn search_target_url(engine: SearchEngine, query: &str) -> String {
    let q: String = url::form_urlencoded::byte_serialize(query.as_bytes()).collect();
    match engine {
        SearchEngine::Google => format!("https://www.google.com/search?q={q}"),
        SearchEngine::Bing => format!("https://www.bing.com/search?q={q}"),
        SearchEngine::DuckDuckGo => format!("https://duckduckgo.com/?q={q}"),
        SearchEngine::Wikipedia => {
            // `fulltext=1` forces the search-results page; without it Wikipedia
            // redirects an exact title match straight to the article.
            format!("https://en.wikipedia.org/wiki/Special:Search?search={q}&fulltext=1")
        }
        SearchEngine::Youtube => format!("https://www.youtube.com/results?search_query={q}"),
        SearchEngine::Reddit => format!("https://www.reddit.com/search/?q={q}"),
        SearchEngine::Amazon => format!("https://www.amazon.com/s?k={q}"),
        SearchEngine::Github => {
            unreachable!("github uses the REST Search API, not the browser")
        }
    }
}

/// One scraped SERP row, as emitted by the per-engine extractors ([`scrape_js`]).
#[derive(Deserialize)]
struct ScrapedRow {
    url: String,
    title: String,
    #[serde(default)]
    content: String,
}

#[derive(Deserialize)]
struct CreateTabResponse {
    #[serde(rename = "tabId")]
    tab_id: String,
}

#[derive(Deserialize)]
struct CamofoxApiResponse {
    #[serde(default = "default_true")]
    ok: bool,
    #[serde(default)]
    error: Option<String>,
    #[serde(default)]
    result: Option<serde_json::Value>,
}

fn default_true() -> bool {
    true
}

#[derive(Deserialize)]
struct ListTabsResponse {
    #[serde(default = "default_true")]
    ok: bool,
    #[serde(default)]
    error: Option<String>,
    #[serde(default)]
    tabs: Vec<ListedTab>,
}

#[derive(Deserialize)]
struct ListedTab {
    #[serde(rename = "tabId")]
    tab_id: String,
    #[serde(default)]
    url: String,
}

/// GitHub REST Search API (`/search/repositories`) response — only the fields
/// we map into a result row.
#[derive(Deserialize)]
struct GithubSearchResponse {
    #[serde(default)]
    items: Vec<GithubRepo>,
}

#[derive(Deserialize)]
struct GithubRepo {
    html_url: String,
    full_name: String,
    #[serde(default)]
    description: Option<String>,
}

/// Search client backed by a camofox-browser REST endpoint. Returns the same
/// [`SearxngResponse`] shape as [`crate::client::SearxngClient`] so callers can
/// treat the two interchangeably.
pub struct CamofoxSearchClient {
    http: reqwest::Client,
    /// Reads Google redirect targets: never follows redirects and keeps no
    /// cookies, so it only fetches the `Location` of each `/goto` link.
    redirect_http: reqwest::Client,
    base_url: String,
    api_key: Option<String>,
    /// Optional GitHub PAT for the `github` engine, which uses the GitHub REST
    /// Search API (not the browser) because GitHub web search rate-limits
    /// unauthenticated scraping. `None` falls back to the lower unauth quota.
    github_token: Option<String>,
    /// Base URL of the GitHub REST API for the `github` engine. Always
    /// `https://api.github.com` in production; overridden in tests to point at
    /// a mock server.
    github_api_base: String,
    timeout: Duration,
    workers: Vec<CamofoxSearchWorker>,
    available_workers: tokio::sync::Semaphore,
    /// Camofox persistent contexts crash when multiple cold workers launch at
    /// once. Serialize only the rare create-tab path across this client; warm
    /// navigation and evaluation never acquire this gate.
    launch_gate: tokio::sync::Mutex<()>,
}

struct CamofoxSearchWorker {
    user_id: String,
    session_key: String,
    /// Lazily created warm tab, reused across every query assigned to this
    /// worker and reset independently when it goes stale.
    tab: tokio::sync::Mutex<Option<String>>,
}

impl CamofoxSearchClient {
    /// Build a client pointed at the camofox-browser base URL
    /// (e.g. `http://camofox:9377`). `timeout` caps each HTTP round-trip.
    /// `github_token` authenticates the `github` engine's Search-API calls.
    pub fn new(
        base_url: impl Into<String>,
        api_key: Option<String>,
        github_token: Option<String>,
        timeout: Duration,
    ) -> Self {
        Self::new_with_pool_size(base_url, api_key, github_token, timeout, 1)
    }

    /// Build a client with up to `pool_size` simultaneous Camofox searches.
    /// Each worker owns a distinct persistent session identity and warm tab.
    /// A zero size is treated as one so the client always remains usable.
    pub fn new_with_pool_size(
        base_url: impl Into<String>,
        api_key: Option<String>,
        github_token: Option<String>,
        timeout: Duration,
        pool_size: usize,
    ) -> Self {
        let pool_size = pool_size.clamp(1, 8);
        let base_url = base_url.into().trim_end_matches('/').to_string();
        let http = reqwest::Client::builder()
            .timeout(timeout)
            .build()
            .unwrap_or_else(|_| reqwest::Client::new());
        let redirect_http = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .timeout(REDIRECT_RESOLVE_BUDGET)
            .build()
            .unwrap_or_else(|_| reqwest::Client::new());
        let workers = (0..pool_size)
            .map(|index| CamofoxSearchWorker {
                user_id: if pool_size == 1 {
                    USER_ID.to_string()
                } else {
                    format!("{USER_ID}-{index}")
                },
                session_key: if pool_size == 1 {
                    SESSION_KEY.to_string()
                } else {
                    format!("{SESSION_KEY}-{index}")
                },
                tab: tokio::sync::Mutex::new(None),
            })
            .collect();
        Self {
            http,
            redirect_http,
            base_url,
            api_key,
            github_token,
            github_api_base: "https://api.github.com".to_string(),
            timeout,
            workers,
            available_workers: tokio::sync::Semaphore::new(pool_size),
            launch_gate: tokio::sync::Mutex::new(()),
        }
    }

    /// Configured base URL (trailing slash trimmed). Mirrors
    /// [`SearxngClient::base_url`](crate::client::SearxngClient::base_url) so the
    /// route layer can name the host in errors uniformly.
    pub fn base_url(&self) -> &str {
        &self.base_url
    }

    /// Base URL the `github` engine calls instead of the browser, so errors from
    /// that engine can name the host that actually failed.
    pub fn github_api_base(&self) -> &str {
        &self.github_api_base
    }

    fn auth(&self, req: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        match &self.api_key {
            Some(k) => req.bearer_auth(k),
            None => req,
        }
    }

    async fn post(
        &self,
        path: &str,
        body: serde_json::Value,
    ) -> Result<reqwest::Response, SearchError> {
        self.auth(self.http.post(format!("{}{path}", self.base_url)))
            .json(&body)
            .send()
            .await
            .map_err(|e: reqwest::Error| {
                if e.is_timeout() {
                    SearchError::Timeout
                } else {
                    SearchError::Transport(e.without_url().to_string())
                }
            })
    }

    async fn api_response(
        response: reqwest::Response,
        operation: &str,
    ) -> Result<CamofoxApiResponse, SearchError> {
        if !response.status().is_success() {
            return Err(upstream_error(operation, response).await);
        }
        let body = response.json::<CamofoxApiResponse>().await.map_err(|e| {
            SearchError::InvalidResponse(format!(
                "camofox: bad {operation} response: {}",
                crw_core::error::reqwest_message(e)
            ))
        })?;
        if !body.ok {
            return Err(SearchError::Upstream {
                status: 502,
                body: format!(
                    "camofox: {operation} rejected: {}",
                    body.error.as_deref().unwrap_or("unknown upstream error")
                ),
            });
        }
        Ok(body)
    }

    async fn current_tab_url(
        &self,
        worker: &CamofoxSearchWorker,
        tab_id: &str,
    ) -> Result<Option<String>, SearchError> {
        let mut url = url::Url::parse(&format!("{}/tabs", self.base_url))
            .map_err(|e| SearchError::Transport(format!("camofox: invalid tabs URL: {e}")))?;
        url.query_pairs_mut().append_pair("userId", &worker.user_id);
        let response =
            self.auth(self.http.get(url))
                .send()
                .await
                .map_err(|e: reqwest::Error| {
                    if e.is_timeout() {
                        SearchError::Timeout
                    } else {
                        SearchError::Transport(e.without_url().to_string())
                    }
                })?;
        if !response.status().is_success() {
            return Err(upstream_error("list tabs", response).await);
        }
        let body = response.json::<ListTabsResponse>().await.map_err(|e| {
            SearchError::InvalidResponse(format!(
                "camofox: bad list tabs response: {}",
                crw_core::error::reqwest_message(e)
            ))
        })?;
        if !body.ok {
            return Err(SearchError::Upstream {
                status: 502,
                body: format!(
                    "camofox: list tabs rejected: {}",
                    body.error.as_deref().unwrap_or("unknown upstream error")
                ),
            });
        }
        Ok(body
            .tabs
            .into_iter()
            .find(|tab| tab.tab_id == tab_id)
            .map(|tab| tab.url))
    }

    /// Run the requested engines via Camofox and map the merged rows into a
    /// [`SearxngResponse`]. Typed [`SearchError`]s match the SearXNG client so
    /// the route layer's existing error mapping applies unchanged.
    ///
    /// Checks out one worker for the full query, serializing only with other
    /// searches assigned to that worker. The
    /// engines in `params.camofox_engines` are run sequentially on that tab
    /// (the single-tab design dodges camofox's teardown race, so fan-out is
    /// serial — N engines ≈ N× latency). A stale tab is recreated and the
    /// engine retried once. An engine that still fails is skipped; results from
    /// the engines that succeeded are merged and returned. Only when *every*
    /// engine fails is the last error surfaced.
    pub async fn fetch(&self, params: &SearxngParams) -> Result<SearxngResponse, SearchError> {
        let _permit = self
            .available_workers
            .acquire()
            .await
            .expect("Camofox worker semaphore is never closed");
        let (worker_index, mut tab) = loop {
            if let Some(checked_out) = self
                .workers
                .iter()
                .enumerate()
                .find_map(|(index, worker)| worker.tab.try_lock().ok().map(|tab| (index, tab)))
            {
                break checked_out;
            }
            tokio::task::yield_now().await;
        };
        let worker = &self.workers[worker_index];
        let mut all: Vec<SearxngResult> = Vec::new();
        let mut last_err: Option<SearchError> = None;
        let mut any_ok = false;
        // Engines that failed OR returned zero rows, so a caller can tell a
        // blocked/consent-walled engine (0 rows despite HTTP 200) from a genuine
        // "no matches". Surfaced via SearxngResponse.unresponsive_engines →
        // response warnings; without it a hung/blocked engine looks like a clean
        // empty success (the exact Bing/Google-from-a-flagged-IP silent failure).
        let mut unresponsive: Vec<serde_json::Value> = Vec::new();

        for &engine in &params.camofox_engines {
            let label = engine.label();
            // GitHub uses the REST Search API, not the browser — no tab, no
            // stale-tab retry. Every other engine drives the warm camofox tab.
            let outcome = if matches!(engine, SearchEngine::Github) {
                self.github_search(&params.q).await
            } else {
                let outcome = match self.attempt(worker, &mut tab, engine, params).await {
                    Ok(rows) => Ok(rows),
                    Err(e) if is_stale_tab(&e) => {
                        // Warm tab/context died (idle eviction or camofox
                        // restart). Drop the dead id, let any in-flight relaunch
                        // settle, then recreate and retry this engine once.
                        *tab = None;
                        tokio::time::sleep(RETRY_BACKOFF).await;
                        self.attempt(worker, &mut tab, engine, params).await
                    }
                    Err(e) => Err(e),
                };
                if matches!(outcome, Err(SearchError::Timeout)) {
                    self.abandon_tab(worker, &mut tab).await;
                }
                outcome
            };
            match outcome {
                Ok(rows) => {
                    any_ok = true;
                    if rows.is_empty() {
                        // Zero rows is either a genuine empty result or a
                        // bot-wall/consent page served as HTTP 200 — the scrape
                        // can't tell them apart, so word it neutrally.
                        unresponsive.push(serde_json::json!([
                            label,
                            "returned no results (no matches, or a bot wall / consent page)"
                        ]));
                    }
                    all.extend(rows);
                }
                Err(e) => {
                    // The API response gets only the stripped reason; the full
                    // error (with camofox's message) is only visible here.
                    tracing::warn!(engine = label, error = %e, "camofox: engine failed");
                    unresponsive.push(serde_json::json!([label, engine_failure_reason(&e)]));
                    last_err = Some(e);
                }
            }
        }

        if !any_ok {
            return Err(last_err.unwrap_or(SearchError::Timeout));
        }
        Ok(merge_results(params.q.clone(), all, unresponsive))
    }

    /// Ensure a warm tab exists, then run one engine's search against it. Caller
    /// holds the tab mutex, so this is the single in-flight search.
    async fn attempt(
        &self,
        worker: &CamofoxSearchWorker,
        tab: &mut Option<String>,
        engine: SearchEngine,
        params: &SearxngParams,
    ) -> Result<Vec<SearxngResult>, SearchError> {
        let deadline = Instant::now() + self.timeout;
        match tokio::time::timeout(self.timeout + Duration::from_millis(50), async {
            let tab_id = self.ensure_tab(worker, tab).await?;
            self.run_search(worker, &tab_id, engine, params, deadline)
                .await
        })
        .await
        {
            Ok(result) => result,
            Err(_) => Err(SearchError::Timeout),
        }
    }

    /// Return the warm tab id, creating one if we don't have it cached. The id
    /// is cached back into `tab` so subsequent searches reuse it.
    async fn ensure_tab(
        &self,
        worker: &CamofoxSearchWorker,
        tab: &mut Option<String>,
    ) -> Result<String, SearchError> {
        if let Some(id) = tab.as_ref() {
            return Ok(id.clone());
        }
        let _launch = self.launch_gate.lock().await;
        // Re-check after waiting: keep this correct if tab ownership is ever
        // narrowed so another task can warm the same worker while we queue.
        if let Some(id) = tab.as_ref() {
            return Ok(id.clone());
        }
        let create = self
            .post(
                "/tabs",
                json!({ "userId": worker.user_id, "sessionKey": worker.session_key }),
            )
            .await?;
        if !create.status().is_success() {
            return Err(upstream_error("create tab", create).await);
        }
        let id = create
            .json::<CreateTabResponse>()
            .await
            .map_err(|e| {
                SearchError::InvalidResponse(format!(
                    "camofox: bad /tabs response: {}",
                    crw_core::error::reqwest_message(e)
                ))
            })?
            .tab_id;
        *tab = Some(id.clone());
        Ok(id)
    }

    /// Mint the replacement before deleting the old tab, avoiding Camofox's
    /// eager zero-tab context teardown. Both operations are bounded separately;
    /// failed prewarming leaves no cached id, so the next query can retry create.
    async fn abandon_tab(&self, worker: &CamofoxSearchWorker, tab: &mut Option<String>) {
        let Some(old) = tab.take() else { return };
        match tokio::time::timeout(CLEANUP_BUDGET, self.ensure_tab(worker, tab)).await {
            Ok(Ok(_)) => {}
            Ok(Err(error)) => tracing::debug!(%error, "camofox: replacement tab creation failed"),
            Err(_) => tracing::debug!("camofox: replacement tab creation timed out"),
        }
        let close = self
            .auth(self.http.delete(format!("{}/tabs/{old}", self.base_url)))
            .json(&json!({ "userId": worker.user_id }))
            .send();
        match tokio::time::timeout(CLEANUP_BUDGET, close).await {
            Ok(Ok(response))
                if response.status().is_success()
                    || matches!(response.status().as_u16(), 404 | 410) => {}
            Ok(Ok(response)) => tracing::warn!(
                status = response.status().as_u16(),
                "camofox: abandoned tab deletion rejected"
            ),
            Ok(Err(error)) => {
                tracing::warn!(error = %error.without_url(), "camofox: abandoned tab deletion failed")
            }
            Err(_) => tracing::warn!("camofox: abandoned tab deletion timed out"),
        }
    }

    async fn run_search(
        &self,
        worker: &CamofoxSearchWorker,
        tab_id: &str,
        engine: SearchEngine,
        params: &SearxngParams,
        deadline: Instant,
    ) -> Result<Vec<SearxngResult>, SearchError> {
        let target_url = search_target_url(engine, &params.q);
        if Instant::now() >= deadline {
            return Err(SearchError::Timeout);
        }
        // Both Camofox 2.4.6 and 2.4.8 ignore a request `timeout` and use
        // their own 30-second navigation budget. attempt() enforces our total
        // deadline; fetch() replaces a tab that may still be busy upstream.
        let navigation = self
            .post(
                &format!("/tabs/{tab_id}/navigate"),
                json!({
                    "userId": worker.user_id,
                    "url": target_url,
                }),
            )
            .await?;
        Self::api_response(navigation, "navigate").await?;
        loop {
            if Instant::now() >= deadline {
                return Ok(Vec::new());
            }
            match self.current_tab_url(worker, tab_id).await? {
                None => {
                    return Err(SearchError::Upstream {
                        status: 404,
                        body: "camofox: warm search tab disappeared".to_string(),
                    });
                }
                Some(url) if is_challenge_url(engine, &url) => return Ok(Vec::new()),
                Some(url) if url_matches_search(engine, &url, &params.q) => break,
                Some(_) => {
                    let remaining = deadline.saturating_duration_since(Instant::now());
                    tokio::time::sleep(URL_POLL_INTERVAL.min(remaining)).await;
                }
            }
        }

        // Do not call `/wait`: Camofox 2.4.6 can keep that route alive beyond
        // the caller's budget. Extraction polling stays on non-blocking API
        // calls and treats a slow/challenged engine as an explicit empty result.
        let remaining = deadline.saturating_duration_since(Instant::now());
        tokio::time::sleep(RENDER_SETTLE.min(remaining)).await;
        while Instant::now() < deadline {
            match self.evaluate_rows(worker, tab_id, engine).await {
                Ok(rows) if !rows.is_empty() => {
                    return Ok(match engine {
                        SearchEngine::Google => {
                            resolve_google_links(&self.redirect_http, rows).await
                        }
                        _ => rows,
                    });
                }
                // A dead tab cannot recover through polling. Let fetch's
                // bounded stale-tab retry replace it before trying again.
                Err(error) if is_stale_tab(&error) => return Err(error),
                Ok(_) | Err(_) => {
                    let remaining = deadline.saturating_duration_since(Instant::now());
                    tokio::time::sleep(URL_POLL_INTERVAL.min(remaining)).await;
                }
            }
        }
        Ok(Vec::new())
    }

    async fn evaluate_rows(
        &self,
        worker: &CamofoxSearchWorker,
        tab_id: &str,
        engine: SearchEngine,
    ) -> Result<Vec<SearxngResult>, SearchError> {
        let eval = self
            .post(
                &format!("/tabs/{tab_id}/evaluate"),
                json!({ "userId": worker.user_id, "expression": scrape_js(engine) }),
            )
            .await?;
        let body = Self::api_response(eval, "evaluate results").await?;
        let raw = match body.result {
            Some(serde_json::Value::String(value)) => value,
            None | Some(serde_json::Value::Null) => String::new(),
            Some(other) => {
                return Err(SearchError::InvalidResponse(format!(
                    "camofox: evaluate result was not a string: {other}"
                )));
            }
        };

        let rows: Vec<ScrapedRow> = if raw.trim().is_empty() {
            Vec::new()
        } else {
            serde_json::from_str(&raw)
                .map_err(|e| SearchError::InvalidResponse(format!("camofox: scrape JSON: {e}")))?
        };

        let n = rows.len();
        let label = engine.label();
        let results = rows
            .into_iter()
            .enumerate()
            .map(|(i, r)| SearxngResult {
                url: Some(r.url),
                title: Some(r.title),
                engine: Some(label.to_string()),
                content: (!r.content.is_empty()).then_some(r.content),
                // Synthesize a descending score from SERP position so the
                // existing score-sort in transform.rs preserves engine order.
                score: Some((n - i) as f64),
                engines: vec![label.to_string()],
                positions: vec![(i + 1) as u32],
                category: Some("general".to_string()),
                template: None,
                published_date: None,
                img_src: None,
                thumbnail_src: None,
                img_format: None,
                resolution: None,
            })
            .collect();

        Ok(results)
    }

    /// Search GitHub repositories via the REST Search API. Used instead of the
    /// browser because GitHub's web search rate-limits unauthenticated scraping
    /// almost immediately; the API gives clean JSON and a token lifts the quota.
    /// GitHub requires a `User-Agent`; the token (when set) is sent as a bearer.
    async fn github_search(&self, query: &str) -> Result<Vec<SearxngResult>, SearchError> {
        let q: String = url::form_urlencoded::byte_serialize(query.as_bytes()).collect();
        let url = format!(
            "{}/search/repositories?q={q}&per_page=10",
            self.github_api_base
        );
        let mut req = self
            .http
            .get(url)
            .header("Accept", "application/vnd.github+json")
            .header("User-Agent", "crw-search");
        if let Some(token) = &self.github_token {
            req = req.bearer_auth(token);
        }
        let resp = req.send().await.map_err(|e: reqwest::Error| {
            if e.is_timeout() {
                SearchError::Timeout
            } else {
                SearchError::Transport(e.without_url().to_string())
            }
        })?;
        if !resp.status().is_success() {
            return Err(SearchError::Upstream {
                status: resp.status().as_u16(),
                body: "github: search failed".to_string(),
            });
        }
        let data = resp.json::<GithubSearchResponse>().await.map_err(|e| {
            SearchError::InvalidResponse(format!(
                "github: bad search response: {}",
                crw_core::error::reqwest_message(e)
            ))
        })?;

        let n = data.items.len();
        Ok(data
            .items
            .into_iter()
            .enumerate()
            .map(|(i, r)| SearxngResult {
                url: Some(r.html_url),
                title: Some(r.full_name),
                engine: Some("github".to_string()),
                content: r.description.filter(|d| !d.is_empty()),
                score: Some((n - i) as f64),
                engines: vec!["github".to_string()],
                positions: vec![(i + 1) as u32],
                category: Some("general".to_string()),
                template: None,
                published_date: None,
                img_src: None,
                thumbnail_src: None,
                img_format: None,
                resolution: None,
            })
            .collect())
    }
}

/// Merge per-engine result rows into one response, deduped by URL. A URL seen
/// by multiple engines accumulates their `engines`/`positions` and sums their
/// position-scores, so cross-engine agreement ranks higher. First-appearance
/// order is preserved; downstream `rerank` does the final ordering.
/// Concise, user-facing reason for an engine failure — no internal detail, just
/// enough to tell a timeout/block apart. Feeds `unresponsive_engines`.
fn is_google_host(url: &url::Url) -> bool {
    url.host_str()
        .is_some_and(|host| host == "google.com" || host.ends_with(".google.com"))
}

/// Google wraps result links in `/goto?url=<token>` (or `/url?…`). The token
/// is encrypted, so only Google can map it to the destination.
fn is_google_redirect(url: &str) -> bool {
    url::Url::parse(url).is_ok_and(|u| is_google_host(&u) && matches!(u.path(), "/goto" | "/url"))
}

/// A row pointing back into Google itself (an AI Mode reply, related
/// searches) rather than at a result page.
fn is_google_internal(url: &str) -> bool {
    url::Url::parse(url).is_ok_and(|u| is_google_host(&u))
}

/// The redirect target of `url`, without following it. `None` unless the
/// response is a redirect to an http(s) URL.
async fn resolve_location(http: &reqwest::Client, url: &str) -> Option<String> {
    let response = http.get(url).send().await.ok()?;
    if !response.status().is_redirection() {
        return None;
    }
    let location = response
        .headers()
        .get(reqwest::header::LOCATION)?
        .to_str()
        .ok()?;
    let target = url::Url::parse(url).ok()?.join(location).ok()?;
    // Google answers a rate-limited or cookieless request with a redirect to
    // its own consent or `/sorry` interstitial, which is not the result. Leave
    // the row on its (working) redirect URL instead of dropping it as internal.
    if is_google_interstitial(&target) {
        return None;
    }
    matches!(target.scheme(), "http" | "https").then(|| target.to_string())
}

/// Google's consent wall or rate-limit (`/sorry`) page.
fn is_google_interstitial(url: &url::Url) -> bool {
    match url.host_str() {
        Some("consent.google.com") => true,
        Some(_) if is_google_host(url) => url.path().starts_with("/sorry"),
        _ => false,
    }
}

/// Replace Google redirect links with their destinations, resolving the whole
/// page in parallel within [`REDIRECT_RESOLVE_BUDGET`]. Unresolved links keep
/// the redirect URL (it still works when opened); rows that stay inside
/// Google are dropped.
async fn resolve_google_links(
    http: &reqwest::Client,
    rows: Vec<SearxngResult>,
) -> Vec<SearxngResult> {
    let lookups = rows.iter().map(|row| async move {
        match row.url.as_deref() {
            Some(url) if is_google_redirect(url) => resolve_location(http, url).await,
            _ => None,
        }
    });
    let resolved =
        tokio::time::timeout(REDIRECT_RESOLVE_BUDGET, futures::future::join_all(lookups))
            .await
            .unwrap_or_default();
    rows.into_iter()
        .enumerate()
        .filter_map(|(i, mut row)| {
            if let Some(Some(target)) = resolved.get(i) {
                row.url = Some(target.clone());
            }
            let keep = row
                .url
                .as_deref()
                .is_some_and(|url| is_google_redirect(url) || !is_google_internal(url));
            keep.then_some(row)
        })
        .collect()
}

/// Cap on how much of camofox's `error` message is carried into the error.
/// The route layer trims `Upstream.body` again (to 200 chars) before it reaches
/// an HTTP client; this cap only bounds what lands in logs.
const UPSTREAM_BODY_CAP: usize = 300;

/// Turn a non-2xx camofox response into an `Upstream` error carrying camofox's
/// own `error` message (e.g. a profile/Camoufox version mismatch), so the
/// cause is visible instead of a bare status. Only that JSON field passes
/// through: a non-JSON body (a proxy's HTML error page, a crash trace) is
/// logged, not surfaced, since `Upstream.body` reaches API responses. `what`
/// names the step (`create tab`, `navigate`, `list tabs`).
async fn upstream_error(what: &str, resp: reqwest::Response) -> SearchError {
    let status = resp.status().as_u16();
    let raw = match read_capped(resp, MAX_ERROR_BODY_BYTES).await {
        Ok(b) => String::from_utf8_lossy(&b).into_owned(),
        Err(e) => {
            tracing::debug!(status, step = what, error = %e, "camofox: error body unreadable");
            String::new()
        }
    };
    let detail = serde_json::from_str::<serde_json::Value>(&raw)
        .ok()
        .and_then(|v| v.get("error")?.as_str().map(str::to_string))
        .unwrap_or_else(|| {
            if !raw.trim().is_empty() {
                tracing::debug!(status, step = what, body = %raw.trim(), "camofox: non-JSON error body");
            }
            String::new()
        });
    let detail: String = detail.trim().chars().take(UPSTREAM_BODY_CAP).collect();
    let body = if detail.is_empty() {
        format!("camofox: {what} failed")
    } else {
        format!("camofox: {what} failed: {detail}")
    };
    SearchError::Upstream { status, body }
}

fn engine_failure_reason(e: &SearchError) -> String {
    match e {
        SearchError::Timeout => "timed out".to_string(),
        SearchError::Upstream { status, .. } => format!("upstream error (HTTP {status})"),
        SearchError::InvalidResponse(_) => "unreadable response".to_string(),
        _ => "request failed".to_string(),
    }
}

fn query_value(url: &url::Url, key: &str) -> Option<String> {
    url.query_pairs()
        .find_map(|(name, value)| (name == key).then(|| value.into_owned()))
}

/// A warm tab can still report the previous query while navigation settles.
/// Require both the expected engine route and the current query
/// before extraction so stale rows cannot be returned for a new request.
fn url_matches_search(engine: SearchEngine, observed: &str, query: &str) -> bool {
    let Ok(url) = url::Url::parse(observed) else {
        return false;
    };
    let host = url.host_str().unwrap_or_default();
    match engine {
        SearchEngine::Google => {
            (host == "google.com" || host.ends_with(".google.com"))
                && url.path().starts_with("/search")
                && query_value(&url, "q").as_deref() == Some(query)
        }
        SearchEngine::Bing => {
            (host == "bing.com" || host.ends_with(".bing.com"))
                && url.path().starts_with("/search")
                && query_value(&url, "q").as_deref() == Some(query)
        }
        SearchEngine::DuckDuckGo => {
            (host == "duckduckgo.com" || host.ends_with(".duckduckgo.com"))
                && query_value(&url, "q").as_deref() == Some(query)
        }
        SearchEngine::Wikipedia => {
            host == "en.wikipedia.org"
                && url.path().contains("Special:Search")
                && query_value(&url, "search").as_deref() == Some(query)
        }
        SearchEngine::Youtube => {
            (host == "youtube.com" || host.ends_with(".youtube.com"))
                && url.path().starts_with("/results")
                && query_value(&url, "search_query").as_deref() == Some(query)
        }
        SearchEngine::Reddit => {
            (host == "reddit.com" || host.ends_with(".reddit.com"))
                && url.path().starts_with("/search")
                && query_value(&url, "q").as_deref() == Some(query)
        }
        SearchEngine::Amazon => {
            (host == "amazon.com" || host.ends_with(".amazon.com"))
                && url.path().starts_with("/s")
                && query_value(&url, "k").as_deref() == Some(query)
        }
        SearchEngine::Github => false,
    }
}

fn is_challenge_url(engine: SearchEngine, observed: &str) -> bool {
    let Ok(url) = url::Url::parse(observed) else {
        return false;
    };
    let host = url.host_str().unwrap_or_default();
    match engine {
        SearchEngine::Google => {
            (host == "google.com" || host.ends_with(".google.com"))
                && (url.path().starts_with("/sorry/")
                    || host.starts_with("consent.")
                    || url.path().starts_with("/consent"))
        }
        _ => false,
    }
}

fn merge_results(
    query: String,
    rows: Vec<SearxngResult>,
    unresponsive_engines: Vec<serde_json::Value>,
) -> SearxngResponse {
    use std::collections::HashMap;
    let mut order: Vec<String> = Vec::new();
    let mut by_url: HashMap<String, SearxngResult> = HashMap::new();
    for r in rows {
        let key = r.url.clone().unwrap_or_default();
        if let Some(existing) = by_url.get_mut(&key) {
            existing.engines.extend(r.engines);
            existing.positions.extend(r.positions);
            existing.score = Some(existing.score.unwrap_or(0.0) + r.score.unwrap_or(0.0));
        } else {
            order.push(key.clone());
            by_url.insert(key, r);
        }
    }
    let results: Vec<SearxngResult> = order
        .into_iter()
        .filter_map(|k| by_url.remove(&k))
        .collect();
    SearxngResponse {
        query,
        number_of_results: results.len() as u64,
        results,
        unresponsive_engines,
        ..Default::default()
    }
}

/// Whether an error means the warm tab/context is gone and recreating it could
/// recover — a missing tab (404), a destroyed timed-out tab (410), a server-side
/// fault like `window is null` (5xx), or a dropped connection during a relaunch.
/// Client-side timeouts rotate the tab for the next engine but are not retried;
/// malformed responses are reported without replacing the tab.
fn is_stale_tab(e: &SearchError) -> bool {
    match e {
        SearchError::Upstream { status, .. } => matches!(*status, 404 | 410) || *status >= 500,
        SearchError::Transport(_) => true,
        SearchError::Timeout | SearchError::InvalidResponse(_) => false,
    }
}

#[cfg(test)]
mod extractor_tests {
    use super::*;
    use crw_core::types::SearchEngine;

    #[test]
    fn browser_engines_have_dedicated_extractors() {
        // GitHub is excluded: it uses the REST Search API, not the browser.
        assert!(scrape_js(SearchEngine::Google).contains("div.g"));
        assert!(scrape_js(SearchEngine::Bing).contains("li.b_algo"));
        assert!(scrape_js(SearchEngine::DuckDuckGo).contains("article"));
        assert!(scrape_js(SearchEngine::Wikipedia).contains("mw-search-result-heading"));
        assert!(scrape_js(SearchEngine::Youtube).contains("ytd-video-renderer"));
        assert!(scrape_js(SearchEngine::Reddit).contains("/comments/"));
        assert!(scrape_js(SearchEngine::Amazon).contains("s-search-result"));
    }

    #[test]
    fn browser_engines_have_query_specific_target_urls() {
        let cases = [
            (
                SearchEngine::Google,
                "https://www.google.com/search?q=rust+lang",
            ),
            (
                SearchEngine::Bing,
                "https://www.bing.com/search?q=rust+lang",
            ),
            (
                SearchEngine::DuckDuckGo,
                "https://duckduckgo.com/?q=rust+lang",
            ),
            (
                SearchEngine::Wikipedia,
                "https://en.wikipedia.org/wiki/Special:Search?search=rust+lang&fulltext=1",
            ),
            (
                SearchEngine::Youtube,
                "https://www.youtube.com/results?search_query=rust+lang",
            ),
            (
                SearchEngine::Reddit,
                "https://www.reddit.com/search/?q=rust+lang",
            ),
            (SearchEngine::Amazon, "https://www.amazon.com/s?k=rust+lang"),
        ];
        for (engine, expected) in cases {
            let target = search_target_url(engine, "rust lang");
            assert_eq!(target, expected);
            assert!(url_matches_search(engine, &target, "rust lang"));
            assert!(!url_matches_search(engine, &target, "old query"));
        }
    }

    #[test]
    fn google_challenge_is_not_a_matching_result_page() {
        let challenge = "https://www.google.com/sorry/index?continue=x";
        assert!(is_challenge_url(SearchEngine::Google, challenge));
        assert!(!url_matches_search(SearchEngine::Google, challenge, "rust"));
    }

    #[test]
    fn engine_failure_reason_is_concise_and_leaks_no_internals() {
        assert_eq!(engine_failure_reason(&SearchError::Timeout), "timed out");
        assert_eq!(
            engine_failure_reason(&SearchError::Upstream {
                status: 503,
                body: "secret internal detail".into(),
            }),
            "upstream error (HTTP 503)"
        );
        // The upstream body (potential internal detail) must not leak through.
        assert!(
            !engine_failure_reason(&SearchError::Upstream {
                status: 500,
                body: "stacktrace".into(),
            })
            .contains("stacktrace")
        );
    }

    #[test]
    fn merge_results_propagates_unresponsive_engines() {
        // A zero-row / errored engine is carried on the response so the route can
        // warn instead of returning a silent empty success.
        let resp = merge_results(
            "q".into(),
            vec![],
            vec![serde_json::json!(["bing", "timed out"])],
        );
        assert!(resp.results.is_empty());
        assert_eq!(resp.unresponsive_engines.len(), 1);
        assert_eq!(resp.unresponsive_engines[0][0], "bing");
        assert_eq!(resp.unresponsive_engines[0][1], "timed out");
    }
}

#[cfg(test)]
mod github_api_tests {
    use super::*;
    use wiremock::matchers::{method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    /// A tab can disappear after URL polling succeeded. Propagate that
    /// extraction failure to the existing one-shot stale-tab recovery.
    #[tokio::test]
    async fn fetch_recovers_tab_lost_during_result_evaluation() {
        let server = MockServer::start().await;
        let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(3));
        *client.workers[0].tab.lock().await = Some("t1".into());
        Mock::given(method("POST"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "t2" })))
            .expect(1)
            .mount(&server)
            .await;
        Mock::given(method("GET"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "ok": true,
                "tabs": [
                    { "tabId": "t1", "url": "https://www.google.com/search?q=rust" },
                    { "tabId": "t2", "url": "https://www.google.com/search?q=rust" }
                ]
            })))
            .mount(&server)
            .await;
        for tab in ["t1", "t2"] {
            Mock::given(method("POST"))
                .and(path(format!("/tabs/{tab}/navigate")))
                .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                    "ok": true, "result": true
                })))
                .with_priority(1)
                .mount(&server)
                .await;
        }
        Mock::given(method("POST"))
            .and(path("/tabs/t1/evaluate"))
            .respond_with(ResponseTemplate::new(404).set_body_json(json!({
                "error": "tab not found"
            })))
            .with_priority(10)
            .expect(1)
            .mount(&server)
            .await;
        let rows = r#"[{"url":"https://rust-lang.org","title":"Rust","content":"language"}]"#;
        Mock::given(method("POST"))
            .and(path("/tabs/t2/evaluate"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "ok": true, "result": rows
            })))
            .with_priority(10)
            .expect(1)
            .mount(&server)
            .await;
        let response = client
            .fetch(&SearxngParams {
                q: "rust".into(),
                camofox_engines: vec![SearchEngine::Google],
                ..Default::default()
            })
            .await
            .expect("the stale tab is retried on a fresh tab");
        assert_eq!(response.results.len(), 1);
        assert!(response.unresponsive_engines.is_empty());
        assert_eq!(client.workers[0].tab.lock().await.as_deref(), Some("t2"));
        server.verify().await;
    }

    /// `github_search` hits the REST Search API and maps `items[]` into result
    /// rows: `html_url` → url, `full_name` → title, `description` → content,
    /// with the engine tagged `github` and descending position scores.
    #[tokio::test]
    async fn github_search_maps_items_to_rows() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/search/repositories"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "items": [
                    { "html_url": "https://github.com/a/one", "full_name": "a/one", "description": "first" },
                    { "html_url": "https://github.com/b/two", "full_name": "b/two", "description": null },
                ]
            })))
            .mount(&server)
            .await;

        let mut client = CamofoxSearchClient::new(
            "http://unused",
            None,
            Some("tok".into()),
            Duration::from_secs(5),
        );
        client.github_api_base = server.uri();

        let rows = client
            .github_search("rust")
            .await
            .expect("github search ok");
        assert_eq!(rows.len(), 2);

        let first = &rows[0];
        assert_eq!(first.url.as_deref(), Some("https://github.com/a/one"));
        assert_eq!(first.title.as_deref(), Some("a/one"));
        assert_eq!(first.content.as_deref(), Some("first"));
        assert_eq!(first.engine.as_deref(), Some("github"));
        // A null description maps to no content.
        assert_eq!(rows[1].content, None);
        // Descending score by position so the merge ranks earlier hits higher.
        assert!(rows[0].score.unwrap() > rows[1].score.unwrap());
    }

    /// A non-2xx GitHub response surfaces as an `Upstream` error (not a panic or
    /// silent empty), so the fetch loop records it and skips the engine.
    #[tokio::test]
    async fn github_search_maps_error_status() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/search/repositories"))
            .respond_with(ResponseTemplate::new(403))
            .mount(&server)
            .await;

        let mut client =
            CamofoxSearchClient::new("http://unused", None, None, Duration::from_secs(5));
        client.github_api_base = server.uri();

        let err = client.github_search("rust").await.unwrap_err();
        assert!(matches!(err, SearchError::Upstream { status: 403, .. }));
    }

    /// Partial-failure skip: a multi-engine fetch where one engine fails must
    /// still return the others' rows (the failed engine is skipped, not fatal).
    /// Here Google (browser) succeeds and GitHub (API) 500s; the response holds
    /// only Google's row.
    #[tokio::test]
    async fn fetch_skips_failed_engine_and_returns_partial() {
        let server = MockServer::start().await;
        // Camofox browser flow for the Google engine → one row.
        let rows = serde_json::to_string(&json!([
            { "url": "https://rust-lang.org", "title": "Rust", "content": "" },
        ]))
        .unwrap();
        Mock::given(method("POST"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "t1" })))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/tabs/t1/navigate"))
            .respond_with(
                ResponseTemplate::new(200).set_body_json(json!({ "ok": true, "result": true })),
            )
            .with_priority(1)
            .mount(&server)
            .await;
        Mock::given(method("GET"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "ok": true,
                "tabs": [{ "tabId": "t1", "url": "https://www.google.com/search?q=rust" }]
            })))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/tabs/t1/evaluate"))
            .respond_with(
                ResponseTemplate::new(200).set_body_json(json!({ "ok": true, "result": rows })),
            )
            .with_priority(10)
            .mount(&server)
            .await;
        // GitHub API fails.
        Mock::given(method("GET"))
            .and(path("/search/repositories"))
            .respond_with(ResponseTemplate::new(500))
            .mount(&server)
            .await;

        let mut client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(5));
        client.github_api_base = server.uri();

        let params = SearxngParams {
            q: "rust".to_string(),
            camofox_engines: vec![SearchEngine::Google, SearchEngine::Github],
            ..Default::default()
        };
        let resp = client
            .fetch(&params)
            .await
            .expect("partial success, not error");
        assert_eq!(resp.results.len(), 1);
        assert_eq!(
            resp.results[0].url.as_deref(),
            Some("https://rust-lang.org")
        );
        assert_eq!(resp.results[0].engine.as_deref(), Some("google"));
    }

    /// When *every* engine fails, the fetch surfaces an error (not an empty Ok).
    #[tokio::test]
    async fn fetch_errors_when_all_engines_fail() {
        let server = MockServer::start().await;
        // Tab creation fails → the Google engine errors; no other engine.
        Mock::given(method("POST"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(500))
            .mount(&server)
            .await;

        let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(5));
        let params = SearxngParams {
            q: "rust".to_string(),
            camofox_engines: vec![SearchEngine::Google],
            ..Default::default()
        };
        assert!(client.fetch(&params).await.is_err());
    }
}

#[cfg(test)]
mod google_redirect_tests {
    use super::*;
    use wiremock::matchers::{method, path, query_param};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    fn row(url: &str) -> SearxngResult {
        SearxngResult {
            url: Some(url.to_string()),
            title: Some("t".to_string()),
            engine: Some("google".to_string()),
            content: None,
            score: None,
            engines: vec![],
            positions: vec![],
            category: None,
            template: None,
            published_date: None,
            img_src: None,
            thumbnail_src: None,
            img_format: None,
            resolution: None,
        }
    }

    /// Routes every request through the mock server, so Google-looking
    /// `http://` URLs are answered locally instead of by Google.
    fn proxied_client(server: &MockServer) -> reqwest::Client {
        reqwest::Client::builder()
            .proxy(reqwest::Proxy::all(server.uri()).unwrap())
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .unwrap()
    }

    #[test]
    fn classifies_google_links() {
        assert!(is_google_redirect("https://www.google.com/goto?url=CAES"));
        assert!(is_google_redirect("https://www.google.com/url?q=x"));
        assert!(!is_google_redirect("https://www.google.com/?ictx=0&sa=X"));
        assert!(!is_google_redirect("https://lightpanda.io/goto"));
        assert!(is_google_internal("https://www.google.com/?ictx=0&sa=X"));
        assert!(!is_google_internal(
            "https://github.com/lightpanda-io/browser"
        ));
    }

    #[tokio::test]
    async fn resolves_redirects_and_drops_google_internal_rows() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/goto"))
            .and(query_param("url", "A"))
            .respond_with(
                ResponseTemplate::new(302).insert_header("location", "https://lightpanda.io/"),
            )
            .mount(&server)
            .await;
        Mock::given(method("GET"))
            .and(path("/goto"))
            .and(query_param("url", "B"))
            .respond_with(ResponseTemplate::new(200))
            .mount(&server)
            .await;
        let rows = vec![
            row("http://www.google.com/goto?url=A"),
            row("http://www.google.com/?ictx=0&sa=X"),
            row("http://www.google.com/goto?url=B"),
            row("https://example.com/direct"),
        ];
        let urls: Vec<String> = resolve_google_links(&proxied_client(&server), rows)
            .await
            .into_iter()
            .filter_map(|r| r.url)
            .collect();
        assert_eq!(
            urls,
            vec![
                "https://lightpanda.io/",
                // Not a redirect response: keeps the working redirect URL.
                "http://www.google.com/goto?url=B",
                "https://example.com/direct",
            ]
        );
    }

    #[tokio::test]
    async fn ignores_relative_and_non_http_locations() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/goto"))
            .and(query_param("url", "rel"))
            .respond_with(ResponseTemplate::new(302).insert_header("location", "/search?q=x"))
            .mount(&server)
            .await;
        Mock::given(method("GET"))
            .and(path("/goto"))
            .and(query_param("url", "js"))
            .respond_with(
                ResponseTemplate::new(302).insert_header("location", "javascript:alert(1)"),
            )
            .mount(&server)
            .await;
        let http = proxied_client(&server);
        // A relative target resolves against Google, so the row is dropped as internal.
        let rows =
            resolve_google_links(&http, vec![row("http://www.google.com/goto?url=rel")]).await;
        assert!(rows.is_empty());
        assert_eq!(
            resolve_location(&http, "http://www.google.com/goto?url=js").await,
            None
        );
    }

    #[tokio::test]
    async fn consent_and_rate_limit_redirects_keep_the_result_row() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/goto"))
            .and(query_param("url", "consent"))
            .respond_with(ResponseTemplate::new(302).insert_header(
                "location",
                "https://consent.google.com/ml?continue=https://www.google.com/goto",
            ))
            .mount(&server)
            .await;
        Mock::given(method("GET"))
            .and(path("/goto"))
            .and(query_param("url", "sorry"))
            .respond_with(ResponseTemplate::new(302).insert_header("location", "/sorry/index"))
            .mount(&server)
            .await;
        let rows = vec![
            row("http://www.google.com/goto?url=consent"),
            row("http://www.google.com/goto?url=sorry"),
        ];
        let urls: Vec<String> = resolve_google_links(&proxied_client(&server), rows)
            .await
            .into_iter()
            .filter_map(|r| r.url)
            .collect();
        assert_eq!(
            urls,
            vec![
                "http://www.google.com/goto?url=consent",
                "http://www.google.com/goto?url=sorry",
            ]
        );
    }
}

#[cfg(test)]
mod upstream_error_tests {
    //! Ported from upstream 1.5.0: camofox's own `error` message reaches the
    //! `Upstream` body, a non-JSON or empty body degrades to the step label.
    use super::*;
    use wiremock::matchers::{method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    /// Item 12: the github engine's errors name the host it actually calls.
    #[test]
    fn github_api_base_is_the_rest_api_not_the_browser() {
        let client =
            CamofoxSearchClient::new("http://camofox:9377", None, None, Duration::from_secs(5));
        assert_eq!(client.github_api_base(), "https://api.github.com");
        assert_eq!(client.base_url(), "http://camofox:9377");
    }

    /// A failed camofox call carries the server's own `error` message in the
    /// `Upstream` body (truncated), not a fixed label — the message is what
    /// tells a profile-version pin apart from a crashed browser.
    #[tokio::test]
    async fn create_tab_error_surfaces_camofox_message() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(500).set_body_json(json!({
                "error": "Profile for user \"crw-search\" was created with Camoufox 135.0.1-beta.24, but the current version is 152.0.4-beta.28"
            })))
            .mount(&server)
            .await;

        let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(5));
        let params = SearxngParams {
            q: "rust".to_string(),
            camofox_engines: vec![SearchEngine::Google],
            ..Default::default()
        };
        let err = client.fetch(&params).await.unwrap_err();
        match err {
            SearchError::Upstream { status, body } => {
                assert_eq!(status, 500);
                assert!(
                    body.starts_with("camofox: create tab failed: Profile for user"),
                    "{body}"
                );
                assert!(body.contains("152.0.4-beta.28"), "{body}");
            }
            other => panic!("expected Upstream, got {other:?}"),
        }
    }

    /// A non-JSON error body (a proxy's HTML page, a crash trace) stays out of
    /// the message — `Upstream.body` reaches API responses — so the error
    /// degrades to the bare step label.
    #[tokio::test]
    async fn navigate_error_with_non_json_body_keeps_label() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "t1" })))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/tabs/t1/navigate"))
            .respond_with(
                ResponseTemplate::new(502)
                    .set_body_string("<html><body>Bad Gateway at /internal/x</body></html>"),
            )
            .mount(&server)
            .await;

        let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(5));
        let params = SearxngParams {
            q: "rust".to_string(),
            camofox_engines: vec![SearchEngine::Bing],
            ..Default::default()
        };
        match client.fetch(&params).await.unwrap_err() {
            SearchError::Upstream { status, body } => {
                assert_eq!(status, 502);
                assert_eq!(body, "camofox: navigate failed");
            }
            other => panic!("expected Upstream, got {other:?}"),
        }
    }

    /// An empty error body degrades to the bare step label.
    #[tokio::test]
    async fn navigate_error_without_body_keeps_label() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "t1" })))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/tabs/t1/navigate"))
            .respond_with(ResponseTemplate::new(502))
            .mount(&server)
            .await;

        let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(5));
        let params = SearxngParams {
            q: "rust".to_string(),
            camofox_engines: vec![SearchEngine::Bing],
            ..Default::default()
        };
        let err = client.fetch(&params).await.unwrap_err();
        match err {
            SearchError::Upstream { status, body } => {
                assert_eq!(status, 502);
                assert_eq!(body, "camofox: navigate failed");
            }
            other => panic!("expected Upstream, got {other:?}"),
        }
    }

    /// Google's consent wall or `/sorry` rate-limit page is not a result: the
    /// row keeps its working redirect URL instead of being dropped as
    /// Google-internal.
    #[test]
    fn google_interstitials_are_recognised() {
        let parse = |u: &str| url::Url::parse(u).unwrap();
        assert!(is_google_interstitial(&parse(
            "https://consent.google.com/ml?continue=https://www.google.com/goto"
        )));
        assert!(is_google_interstitial(&parse(
            "https://www.google.com/sorry/index?continue=https://www.google.com/goto"
        )));
        assert!(!is_google_interstitial(&parse(
            "https://www.google.com/search?q=x"
        )));
        assert!(!is_google_interstitial(&parse("https://example.com/sorry")));
    }
}
