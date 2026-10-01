//! Behavioural tests for the Camofox-backed browser search client. A wiremock
//! server emulates the bounded REST flow (create warm tab → navigate →
//! observe `/tabs` → evaluate result rows).

use std::collections::HashMap;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use crw_core::types::SearchEngine;
use crw_search::SearxngParams;
use crw_search::camofox_search::CamofoxSearchClient;
use serde_json::json;
use wiremock::matchers::{body_partial_json, method, path, path_regex};
use wiremock::{Mock, MockServer, Request, Respond, ResponseTemplate};

#[derive(Clone)]
struct ConcurrencyProbe {
    active: Arc<AtomicUsize>,
    max_active: Arc<AtomicUsize>,
    calls: Arc<AtomicUsize>,
    delay: Duration,
    response: ProbeResponse,
}

#[derive(Clone, Copy)]
enum ProbeResponse {
    CreateTab,
}

impl ConcurrencyProbe {
    fn new(delay: Duration, response: ProbeResponse) -> Self {
        Self {
            active: Arc::new(AtomicUsize::new(0)),
            max_active: Arc::new(AtomicUsize::new(0)),
            calls: Arc::new(AtomicUsize::new(0)),
            delay,
            response,
        }
    }

    fn max_active(&self) -> usize {
        self.max_active.load(Ordering::SeqCst)
    }

    fn calls(&self) -> usize {
        self.calls.load(Ordering::SeqCst)
    }
}

impl Respond for ConcurrencyProbe {
    fn respond(&self, request: &Request) -> ResponseTemplate {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let active = self.active.fetch_add(1, Ordering::SeqCst) + 1;
        self.max_active.fetch_max(active, Ordering::SeqCst);

        let active_counter = Arc::clone(&self.active);
        let delay = self.delay;
        tokio::spawn(async move {
            tokio::time::sleep(delay).await;
            active_counter.fetch_sub(1, Ordering::SeqCst);
        });

        let response = match self.response {
            ProbeResponse::CreateTab => {
                let body: serde_json::Value = request.body_json().unwrap();
                let session_key = body["sessionKey"].as_str().unwrap();
                ResponseTemplate::new(200)
                    .set_body_json(json!({ "tabId": format!("tab-{session_key}") }))
            }
        };
        response.set_delay(delay)
    }
}

#[derive(Clone)]
struct BrowserHarness {
    urls: Arc<Mutex<HashMap<String, String>>>,
    rows: String,
    navigation_delay: Duration,
    active: Arc<AtomicUsize>,
    max_active: Arc<AtomicUsize>,
    navigation_calls: Arc<AtomicUsize>,
}

impl BrowserHarness {
    fn new(rows: serde_json::Value) -> Self {
        Self::with_delay(rows, Duration::ZERO)
    }

    fn with_delay(rows: serde_json::Value, navigation_delay: Duration) -> Self {
        Self {
            urls: Arc::new(Mutex::new(HashMap::new())),
            rows: serde_json::to_string(&rows).unwrap(),
            navigation_delay,
            active: Arc::new(AtomicUsize::new(0)),
            max_active: Arc::new(AtomicUsize::new(0)),
            navigation_calls: Arc::new(AtomicUsize::new(0)),
        }
    }

    fn max_active(&self) -> usize {
        self.max_active.load(Ordering::SeqCst)
    }

    fn navigation_calls(&self) -> usize {
        self.navigation_calls.load(Ordering::SeqCst)
    }
}

impl Respond for BrowserHarness {
    fn respond(&self, request: &Request) -> ResponseTemplate {
        let body: serde_json::Value = request.body_json().unwrap();
        if request.url.path().ends_with("/navigate") {
            let tab_id = request.url.path().split('/').nth(2).unwrap().to_string();
            let target = body["url"].as_str().unwrap().to_string();
            self.urls.lock().unwrap().insert(tab_id, target);
            self.navigation_calls.fetch_add(1, Ordering::SeqCst);
            let active = self.active.fetch_add(1, Ordering::SeqCst) + 1;
            self.max_active.fetch_max(active, Ordering::SeqCst);
            let active_counter = Arc::clone(&self.active);
            let delay = self.navigation_delay;
            tokio::spawn(async move {
                tokio::time::sleep(delay).await;
                active_counter.fetch_sub(1, Ordering::SeqCst);
            });
            ResponseTemplate::new(200)
                .set_delay(delay)
                .set_body_json(json!({ "ok": true, "result": true }))
        } else {
            ResponseTemplate::new(200).set_body_json(json!({
                "ok": true,
                "result": self.rows.clone(),
                "resultType": "string",
                "truncated": false
            }))
        }
    }
}

