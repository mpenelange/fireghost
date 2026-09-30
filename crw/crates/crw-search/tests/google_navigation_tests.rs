//! Google navigation contract against a mock Camofox REST endpoint.
//!
//! A scheduled navigation must establish a new document before accepting its
//! rows, even when a warm tab already has the requested query's URL. Other
//! engines retain the direct navigation protocol.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use crw_core::types::SearchEngine;
use crw_search::{CamofoxSearchClient, SearchError, SearxngParams};
use serde_json::{Value, json};
use wiremock::matchers::{body_partial_json, method, path};
use wiremock::{Mock, MockServer, Request, Respond, ResponseTemplate};

const QUERY: &str = "repeated query";
const TARGET: &str = "https://www.google.com/search?q=repeated+query";
const CHALLENGE: &str = "https://www.google.com/sorry/index?continue=private-query";
const OLD_ORIGIN: f64 = 100.0;
const FRESH_ORIGIN: f64 = 200.0;
const BUDGET: Duration = Duration::from_millis(750);

fn params(engine: SearchEngine) -> SearxngParams {
    SearxngParams {
        q: QUERY.into(),
        camofox_engines: vec![engine],
        ..Default::default()
    }
}

fn rows(title: &str) -> Value {
    json!([{
        "url": "https://example.org/result",
        "title": title,
        "content": "A result belonging to the observed document."
    }])
}

fn document(url: &str, time_origin: f64, title: &str) -> Value {
    json!({"url": url, "timeOrigin": time_origin, "rows": rows(title)})
}

#[derive(Clone)]
struct Browser {
    scheduler_reply: Value,
    harvest_reply: Option<Value>,
    documents: Arc<Vec<Value>>,
    current_url: Arc<Mutex<String>>,
    scheduled: Arc<AtomicUsize>,
    document_polls: Arc<AtomicUsize>,
    row_evaluations: Arc<AtomicUsize>,
    direct_navigations: Arc<AtomicUsize>,
}

impl Browser {
    fn new(documents: Vec<Value>) -> Self {
        assert!(!documents.is_empty());
        Self {
            scheduler_reply: json!({"ok": true, "result": OLD_ORIGIN, "truncated": false}),
            harvest_reply: None,
            current_url: Arc::new(Mutex::new(
                documents[0]["url"].as_str().unwrap_or(TARGET).into(),
            )),
            documents: Arc::new(documents),
            scheduled: Arc::new(AtomicUsize::new(0)),
            document_polls: Arc::new(AtomicUsize::new(0)),
            row_evaluations: Arc::new(AtomicUsize::new(0)),
            direct_navigations: Arc::new(AtomicUsize::new(0)),
        }
    }

    fn assert_google_scheduled(&self) {
        assert_eq!(self.scheduled.load(Ordering::SeqCst), 1);
        assert_eq!(self.direct_navigations.load(Ordering::SeqCst), 0);
        assert_eq!(
            self.row_evaluations.load(Ordering::SeqCst),
            0,
            "Google rows must come from the same evaluation as document identity"
        );
    }
}

impl Respond for Browser {
    fn respond(&self, request: &Request) -> ResponseTemplate {
        let body: Value = request.body_json().unwrap();
        assert_eq!(body["userId"], "crw-search");
        let expression = body["expression"].as_str().unwrap();
        if expression.contains("location.assign") {
            assert!(expression.contains("setTimeout"));
            assert!(expression.contains("performance.timeOrigin"));
            assert!(expression.contains(TARGET));
            let timeout = body["timeout"].as_u64().unwrap();
            assert!(timeout > 0 && timeout <= BUDGET.as_millis() as u64);
            self.scheduled.fetch_add(1, Ordering::SeqCst);
            ResponseTemplate::new(200).set_body_json(self.scheduler_reply.clone())
        } else if expression.contains("performance.timeOrigin") {
            assert!(expression.starts_with("JSON.stringify"));
            assert!(expression.contains("location.href"));
            let index = self.document_polls.fetch_add(1, Ordering::SeqCst);
            let snapshot = &self.documents[index.min(self.documents.len() - 1)];
            if let Some(url) = snapshot["url"].as_str() {
                *self.current_url.lock().unwrap() = url.into();
            }
            ResponseTemplate::new(200).set_body_json(self.harvest_reply.clone().unwrap_or_else(
                || {
                    json!({
                        "ok": true, "result": snapshot.to_string(), "truncated": false
                    })
                },
            ))
        } else {
            // Keep the legacy row-array protocol healthy: the RED failure must
            // expose navigation/freshness behavior, not an unrelated JSON error.
            self.row_evaluations.fetch_add(1, Ordering::SeqCst);
            ResponseTemplate::new(200).set_body_json(json!({
                "ok": true, "result": rows("Old document result").to_string(),
                "truncated": false
            }))
        }
    }
}

