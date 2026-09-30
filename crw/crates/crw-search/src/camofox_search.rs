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

use crate::client::{SearchError, SearxngResponse, SearxngResult};
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

/// JS evaluated in the Google SERP to extract result rows. Returns a JSON
/// *string* (via `JSON.stringify`) so the camofox `/evaluate` `result` field
/// comes back as a string we can parse. Selectors are intentionally broad and
/// kept in this one place — Google rewrites its SERP DOM periodically, so this
/// is the single spot to fix when extraction drifts.
const GOOGLE_SCRAPE_JS: &str = r#"JSON.stringify((function(){
    var rows=[],seen=new Set(),primaryHeadings=[],primary=new Set();
    function destination(anchor){
        var href=(anchor.getAttribute('href')||'').trim();
        if(!href)return null;
        try{
            var url=new URL(href,document.baseURI);
            var provider=url.hostname==='google.com'||url.hostname==='www.google.com';
            if(provider&&url.pathname==='/url'){
                var target=url.searchParams.get('q')||url.searchParams.get('url');
                if(!target)return null;
                url=new URL(target);
            }
            if(!/^https?:$/.test(url.protocol)||url.username||url.password)return null;
            provider=url.hostname==='google.com'||url.hostname==='www.google.com';
            if(provider&&(['/search','/url','/imgres','/aclk','/preferences','/advanced_search','/setprefs'].includes(url.pathname)||/^\/sorry(\/|$)/.test(url.pathname)))return null;
            return url.href;
        }catch(e){return null;}
    }
    document.querySelectorAll('div.g, div.MjjYud').forEach(function(container){
        var heading=container.querySelector('h3');
        if(heading&&!primary.has(heading)){
            primary.add(heading);
            primaryHeadings.push(heading);
        }
    });
    primaryHeadings.forEach(function(heading){
        var anchor=heading.closest('a[href]');
        if(!anchor){
            var ownLinks=heading.querySelectorAll('a[href]');
            if(ownLinks.length!==1)return;
            anchor=ownLinks[0];
        }
        var title=(heading.innerText||heading.textContent||'').trim();
        var url=destination(anchor);
        if(!title||!url||seen.has(url))return;
        var container=heading.closest('div.g, div.MjjYud');
        var snippet=null;
        while(container){
            var headings=Array.from(container.querySelectorAll('h3')).filter(function(h){return primary.has(h);});
            if(headings.length!==1||headings[0]!==heading)break;
            snippet=container.querySelector('.VwiC3b, [data-sncf], .st');
            if(snippet)break;
            container=container.parentElement?container.parentElement.closest('div.g, div.MjjYud'):null;
        }
        seen.add(url);
        rows.push({url:url,title:title,content:snippet?(snippet.innerText||snippet.textContent||'').trim():''});
    });
    return rows;
})())"#;

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
    result: Option<serde_json::Value>,
    #[serde(default)]
    truncated: bool,
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
        let status = response.status();
        if !status.is_success() {
            return Err(SearchError::Upstream {
                status: status.as_u16(),
                body: format!("camofox: {operation} failed"),
            });
        }
        let body = response.json::<CamofoxApiResponse>().await.map_err(|e| {
            SearchError::InvalidResponse(format!("camofox: bad {operation} response: {e}"))
        })?;
        if !body.ok {
            return Err(SearchError::Upstream {
                status: 502,
                body: format!("camofox: {operation} rejected"),
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
        let status = response.status();
        if !status.is_success() {
            return Err(SearchError::Upstream {
                status: status.as_u16(),
                body: "camofox: list tabs failed".to_string(),
            });
        }
        let body = response.json::<ListTabsResponse>().await.map_err(|e| {
            SearchError::InvalidResponse(format!("camofox: bad list tabs response: {e}"))
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
            return Err(SearchError::Upstream {
                status: create.status().as_u16(),
                body: "camofox: create tab failed".to_string(),
            });
        }
        let id = create
            .json::<CreateTabResponse>()
            .await
            .map_err(|e| SearchError::InvalidResponse(format!("camofox: bad /tabs response: {e}")))?
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
                return Err(SearchError::Timeout);
            }
            if self
                .tab_matches_search(worker, tab_id, engine, &params.q)
                .await?
            {
                break;
            }
            let remaining = deadline.saturating_duration_since(Instant::now());
            tokio::time::sleep(URL_POLL_INTERVAL.min(remaining)).await;
        }

        // Do not call `/wait`: Camofox 2.4.6 can keep that route alive beyond
        // the caller's budget. Extraction polling stays on non-blocking API
        // calls. Only a successfully decoded empty row array can establish an
        // empty result; challenges and malformed replies remain explicit errors.
        let remaining = deadline.saturating_duration_since(Instant::now());
        tokio::time::sleep(RENDER_SETTLE.min(remaining)).await;
        let mut observed_empty = false;
        while Instant::now() < deadline {
            // The tab may redirect during settling or between evaluations.
            // A previously observed empty array cannot establish this query's
            // outcome after the browser has moved to a different destination.
            if !self
                .tab_matches_search(worker, tab_id, engine, &params.q)
                .await?
            {
                observed_empty = false;
                let remaining = deadline.saturating_duration_since(Instant::now());
                tokio::time::sleep(URL_POLL_INTERVAL.min(remaining)).await;
                continue;
            }
            // Evaluation failures propagate to fetch's existing typed error
            // handling, including its bounded stale-tab retry.
            let rows = self.evaluate_rows(worker, tab_id, engine).await?;
            if Instant::now() >= deadline {
                return Err(SearchError::Timeout);
            }
            // A navigation can finish while evaluate is in flight. Neither
            // rows nor an empty array are accepted without rechecking the
            // current destination after the reply has been decoded.
            let matches = self
                .tab_matches_search(worker, tab_id, engine, &params.q)
                .await?;
            if Instant::now() >= deadline {
                return Err(SearchError::Timeout);
            }
            if !matches {
                observed_empty = false;
            } else if !rows.is_empty() {
                return Ok(rows);
            } else {
                observed_empty = true;
            }
            let remaining = deadline.saturating_duration_since(Instant::now());
            tokio::time::sleep(URL_POLL_INTERVAL.min(remaining)).await;
        }
        if observed_empty {
            Ok(Vec::new())
        } else {
            Err(SearchError::Timeout)
        }
    }

    async fn tab_matches_search(
        &self,
        worker: &CamofoxSearchWorker,
        tab_id: &str,
        engine: SearchEngine,
        query: &str,
    ) -> Result<bool, SearchError> {
        let url =
            self.current_tab_url(worker, tab_id)
                .await?
                .ok_or_else(|| SearchError::Upstream {
                    status: 404,
                    body: "camofox: warm search tab disappeared".to_string(),
                })?;
        if is_challenge_url(engine, &url) {
            return Err(SearchError::Blocked {
                engine: engine.label().to_string(),
            });
        }
        Ok(url_matches_search(engine, &url, query))
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
        if body.truncated {
            return Err(SearchError::InvalidResponse(
                "camofox: truncated search evaluation".into(),
            ));
        }
        let raw = match body.result {
            Some(serde_json::Value::String(value)) => value,
            _ => {
                return Err(SearchError::InvalidResponse(
                    "camofox: search evaluation must return a JSON row string".into(),
                ));
            }
        };

        let rows: Vec<ScrapedRow> = serde_json::from_str(&raw)
            .map_err(|_| SearchError::InvalidResponse("camofox: invalid search row JSON".into()))?;

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
            SearchError::InvalidResponse(format!("github: bad search response: {e}"))
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
fn engine_failure_reason(e: &SearchError) -> String {
    match e {
        SearchError::Timeout => "timed out".to_string(),
        SearchError::Upstream { status, .. } => format!("upstream error (HTTP {status})"),
        SearchError::InvalidResponse(_) => "unreadable response".to_string(),
        SearchError::Blocked { .. } => "blocked by a challenge or consent page".to_string(),
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
                && (url.path() == "/sorry"
                    || url.path().starts_with("/sorry/")
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
        SearchError::Timeout | SearchError::InvalidResponse(_) | SearchError::Blocked { .. } => {
            false
        }
    }
}

#[cfg(test)]
mod extractor_tests {
    use super::*;
    use crw_core::types::SearchEngine;

    #[test]
    fn google_challenge_paths_include_bare_sorry_without_matching_other_routes() {
        for url in [
            "https://www.google.com/sorry",
            "https://www.google.com/sorry/",
            "https://www.google.com/sorry/index",
        ] {
            assert!(is_challenge_url(SearchEngine::Google, url), "{url}");
        }
        for url in [
            "https://www.google.com/sorry-about-that",
            "https://www.google.com/search?q=sorry",
            "https://google.com.example.org/sorry",
        ] {
            assert!(!is_challenge_url(SearchEngine::Google, url), "{url}");
        }
    }

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
mod diagnostic_tests {
    use super::*;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    use wiremock::matchers::{method, path};
    use wiremock::{Mock, MockServer, Request, ResponseTemplate};

    fn google_params() -> SearxngParams {
        SearxngParams {
            q: "diagnostic query".into(),
            camofox_engines: vec![SearchEngine::Google],
            ..Default::default()
        }
    }

    async fn warm_client(server: &MockServer, observed_url: &str) -> CamofoxSearchClient {
        warm_client_with_navigation_count(server, observed_url, 1).await
    }

    async fn warm_client_with_navigation_count(
        server: &MockServer,
        observed_url: &str,
        navigation_count: u64,
    ) -> CamofoxSearchClient {
        let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_millis(750));
        *client.workers[0].tab.lock().await = Some("diagnostic-tab".into());
        Mock::given(method("POST"))
            .and(path("/tabs/diagnostic-tab/navigate"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({"ok": true})))
            .expect(navigation_count)
            .mount(server)
            .await;
        Mock::given(method("GET"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "tabs": [{"tabId": "diagnostic-tab", "url": observed_url}]
            })))
            .mount(server)
            .await;
        // Deadline failures retain the existing bounded replacement/cleanup
        // path. Respond promptly so its lifecycle work cannot obscure errors.
        Mock::given(method("POST"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "tabId": "replacement-tab"
            })))
            .mount(server)
            .await;
        Mock::given(method("DELETE"))
            .and(path("/tabs/diagnostic-tab"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({"ok": true})))
            .mount(server)
            .await;
        client
    }

    async fn assert_no_row_evaluation(server: &MockServer) {
        assert!(
            server
                .received_requests()
                .await
                .unwrap()
                .iter()
                .all(|request| { !request.url.path().ends_with("/evaluate") })
        );
    }

    async fn client_with_url_changed_by_evaluation(
        server: &MockServer,
        next_url: &str,
    ) -> (CamofoxSearchClient, Arc<AtomicBool>) {
        let target = search_target_url(SearchEngine::Google, &google_params().q);
        let client = warm_client(server, &target).await;
        let changed = Arc::new(AtomicBool::new(false));
        let observed = Arc::clone(&changed);
        let next_url = next_url.to_string();
        Mock::given(method("GET"))
            .and(path("/tabs"))
            .respond_with(move |_: &Request| {
                let url = if observed.load(Ordering::SeqCst) {
                    &next_url
                } else {
                    &target
                };
                ResponseTemplate::new(200).set_body_json(json!({
                    "tabs": [{"tabId": "diagnostic-tab", "url": url}]
                }))
            })
            .with_priority(1)
            .mount(server)
            .await;
        (client, changed)
    }

    async fn mount_nonempty_evaluation_changing_url(server: &MockServer, changed: Arc<AtomicBool>) {
        Mock::given(method("POST"))
            .and(path("/tabs/diagnostic-tab/evaluate"))
            .respond_with(move |_: &Request| {
                changed.store(true, Ordering::SeqCst);
                ResponseTemplate::new(200).set_body_json(json!({
                    "ok": true,
                    "result": "[{\"url\":\"https://example.com/\",\"title\":\"Rows from a moving tab\"}]",
                    "truncated": false
                }))
            })
            .expect(1)
            .mount(server)
            .await;
    }

    #[tokio::test]
    async fn late_navigation_nonempty_evaluation_changes_query() {
        let server = MockServer::start().await;
        let (client, changed) = client_with_url_changed_by_evaluation(
            &server,
            "https://www.google.com/search?q=previous",
        )
        .await;
        mount_nonempty_evaluation_changing_url(&server, changed).await;
        assert!(
            matches!(
                client.fetch(&google_params()).await,
                Err(SearchError::Timeout)
            ),
            "rows from a tab that changed query must not be accepted"
        );
        server.verify().await;
    }

    #[tokio::test]
    async fn late_navigation_nonempty_evaluation_redirects_to_challenge() {
        let server = MockServer::start().await;
        let (client, changed) =
            client_with_url_changed_by_evaluation(&server, "https://www.google.com/sorry/index")
                .await;
        mount_nonempty_evaluation_changing_url(&server, changed).await;
        assert!(
            matches!(
                client.fetch(&google_params()).await,
                Err(SearchError::Blocked { engine }) if engine == "google"
            ),
            "a challenge reached during evaluation must not return its rows"
        );
        server.verify().await;
    }

    #[tokio::test]
    async fn late_navigation_to_challenge_during_settle_is_blocked() {
        let server = MockServer::start().await;
        let client = warm_client(&server, "https://www.google.com/sorry/index").await;
        let target = search_target_url(SearchEngine::Google, &google_params().q);
        // Only the first observation matches. The redirect becomes visible
        // after that match, before extraction, without a timing-based fixture.
        Mock::given(method("GET"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "tabs": [{"tabId": "diagnostic-tab", "url": target}]
            })))
            .with_priority(1)
            .up_to_n_times(1)
            .expect(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/tabs/diagnostic-tab/evaluate"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "ok": true,
                "result": "[{\"url\":\"https://example.com/\",\"title\":\"Stale row\"}]"
            })))
            .mount(&server)
            .await;
        assert!(matches!(
            client.fetch(&google_params()).await,
            Err(SearchError::Blocked { engine }) if engine == "google"
        ));
        assert_no_row_evaluation(&server).await;
        server.verify().await;
    }

    #[tokio::test]
    async fn late_navigation_to_challenge_after_empty_evaluation_is_blocked() {
        let server = MockServer::start().await;
        let (client, changed) =
            client_with_url_changed_by_evaluation(&server, "https://www.google.com/sorry/index")
                .await;
        Mock::given(method("POST"))
            .and(path("/tabs/diagnostic-tab/evaluate"))
            .respond_with(move |_: &Request| {
                changed.store(true, Ordering::SeqCst);
                ResponseTemplate::new(200).set_body_json(json!({
                    "ok": true, "result": "[]", "truncated": false
                }))
            })
            .expect(1)
            .mount(&server)
            .await;
        assert!(matches!(
            client.fetch(&google_params()).await,
            Err(SearchError::Blocked { engine }) if engine == "google"
        ));
        server.verify().await;
    }

    #[tokio::test]
    async fn late_navigation_changes_query_without_returning_stale_rows() {
        let server = MockServer::start().await;
        let (client, changed) = client_with_url_changed_by_evaluation(
            &server,
            "https://www.google.com/search?q=previous",
        )
        .await;
        let evaluations = Arc::new(AtomicUsize::new(0));
        let count = Arc::clone(&evaluations);
        Mock::given(method("POST"))
            .and(path("/tabs/diagnostic-tab/evaluate"))
            .respond_with(move |_: &Request| {
                let first = count.fetch_add(1, Ordering::SeqCst) == 0;
                changed.store(true, Ordering::SeqCst);
                let rows = if first {
                    "[]"
                } else {
                    "[{\"url\":\"https://example.com/\",\"title\":\"Previous query row\"}]"
                };
                ResponseTemplate::new(200).set_body_json(json!({
                    "ok": true, "result": rows, "truncated": false
                }))
            })
            .expect(1)
            .mount(&server)
            .await;
        assert!(matches!(
            client.fetch(&google_params()).await,
            Err(SearchError::Timeout)
        ));
        assert_eq!(evaluations.load(Ordering::SeqCst), 1);
        server.verify().await;
    }

    #[tokio::test]
    async fn recognized_google_challenges_are_blocked_not_empty_success() {
        for observed in [
            "https://www.google.com/sorry/index?continue=search",
            "https://consent.google.com/m?continue=search",
        ] {
            let server = MockServer::start().await;
            let client = warm_client(&server, observed).await;
            assert!(
                matches!(
                    client.fetch(&google_params()).await,
                    Err(SearchError::Blocked { engine }) if engine == "google"
                ),
                "a known challenge cannot establish a successful empty search"
            );
            assert_no_row_evaluation(&server).await;
            server.verify().await;
        }
    }

    #[tokio::test]
    async fn persistent_previous_query_url_times_out_without_scraping_stale_rows() {
        let server = MockServer::start().await;
        let client = warm_client(&server, "https://www.google.com/search?q=previous").await;
        assert!(
            matches!(
                client.fetch(&google_params()).await,
                Err(SearchError::Timeout)
            ),
            "failure to reach this query must not become an empty success"
        );
        assert_no_row_evaluation(&server).await;
        server.verify().await;
    }

    #[tokio::test]
    async fn malformed_evaluation_is_invalid_response_not_empty_success() {
        for reply in [
            json!({"ok":true,"result":"not row JSON","truncated":false}),
            json!({"ok":true,"result":{"unexpected":"object"},"truncated":false}),
            json!({"ok":true,"result":null,"truncated":false}),
            json!({"ok":true,"truncated":false}),
            json!({"ok":true,"result":"","truncated":false}),
        ] {
            let server = MockServer::start().await;
            let target = search_target_url(SearchEngine::Google, &google_params().q);
            let client = warm_client(&server, &target).await;
            Mock::given(method("POST"))
                .and(path("/tabs/diagnostic-tab/evaluate"))
                .respond_with(ResponseTemplate::new(200).set_body_json(reply))
                .mount(&server)
                .await;
            assert!(
                matches!(
                    client.fetch(&google_params()).await,
                    Err(SearchError::InvalidResponse(_))
                ),
                "malformed extraction cannot establish a successful empty search"
            );
            server.verify().await;
        }
    }

    #[tokio::test]
    async fn truncated_evaluation_is_rejected_even_when_result_parses() {
        let server = MockServer::start().await;
        let target = search_target_url(SearchEngine::Google, &google_params().q);
        let client = warm_client(&server, &target).await;
        Mock::given(method("POST"))
            .and(path("/tabs/diagnostic-tab/evaluate"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "ok": true,
                "result": "[{\"url\":\"https://example.com/\",\"title\":\"Incomplete result\",\"content\":\"\"}]",
                "truncated": true
            })))
            .mount(&server)
            .await;
        assert!(
            matches!(
                client.fetch(&google_params()).await,
                Err(SearchError::InvalidResponse(_))
            ),
            "truncated data cannot be accepted as complete result rows"
        );
        server.verify().await;
    }

    #[tokio::test]
    async fn valid_empty_row_array_remains_a_warned_success() {
        let server = MockServer::start().await;
        let target = search_target_url(SearchEngine::Google, &google_params().q);
        let client = warm_client(&server, &target).await;
        Mock::given(method("POST"))
            .and(path("/tabs/diagnostic-tab/evaluate"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "ok": true, "result": "[]", "truncated": false
            })))
            .mount(&server)
            .await;
        let result = client.fetch(&google_params()).await.unwrap();
        assert!(result.results.is_empty());
        assert_eq!(result.unresponsive_engines.len(), 1);
        assert_eq!(result.unresponsive_engines[0][0], "google");
        server.verify().await;
    }

    #[tokio::test]
    async fn blocked_google_preserves_wikipedia_results_with_a_warning() {
        let server = MockServer::start().await;
        let mut params = google_params();
        params.camofox_engines.push(SearchEngine::Wikipedia);
        let target = search_target_url(SearchEngine::Wikipedia, &params.q);
        let client = warm_client_with_navigation_count(&server, &target, 2).await;
        Mock::given(method("GET"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "tabs": [{
                    "tabId": "diagnostic-tab",
                    "url": "https://www.google.com/sorry/index?continue=search"
                }]
            })))
            .with_priority(1)
            .up_to_n_times(1)
            .expect(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/tabs/diagnostic-tab/evaluate"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "ok": true,
                "result": "[{\"url\":\"https://en.wikipedia.org/wiki/Python_(programming_language)\",\"title\":\"Python\",\"content\":\"Programming language\"}]",
                "truncated": false
            })))
            .expect(1)
            .mount(&server)
            .await;
        let result = client.fetch(&params).await.unwrap();
        assert_eq!(result.results.len(), 1);
        assert_eq!(result.results[0].engine.as_deref(), Some("wikipedia"));
        assert_eq!(result.unresponsive_engines.len(), 1);
        assert_eq!(result.unresponsive_engines[0][0], "google");
        assert!(
            result.unresponsive_engines[0][1]
                .as_str()
                .unwrap()
                .contains("blocked")
        );
        server.verify().await;
    }
}