#[derive(Clone)]
struct TabsResponder(Arc<Mutex<HashMap<String, String>>>);

impl Respond for TabsResponder {
    fn respond(&self, _request: &Request) -> ResponseTemplate {
        let tabs: Vec<_> = self
            .0
            .lock()
            .unwrap()
            .iter()
            .map(|(tab_id, url)| json!({ "tabId": tab_id, "url": url }))
            .collect();
        ResponseTemplate::new(200).set_body_json(json!({ "ok": true, "tabs": tabs }))
    }
}

async fn mount_browser_harness(server: &MockServer, harness: &BrowserHarness) {
    Mock::given(method("POST"))
        .and(path_regex(r"^/tabs/[^/]+/navigate$"))
        .respond_with(harness.clone())
        .with_priority(10)
        .mount(server)
        .await;
    Mock::given(method("POST"))
        .and(path_regex(r"^/tabs/[^/]+/evaluate$"))
        .respond_with(harness.clone())
        .with_priority(10)
        .mount(server)
        .await;
    Mock::given(method("GET"))
        .and(path("/tabs"))
        .respond_with(TabsResponder(Arc::clone(&harness.urls)))
        .mount(server)
        .await;
}

fn params(q: &str) -> SearxngParams {
    params_with_engines(q, vec![SearchEngine::Google])
}

fn params_with_engines(q: &str, engines: Vec<SearchEngine>) -> SearxngParams {
    SearxngParams {
        q: q.to_string(),
        categories: None,
        language: None,
        time_range: None,
        engines: None,
        pageno: None,
        safesearch: None,
        camofox_engines: engines,
    }
}

/// Stand up a camofox-browser mock that returns two Google rows.
async fn mock_with_rows(rows: serde_json::Value) -> MockServer {
    let server = MockServer::start().await;
    let harness = BrowserHarness::new(rows);

    // The real camofox-browser requires both userId and sessionKey — guard it.
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .and(body_partial_json(json!({ "sessionKey": "search" })))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "ok": true, "tabId": "tab-1", "sessionKey": "search"
        })))
        .mount(&server)
        .await;
    mount_browser_harness(&server, &harness).await;
    Mock::given(method("DELETE"))
        .and(path("/tabs/tab-1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "ok": true })))
        .mount(&server)
        .await;
    server
}

#[tokio::test]
async fn fetch_maps_scraped_rows_to_searxng_response() {
    let server = mock_with_rows(json!([
        { "url": "https://a.example", "title": "Result A", "content": "snippet a" },
        { "url": "https://b.example", "title": "Result B", "content": "snippet b" },
    ]))
    .await;

    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(10));
    let resp = client
        .fetch(&params("rust async"))
        .await
        .expect("camofox search should succeed against the mock");

    assert_eq!(resp.query, "rust async");
    assert_eq!(resp.results.len(), 2);

    let first = &resp.results[0];
    assert_eq!(first.url.as_deref(), Some("https://a.example"));
    assert_eq!(first.title.as_deref(), Some("Result A"));
    assert_eq!(first.content.as_deref(), Some("snippet a"));
    assert_eq!(first.engine.as_deref(), Some("google"));

    // Rank must descend with SERP position so the existing score-sort keeps order.
    let s0 = resp.results[0].score.unwrap_or(0.0);
    let s1 = resp.results[1].score.unwrap_or(0.0);
    assert!(
        s0 > s1,
        "expected descending scores by position, got {s0} !> {s1}"
    );
}