async fn mock_browser(browser: &Browser, direct_delay: Duration) -> MockServer {
    let server = MockServer::start().await;
    let creates = Arc::new(AtomicUsize::new(0));
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .and(body_partial_json(
            json!({"userId": "crw-search", "sessionKey": "search"}),
        ))
        .respond_with(move |_: &Request| {
            let tab = if creates.fetch_add(1, Ordering::SeqCst) == 0 {
                "navigation-tab"
            } else {
                "replacement-tab"
            };
            ResponseTemplate::new(200).set_body_json(json!({"ok": true, "tabId": tab}))
        })
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/tabs/navigation-tab/evaluate"))
        .respond_with(browser.clone())
        .mount(&server)
        .await;
    let navigation = browser.clone();
    Mock::given(method("POST"))
        .and(path("/tabs/navigation-tab/navigate"))
        .respond_with(move |request: &Request| {
            navigation.direct_navigations.fetch_add(1, Ordering::SeqCst);
            let body: Value = request.body_json().unwrap();
            assert_eq!(body["userId"], "crw-search");
            *navigation.current_url.lock().unwrap() = body["url"].as_str().unwrap().into();
            ResponseTemplate::new(200)
                .set_body_json(json!({"ok": true}))
                .set_delay(direct_delay)
        })
        .mount(&server)
        .await;
    let tabs = browser.clone();
    Mock::given(method("GET"))
        .and(path("/tabs"))
        .respond_with(move |_: &Request| {
            ResponseTemplate::new(200).set_body_json(json!({
                "ok": true,
                "tabs": [{"tabId": "navigation-tab", "url": tabs.current_url.lock().unwrap().clone()}]
            }))
        })
        .mount(&server)
        .await;
    Mock::given(method("DELETE"))
        .and(path("/tabs/navigation-tab"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"ok": true})))
        .mount(&server)
        .await;
    server
}

#[tokio::test]
async fn google_schedules_navigation_and_accepts_only_new_document_rows() {
    let browser = Browser::new(vec![
        document(TARGET, OLD_ORIGIN, "Old document result"),
        document(TARGET, FRESH_ORIGIN, "Fresh document result"),
    ]);
    // The direct route outlives the entire search budget. A successful result
    // must therefore use scheduling, not merely accept a faster direct reply.
    let server = mock_browser(&browser, Duration::from_secs(2)).await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
    let started = Instant::now();
    let result = client.fetch(&params(SearchEngine::Google)).await.unwrap();
    assert!(started.elapsed() < Duration::from_secs(1));
    assert_eq!(result.results.len(), 1);
    assert_eq!(
        result.results[0].title.as_deref(),
        Some("Fresh document result")
    );
    assert!(result.unresponsive_engines.is_empty());
    assert_eq!(browser.document_polls.load(Ordering::SeqCst), 2);
    browser.assert_google_scheduled();
}

#[tokio::test]
async fn same_query_old_document_never_establishes_empty_or_nonempty_success() {
    for old_rows in [rows("Old document result"), json!([])] {
        let browser = Browser::new(vec![json!({
            "url": TARGET, "timeOrigin": OLD_ORIGIN, "rows": old_rows
        })]);
        let server = mock_browser(&browser, Duration::ZERO).await;
        let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
        let started = Instant::now();
        assert!(matches!(
            client.fetch(&params(SearchEngine::Google)).await,
            Err(SearchError::Timeout)
        ));
        assert!(started.elapsed() < Duration::from_millis(1500));
        assert!(browser.document_polls.load(Ordering::SeqCst) > 0);
        browser.assert_google_scheduled();
    }
}

#[tokio::test]
async fn challenge_is_classified_only_after_a_new_document_is_observed() {
    let browser = Browser::new(vec![
        document(CHALLENGE, OLD_ORIGIN, "Old challenge rows"),
        document(CHALLENGE, FRESH_ORIGIN, "Fresh challenge rows"),
    ]);
    let server = mock_browser(&browser, Duration::ZERO).await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
    assert!(matches!(
        client.fetch(&params(SearchEngine::Google)).await,
        Err(SearchError::Blocked { engine }) if engine == "google"
    ));
    assert_eq!(browser.document_polls.load(Ordering::SeqCst), 2);
    browser.assert_google_scheduled();
}