/// Two engines run over the one warm tab; identical URLs returned by both must
/// dedupe to a single row that accumulates BOTH engine labels and positions and
/// sums their scores (cross-engine agreement ranks higher).
#[tokio::test]
async fn two_engines_merge_and_dedupe_by_url() {
    let server = mock_with_rows(json!([
        { "url": "https://a.example", "title": "Result A", "content": "snippet a" },
        { "url": "https://b.example", "title": "Result B", "content": "snippet b" },
    ]))
    .await;

    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(10));
    let resp = client
        .fetch(&params_with_engines(
            "rust async",
            vec![SearchEngine::Google, SearchEngine::Bing],
        ))
        .await
        .expect("multi-engine camofox search should succeed");

    // Same two URLs from both engines → 2 unique rows after dedupe.
    assert_eq!(resp.results.len(), 2);
    let first = &resp.results[0];
    assert_eq!(first.url.as_deref(), Some("https://a.example"));
    // Both engines recorded, positions accumulated, scores summed.
    assert_eq!(
        first.engines,
        vec!["google".to_string(), "bing".to_string()]
    );
    assert_eq!(first.positions.len(), 2);
    assert_eq!(first.score, Some(4.0)); // (2 from google) + (2 from bing)
}

#[tokio::test]
async fn fetch_tolerates_empty_results() {
    let server = mock_with_rows(json!([])).await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(1));
    let resp = client.fetch(&params("nothing here")).await.unwrap();
    assert_eq!(resp.results.len(), 0);
}

#[tokio::test]
async fn recovers_http_410_tab_destroyed_during_evaluation() {
    let server = MockServer::start().await;
    let harness = BrowserHarness::new(json!([
        { "url": "https://a.example", "title": "A", "content": "" },
    ]));
    for (tab, priority) in [("tab-1", 1), ("tab-2", 2)] {
        Mock::given(method("POST"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": tab })))
            .up_to_n_times(1)
            .with_priority(priority)
            .expect(1)
            .mount(&server)
            .await;
    }
    mount_browser_harness(&server, &harness).await;
    Mock::given(method("POST"))
        .and(path("/tabs/tab-1/evaluate"))
        .and(|request: &Request| {
            request.body_json::<serde_json::Value>().is_ok_and(|body| {
                body["expression"]
                    .as_str()
                    .is_some_and(|expression| expression.starts_with("JSON.stringify"))
            })
        })
        .respond_with(ResponseTemplate::new(410).set_body_json(json!({ "error": "tab_timeout" })))
        .with_priority(1)
        .expect(1)
        .mount(&server)
        .await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(3));
    let response = client
        .fetch(&params("recover destroyed tab"))
        .await
        .unwrap();
    assert_eq!(response.results.len(), 1);
    assert!(response.unresponsive_engines.is_empty());
    server.verify().await;
}

#[tokio::test]
async fn navigation_http_409_is_reported_without_recreation() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "tab-1" })))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/tabs/tab-1/navigate"))
        .respond_with(
            ResponseTemplate::new(409).set_body_json(json!({ "error": "navigation_in_progress" })),
        )
        .expect(1)
        .mount(&server)
        .await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(2));
    assert!(matches!(
        client.fetch(&params("busy tab")).await,
        Err(crw_search::SearchError::Upstream { status: 409, .. })
    ));
    server.verify().await;
}

#[tokio::test]
async fn repeated_http_410_is_retried_only_once() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "destroyed" })))
        .expect(2)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/tabs/destroyed/navigate"))
        .respond_with(ResponseTemplate::new(410).set_body_json(json!({ "error": "tab_timeout" })))
        .expect(2)
        .mount(&server)
        .await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(2));
    assert!(matches!(
        client.fetch(&params("retry bound")).await,
        Err(crw_search::SearchError::Upstream { status: 410, .. })
    ));
    server.verify().await;
}

#[tokio::test]
async fn google_challenge_page_does_not_return_result_rows() {
    let server = mock_with_rows(json!([
        { "url": "https://a.example", "title": "stale row", "content": "" },
    ]))
    .await;
    Mock::given(method("GET"))
        .and(path("/tabs"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "tabs": [{ "tabId": "tab-1", "url": "https://www.google.com/sorry/index" }]
        })))
        .with_priority(1)
        .mount(&server)
        .await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(2));
    let response = client.fetch(&params("challenge")).await.unwrap();
    assert!(response.results.is_empty());
    assert_eq!(response.unresponsive_engines.len(), 1);
    assert!(
        server
            .received_requests()
            .await
            .unwrap()
            .iter()
            .all(|request| { !request.url.path().ends_with("/evaluate") })
    );
}

#[tokio::test]
async fn timed_out_navigation_rotates_tab_before_bounded_cleanup() {
    let server = MockServer::start().await;
    let slow = BrowserHarness::with_delay(json!([]), Duration::from_millis(1200));
    let healthy = BrowserHarness::new(json!([
        { "url": "https://a.example", "title": "A", "content": "" },
    ]));
    for (tab, priority) in [("tab-1", 1), ("tab-2", 2)] {
        Mock::given(method("POST"))
            .and(path("/tabs"))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": tab })))
            .up_to_n_times(1)
            .with_priority(priority)
            .expect(1)
            .mount(&server)
            .await;
    }
    mount_browser_harness(&server, &healthy).await;
    for endpoint in ["navigate", "evaluate"] {
        Mock::given(method("POST"))
            .and(path(format!("/tabs/tab-1/{endpoint}")))
            .respond_with(slow.clone())
            .with_priority(1)
            .mount(&server)
            .await;
    }
    Mock::given(method("DELETE"))
        .and(path("/tabs/tab-1"))
        .respond_with(ResponseTemplate::new(200).set_delay(Duration::from_secs(3)))
        .expect(1)
        .mount(&server)
        .await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_millis(800));
    let start = Instant::now();
    assert!(matches!(
        client.fetch(&params("slow")).await,
        Err(crw_search::SearchError::Timeout)
    ));
    assert!(
        start.elapsed() < Duration::from_millis(1500),
        "cleanup exceeded its own bounded budget"
    );
    let first_requests = server.received_requests().await.unwrap();
    let replace = first_requests
        .iter()
        .rposition(|request| request.method == "POST" && request.url.path() == "/tabs")
        .expect("replacement tab is created");
    let close = first_requests
        .iter()
        .position(|request| request.method == "DELETE")
        .expect("the abandoned tab is closed best effort");
    assert!(
        replace < close,
        "prewarm replacement before closing avoids zero-tab teardown"
    );
    let response = client
        .fetch(&params("healthy"))
        .await
        .expect("next search uses replacement without stalled old navigation");
    assert_eq!(response.results.len(), 1);
    server.verify().await;
}

#[tokio::test]
async fn browser_search_uses_bounded_navigation_without_wait_route() {
    let server = mock_with_rows(json!([
        { "url": "https://a.example", "title": "A", "content": "" },
    ]))
    .await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(3));
    client.fetch(&params("bounded")).await.unwrap();

    let requests = server.received_requests().await.unwrap();
    let navigation = requests
        .iter()
        .find(|request| request.url.path().ends_with("/navigate"))
        .expect("navigate protocol is required; evaluation cannot schedule navigation reliably");
    let body: serde_json::Value = navigation.body_json().unwrap();
    assert_eq!(body["url"], "https://www.google.com/search?q=bounded");
    assert!(
        requests
            .iter()
            .all(|request| !request.url.path().ends_with("/wait"))
    );
}

#[tokio::test]
async fn rejects_http_200_navigation_response_with_ok_false() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "tab-1" })))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/tabs/tab-1/navigate"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "ok": false,
            "error": "window is null"
        })))
        .expect(2)
        .mount(&server)
        .await;

    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(2));
    let error = client.fetch(&params("rejected")).await.unwrap_err();
    assert!(error.to_string().contains("502"));
}

#[derive(Clone)]
struct StaleThenCurrentTabs {
    calls: Arc<AtomicUsize>,
    urls: Arc<Mutex<HashMap<String, String>>>,
}

impl Respond for StaleThenCurrentTabs {
    fn respond(&self, _request: &Request) -> ResponseTemplate {
        let call = self.calls.fetch_add(1, Ordering::SeqCst);
        let url = if call == 0 {
            "https://www.google.com/search?q=old-query".to_string()
        } else {
            self.urls.lock().unwrap()["tab-1"].clone()
        };
        ResponseTemplate::new(200).set_body_json(json!({
            "ok": true,
            "tabs": [{ "tabId": "tab-1", "url": url }]
        }))
    }
}