#[tokio::test]
async fn old_challenge_document_does_not_block_a_fresh_valid_search() {
    let browser = Browser::new(vec![
        document(CHALLENGE, OLD_ORIGIN, "Old challenge rows"),
        document(TARGET, FRESH_ORIGIN, "Fresh document result"),
    ]);
    let server = mock_browser(&browser, Duration::ZERO).await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
    let result = client.fetch(&params(SearchEngine::Google)).await.unwrap();
    assert_eq!(
        result.results[0].title.as_deref(),
        Some("Fresh document result")
    );
    assert_eq!(browser.document_polls.load(Ordering::SeqCst), 2);
    browser.assert_google_scheduled();
}

#[tokio::test]
async fn malformed_or_truncated_navigation_identity_is_rejected_before_polling() {
    for reply in [
        json!({"ok": true}),
        json!({"ok": true, "result": null}),
        json!({"ok": true, "result": "100"}),
        json!({"ok": true, "result": false}),
        json!({"ok": true, "result": 0}),
        json!({"ok": true, "result": -1}),
        json!({"ok": true, "result": OLD_ORIGIN, "truncated": true}),
    ] {
        let mut browser = Browser::new(vec![document(TARGET, FRESH_ORIGIN, "Fresh result")]);
        browser.scheduler_reply = reply;
        let server = mock_browser(&browser, Duration::ZERO).await;
        let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
        assert!(matches!(
            client.fetch(&params(SearchEngine::Google)).await,
            Err(SearchError::InvalidResponse(_))
        ));
        assert_eq!(browser.document_polls.load(Ordering::SeqCst), 0);
        browser.assert_google_scheduled();
    }
}

#[tokio::test]
async fn wikipedia_keeps_direct_navigation_and_legacy_row_array_extraction() {
    let browser = Browser::new(vec![document(TARGET, OLD_ORIGIN, "Unused Google document")]);
    let server = mock_browser(&browser, Duration::ZERO).await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
    let result = client
        .fetch(&params(SearchEngine::Wikipedia))
        .await
        .unwrap();
    assert_eq!(result.results.len(), 1);
    assert_eq!(result.results[0].engine.as_deref(), Some("wikipedia"));
    assert_eq!(browser.direct_navigations.load(Ordering::SeqCst), 1);
    assert_eq!(browser.scheduled.load(Ordering::SeqCst), 0);
    assert_eq!(browser.document_polls.load(Ordering::SeqCst), 0);
    assert_eq!(browser.row_evaluations.load(Ordering::SeqCst), 1);
    assert_eq!(
        browser.current_url.lock().unwrap().as_str(),
        "https://en.wikipedia.org/wiki/Special:Search?search=repeated+query&fulltext=1"
    );
}

#[tokio::test]
async fn malformed_or_truncated_google_harvest_envelopes_are_not_empty_success() {
    let valid_frame = document(TARGET, FRESH_ORIGIN, "Fresh document result").to_string();
    for reply in [
        json!({"ok": true}),
        json!({"ok": true, "result": null}),
        json!({"ok": true, "result": {"url": TARGET, "timeOrigin": FRESH_ORIGIN, "rows": []}}),
        json!({"ok": true, "result": ""}),
        json!({"ok": true, "result": "not JSON"}),
        // An unframed legacy [] cannot establish Google document freshness.
        json!({"ok": true, "result": "[]"}),
        json!({"ok": true, "result": valid_frame, "truncated": true}),
        // JSON cannot express nonfinite origins. Reject an overflowing JSON
        // number rather than accidentally treating it as a fresh document.
        json!({"ok": true, "result": format!("{{\"url\":\"{TARGET}\",\"timeOrigin\":1e400,\"rows\":[]}}")}),
    ] {
        let mut browser = Browser::new(vec![document(TARGET, FRESH_ORIGIN, "Fresh result")]);
        browser.harvest_reply = Some(reply.clone());
        let server = mock_browser(&browser, Duration::ZERO).await;
        let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
        assert!(
            matches!(
                client.fetch(&params(SearchEngine::Google)).await,
                Err(SearchError::InvalidResponse(_))
            ),
            "malformed harvest envelope must be rejected: {reply}"
        );
        assert!(browser.document_polls.load(Ordering::SeqCst) > 0);
        browser.assert_google_scheduled();
    }
}