#[tokio::test]
async fn stale_warm_url_is_not_accepted_for_a_new_query() {
    let server = MockServer::start().await;
    let harness = BrowserHarness::new(json!([
        { "url": "https://a.example", "title": "A", "content": "" },
    ]));
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "tab-1" })))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path_regex(r"^/tabs/[^/]+/navigate$"))
        .respond_with(harness.clone())
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path_regex(r"^/tabs/[^/]+/evaluate$"))
        .respond_with(harness.clone())
        .mount(&server)
        .await;
    let calls = Arc::new(AtomicUsize::new(0));
    Mock::given(method("GET"))
        .and(path("/tabs"))
        .respond_with(StaleThenCurrentTabs {
            calls: Arc::clone(&calls),
            urls: Arc::clone(&harness.urls),
        })
        .mount(&server)
        .await;

    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(3));
    let response = client.fetch(&params("new query")).await.unwrap();
    assert_eq!(response.results.len(), 1);
    assert!(calls.load(Ordering::SeqCst) >= 2);
}

/// Two back-to-back searches must drive ONE warm tab — not create+delete a tab
/// per query — so the camofox context never hits zero tabs and never trips the
/// upstream eager-teardown race. We assert exactly one `POST /tabs` across two
/// `fetch` calls (and that we never DELETE the warm tab).
#[tokio::test]
async fn reuses_one_warm_tab_across_sequential_searches() {
    let server = MockServer::start().await;
    let harness = BrowserHarness::new(json!([
        { "url": "https://a.example", "title": "A", "content": "" },
    ]));

    // Exactly one tab is ever created, regardless of how many searches run.
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "tab-1" })))
        .expect(1)
        .mount(&server)
        .await;
    mount_browser_harness(&server, &harness).await;
    // The warm tab must never be deleted — fail loudly if it is.
    Mock::given(method("DELETE"))
        .and(path("/tabs/tab-1"))
        .respond_with(ResponseTemplate::new(200))
        .expect(0)
        .mount(&server)
        .await;

    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(10));
    client
        .fetch(&params("first"))
        .await
        .expect("first search ok");
    client
        .fetch(&params("second"))
        .await
        .expect("second search ok");
    assert_eq!(harness.navigation_calls(), 2);
    // wiremock verifies `.expect(..)` counts on drop.
}

/// If the warm tab went stale (context idle-evicted or camofox restarted), the
/// first navigation fails; the client must drop the dead tab, create a fresh one,
/// and retry the search — recovering transparently.
#[tokio::test]
async fn recreates_tab_and_retries_after_stale_failure() {
    let server = MockServer::start().await;
    let harness = BrowserHarness::new(json!([
        { "url": "https://a.example", "title": "A", "content": "" },
    ]));

    // First create hands out the (soon-stale) tab-1; the next create hands out
    // tab-2. up_to_n_times + priority makes tab-1 serve once, then tab-2.
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "tab-1" })))
        .up_to_n_times(1)
        .with_priority(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "tab-2" })))
        .with_priority(2)
        .mount(&server)
        .await;
    // tab-1 is dead: navigation returns 500 (the upstream
    // "window is null" shape).
    Mock::given(method("POST"))
        .and(path("/tabs/tab-1/navigate"))
        .respond_with(ResponseTemplate::new(500))
        .with_priority(1)
        .mount(&server)
        .await;
    mount_browser_harness(&server, &harness).await;

    let client = CamofoxSearchClient::new(server.uri(), None, None, Duration::from_secs(10));
    let resp = client
        .fetch(&params("recover"))
        .await
        .expect("search should recover by recreating the tab");
    assert_eq!(resp.results.len(), 1);
}