#[tokio::test]
async fn google_harvest_requires_url_positive_finite_origin_and_array_rows() {
    let base = document(TARGET, FRESH_ORIGIN, "Fresh document result");
    let mut invalid_frames = Vec::new();
    for field in ["url", "timeOrigin", "rows"] {
        let mut frame = base.clone();
        frame.as_object_mut().unwrap().remove(field);
        invalid_frames.push(frame);
    }
    for value in [json!(null), json!(123), json!(false), json!({}), json!([])] {
        let mut frame = base.clone();
        frame["url"] = value;
        invalid_frames.push(frame);
    }
    for value in [
        json!(null),
        json!("200"),
        json!("Infinity"),
        json!("NaN"),
        json!(false),
        json!(0),
        json!(-1),
    ] {
        let mut frame = base.clone();
        frame["timeOrigin"] = value;
        invalid_frames.push(frame);
    }
    for value in [
        json!(null),
        json!("[]"),
        json!(false),
        json!({}),
        json!(123),
    ] {
        let mut frame = base.clone();
        frame["rows"] = value;
        invalid_frames.push(frame);
    }
    for rows in [
        json!([{"url": "https://example.org/missing-title"}]),
        json!([null]),
    ] {
        let mut frame = base.clone();
        frame["rows"] = rows;
        invalid_frames.push(frame);
    }
    for frame in invalid_frames {
        let browser = Browser::new(vec![frame.clone()]);
        let server = mock_browser(&browser, Duration::ZERO).await;
        let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
        assert!(
            matches!(
                client.fetch(&params(SearchEngine::Google)).await,
                Err(SearchError::InvalidResponse(_))
            ),
            "malformed harvest frame must be rejected: {frame}"
        );
        assert!(browser.document_polls.load(Ordering::SeqCst) > 0);
        browser.assert_google_scheduled();
    }
}

#[tokio::test]
async fn fresh_google_empty_rows_remain_a_warned_success() {
    let browser = Browser::new(vec![json!({
        "url": TARGET, "timeOrigin": FRESH_ORIGIN, "rows": []
    })]);
    let server = mock_browser(&browser, Duration::ZERO).await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
    let result = client.fetch(&params(SearchEngine::Google)).await.unwrap();
    assert!(result.results.is_empty());
    assert_eq!(result.unresponsive_engines.len(), 1);
    assert_eq!(result.unresponsive_engines[0][0], "google");
    let notice = result.unresponsive_engines[0][1].as_str().unwrap();
    assert!(notice.contains("returned no results"));
    assert!(!notice.contains("blocked"));
    assert!(browser.document_polls.load(Ordering::SeqCst) > 0);
    browser.assert_google_scheduled();
}

#[tokio::test]
async fn fresh_wrong_query_rows_are_ignored_until_the_requested_query_is_observed() {
    let browser = Browser::new(vec![
        document(
            "https://www.google.com/search?q=other",
            FRESH_ORIGIN,
            "Wrong query result",
        ),
        document(TARGET, FRESH_ORIGIN, "Requested query result"),
    ]);
    let server = mock_browser(&browser, Duration::ZERO).await;
    let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
    let result = client.fetch(&params(SearchEngine::Google)).await.unwrap();
    assert_eq!(result.results.len(), 1);
    assert_eq!(
        result.results[0].title.as_deref(),
        Some("Requested query result")
    );
    assert!(browser.document_polls.load(Ordering::SeqCst) >= 2);
    browser.assert_google_scheduled();
}

#[tokio::test]
async fn fresh_wrong_query_never_establishes_empty_or_nonempty_success() {
    for wrong_rows in [rows("Wrong query result"), json!([])] {
        let browser = Browser::new(vec![json!({
            "url": "https://www.google.com/search?q=other",
            "timeOrigin": FRESH_ORIGIN, "rows": wrong_rows
        })]);
        let server = mock_browser(&browser, Duration::ZERO).await;
        let client = CamofoxSearchClient::new(server.uri(), None, None, BUDGET);
        assert!(matches!(
            client.fetch(&params(SearchEngine::Google)).await,
            Err(SearchError::Timeout)
        ));
        assert!(browser.document_polls.load(Ordering::SeqCst) > 0);
        browser.assert_google_scheduled();
    }
}