/// A two-worker client must let two independent queries overlap. Each worker
/// owns a distinct persistent browser identity and warm tab; the requests must
/// not serialize behind the legacy client's single global tab mutex.
#[tokio::test]
async fn pool_size_two_overlaps_searches_on_independent_warm_tabs() {
    let server = MockServer::start().await;
    let harness = BrowserHarness::with_delay(
        json!([
            { "url": "https://a.example", "title": "A", "content": "" },
        ]),
        Duration::from_millis(200),
    );

    // Keeps the test runnable against the pre-pool implementation so its RED
    // is behavioral (serialized elapsed time), not a mock/setup failure.
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .and(body_partial_json(json!({ "sessionKey": "search" })))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": "tab-0" })))
        .mount(&server)
        .await;

    for (user_id, session_key, tab_id) in [
        ("crw-search-0", "search-0", "tab-0"),
        ("crw-search-1", "search-1", "tab-1"),
    ] {
        Mock::given(method("POST"))
            .and(path("/tabs"))
            .and(body_partial_json(json!({
                "userId": user_id,
                "sessionKey": session_key,
            })))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({ "tabId": tab_id })))
            .expect(1)
            .mount(&server)
            .await;
    }
    mount_browser_harness(&server, &harness).await;

    let client = Arc::new(CamofoxSearchClient::new_with_pool_size(
        server.uri(),
        None,
        None,
        Duration::from_secs(10),
        2,
    ));
    let first_params = params("first");
    let second_params = params("second");
    let start = Instant::now();
    let (first, second) = tokio::join!(client.fetch(&first_params), client.fetch(&second_params),);
    let elapsed = start.elapsed();

    first.expect("first concurrent search should succeed");
    second.expect("second concurrent search should succeed");
    assert!(
        elapsed < Duration::from_millis(1700),
        "two navigations should overlap, but took {elapsed:?}"
    );

    let third_params = params("third");
    let fourth_params = params("fourth");
    let (third, fourth) = tokio::join!(client.fetch(&third_params), client.fetch(&fourth_params),);
    third.expect("third search should reuse a warm worker tab");
    fourth.expect("fourth search should reuse a warm worker tab");
}

/// Camofox cannot safely launch multiple persistent contexts at once. A cold
/// pool therefore serializes only tab creation; once all workers are warm,
/// their independent navigation requests must still overlap.
#[tokio::test]
async fn pool_size_four_serializes_cold_tabs_but_overlaps_warm_navigation() {
    let server = MockServer::start().await;
    let harness = BrowserHarness::with_delay(
        json!([
            { "url": "https://a.example", "title": "A", "content": "" },
        ]),
        Duration::from_millis(250),
    );
    let creates = ConcurrencyProbe::new(Duration::from_millis(100), ProbeResponse::CreateTab);

    Mock::given(method("POST"))
        .and(path("/tabs"))
        .respond_with(creates.clone())
        .expect(4)
        .mount(&server)
        .await;
    mount_browser_harness(&server, &harness).await;

    let client = Arc::new(CamofoxSearchClient::new_with_pool_size(
        server.uri(),
        None,
        None,
        Duration::from_secs(10),
        4,
    ));
    let cold = [
        params("cold-0"),
        params("cold-1"),
        params("cold-2"),
        params("cold-3"),
    ];
    let (a, b, c, d) = tokio::join!(
        client.fetch(&cold[0]),
        client.fetch(&cold[1]),
        client.fetch(&cold[2]),
        client.fetch(&cold[3]),
    );
    for result in [a, b, c, d] {
        result.expect("cold search should succeed");
    }

    assert_eq!(creates.calls(), 4);
    assert_eq!(
        creates.max_active(),
        1,
        "cold create-tab requests must be serialized"
    );

    let warm = [
        params("warm-0"),
        params("warm-1"),
        params("warm-2"),
        params("warm-3"),
    ];
    let start = Instant::now();
    let (a, b, c, d) = tokio::join!(
        client.fetch(&warm[0]),
        client.fetch(&warm[1]),
        client.fetch(&warm[2]),
        client.fetch(&warm[3]),
    );
    let elapsed = start.elapsed();
    for result in [a, b, c, d] {
        result.expect("warm search should succeed");
    }

    assert!(
        harness.max_active() >= 2,
        "independent warm navigations should overlap"
    );
    assert!(
        elapsed < Duration::from_millis(1700),
        "four 250ms warm navigations should overlap, but took {elapsed:?}"
    );
}
