#![cfg(feature = "camofox")]

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::routing::{delete, get, post};
use axum::{Json, Router};
use crw_core::Deadline;
use crw_renderer::pipeline::{BrowserPipelineClient, BrowserPipelineRequest};
use serde_json::{Value, json};

#[derive(Clone)]
struct Mock {
    calls: Arc<Mutex<Vec<(String, Value)>>>,
    evaluations: Arc<Mutex<Vec<Value>>>,
    ready_at: Arc<Mutex<Option<std::time::Instant>>>,
    active: Arc<AtomicUsize>,
    peak: Arc<AtomicUsize>,
    version: Arc<Mutex<String>>,
    tabs: Arc<Mutex<Vec<Value>>>,
    create_failures: Arc<AtomicUsize>,
    reset_ack: Arc<Mutex<Value>>,
}

async fn create(State(mock): State<Mock>, Json(body): Json<Value>) -> (StatusCode, Json<Value>) {
    mock.calls
        .lock()
        .unwrap()
        .push(("create".into(), body.clone()));
    // Camofox's URL guard rejects explicit about:blank. Omitting the URL
    // creates the blank tab internally, before the separately guarded navigate.
    if body.get("url").and_then(Value::as_str) == Some("about:blank") {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({"error":"Only http/https URLs are allowed"})),
        );
    }
    if mock
        .create_failures
        .fetch_update(Ordering::SeqCst, Ordering::SeqCst, |remaining| {
            remaining.checked_sub(1)
        })
        .is_ok()
    {
        return (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(json!({"error":"Browser.newPage delayedStartupPromise window is null"})),
        );
    }
    let active = mock.active.fetch_add(1, Ordering::SeqCst) + 1;
    mock.peak.fetch_max(active, Ordering::SeqCst);
    (StatusCode::OK, Json(json!({"tabId":"pipeline-tab"})))
}
async fn navigate(State(mock): State<Mock>, Json(body): Json<Value>) -> Json<Value> {
    mock.calls.lock().unwrap().push(("navigate".into(), body));
    Json(json!({"ok":true}))
}
async fn wait(State(mock): State<Mock>, Json(body): Json<Value>) -> Json<Value> {
    mock.calls.lock().unwrap().push(("wait".into(), body));
    Json(json!({"ok":true,"ready":true}))
}
async fn evaluate(State(mock): State<Mock>, Json(body): Json<Value>) -> Json<Value> {
    mock.calls.lock().unwrap().push(("evaluate".into(), body));
    let response = {
        let mut responses = mock.evaluations.lock().unwrap();
        if responses.len() > 1 {
            responses.remove(0)
        } else {
            responses[0].clone()
        }
    };
    if let Some(delay) = response.get("evaluationDelayMs").and_then(Value::as_u64) {
        tokio::time::sleep(Duration::from_millis(delay)).await;
    }
    if let Some(delay) = response.get("scheduleMs").and_then(Value::as_u64) {
        *mock.ready_at.lock().unwrap() =
            Some(std::time::Instant::now() + Duration::from_millis(delay));
    }
    if let Some(before) = response.get("beforeReady") {
        let ready = mock
            .ready_at
            .lock()
            .unwrap()
            .is_some_and(|time| std::time::Instant::now() >= time);
        return Json(if ready {
            response["afterReady"].clone()
        } else {
            before.clone()
        });
    }
    Json(response)
}
async fn close(State(mock): State<Mock>, Json(body): Json<Value>) -> Json<Value> {
    mock.calls.lock().unwrap().push(("close".into(), body));
    let _ = mock
        .active
        .fetch_update(Ordering::SeqCst, Ordering::SeqCst, |active| {
            active.checked_sub(1)
        });
    Json(json!({"ok":true}))
}
async fn health(State(mock): State<Mock>) -> Json<Value> {
    Json(json!({"version":mock.version.lock().unwrap().clone()}))
}
async fn tabs(State(mock): State<Mock>) -> Json<Value> {
    Json(json!({"tabs":mock.tabs.lock().unwrap().clone()}))
}
async fn reset_session(State(mock): State<Mock>, Path(user): Path<String>) -> Json<Value> {
    mock.calls
        .lock()
        .unwrap()
        .push(("reset".into(), json!({"userId":user})));
    Json(mock.reset_ack.lock().unwrap().clone())
}
async fn mock(evaluations: Vec<Value>) -> (BrowserPipelineClient, Mock) {
    let mock = Mock {
        calls: Arc::default(),
        evaluations: Arc::new(Mutex::new(evaluations)),
        ready_at: Arc::default(),
        active: Arc::default(),
        peak: Arc::default(),
        version: Arc::new(Mutex::new("2.4.8".into())),
        tabs: Arc::default(),
        create_failures: Arc::default(),
        reset_ack: Arc::new(Mutex::new(json!({"ok":true}))),
    };
    let app = Router::new()
        .route("/health", get(health))
        .route("/tabs", post(create).get(tabs))
        .route("/tabs/pipeline-tab/navigate", post(navigate))
        .route("/tabs/pipeline-tab/wait", post(wait))
        .route("/tabs/pipeline-tab/evaluate", post(evaluate))
        .route("/tabs/pipeline-tab", delete(close))
        .route("/sessions/{userId}", delete(reset_session))
        .with_state(mock.clone());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (
        BrowserPipelineClient::new(&format!("http://{address}"), None),
        mock,
    )
}
fn request(value: Value) -> BrowserPipelineRequest {
    serde_json::from_value(value).unwrap()
}
fn envelope(value: Value) -> Value {
    json!({"ok":true,"result":value.to_string(),"truncated":false})
}
fn article() -> Value {
    json!({"url":"https://example.com/article","title":"An article","contentHtml":"<article><h1>An article</h1><p>Complete small page.</p></article>","items":[],"complete":true,"overflow":false,"outstandingControls":0,"advertisedCount":null,"error":null,"postId":null,"commentsLoaded":false})
}
fn deadline() -> Deadline {
    Deadline::now_plus(Duration::from_secs(2))
}

#[test]
fn validates_budgets_and_reddit_identity_before_browser_work() {
    let default = request(json!({"url":"https://example.com/article"}));
    assert_eq!(default.max_bytes, 196608);
    assert!(default.validate().is_ok());
    for value in [
        json!({"url":"file:///etc/passwd"}),
        json!({"url":"https://u:p@example.com"}),
        json!({"url":"https://example.com","maxRounds":101}),
        json!({"url":"https://example.com","maxItems":1001}),
        json!({"url":"https://example.com","maxBytes":262145}),
        json!({"url":"https://example.com","timeout":60001}),
        json!({"url":"https://www.reddit.com/r/selfhosted/","profile":"redditThread"}),
    ] {
        assert!(request(value).validate().is_err());
    }
}

#[tokio::test]
async fn article_uses_blank_tab_then_bounded_snapshot_and_always_closes() {
    let (client, mock) = mock(vec![envelope(article())]).await;
    let data = client
        .fetch(
            &request(json!({"url":"https://example.com/article"})),
            deadline(),
        )
        .await
        .unwrap();
    assert!(data.markdown.contains("Complete small page."));
    assert!(data.metadata.complete);
    assert_eq!(data.metadata.pipeline, "browser-v1");
    let calls = mock.calls.lock().unwrap();
    assert!(calls.first().unwrap().1.get("url").is_none());
    assert_eq!(calls[1].0, "navigate");
    assert_eq!(calls.last().unwrap().0, "close");
    assert!(
        !calls
            .iter()
            .any(|(_, body)| body["expression"] == "document.documentElement.outerHTML")
    );
}

#[tokio::test]
async fn truncated_or_malformed_evaluation_fails_and_closes() {
    for response in [
        json!({"result":"[Truncated]","truncated":true}),
        json!({"result":"not JSON"}),
        envelope(json!({"url":"https://example.com","contentHtml":""})),
    ] {
        let (client, mock) = mock(vec![response]).await;
        assert!(
            client
                .fetch(
                    &request(json!({"url":"https://example.com/article"})),
                    deadline()
                )
                .await
                .is_err()
        );
        assert_eq!(mock.calls.lock().unwrap().last().unwrap().0, "close");
    }
}

fn thread(items: Value, controls: usize) -> Value {
    json!({"url":"https://www.reddit.com/r/selfhosted/comments/abc123/a_thread/","title":"A thread","contentHtml":"<p>The original post.</p>","items":items,"complete":false,"overflow":false,"outstandingControls":controls,"advertisedCount":8,"error":null,"postId":"t3_abc123","commentsLoaded":true})
}
fn comment(id: &str, parent: &str, body: &str) -> Value {
    json!({"id":id,"parentId":parent,"permalink":null,"author":"reader","depth":1,"bodyHtml":format!("<p>{body}</p>")})
}
fn reddit_request(extra: Value) -> BrowserPipelineRequest {
    let mut body = json!({"url":"https://www.reddit.com/r/selfhosted/comments/abc123/a_thread/","profile":"redditThread"});
    body.as_object_mut()
        .unwrap()
        .extend(extra.as_object().unwrap().clone());
    request(body)
}

#[tokio::test]
async fn reddit_accumulates_by_id_preserves_parents_and_reports_partial() {
    let first = comment("t1_first", "t3_abc123", "First comment.");
    let second = comment("t1_second", "t1_first", "Reply survives rerender.");
    let (client, mock) = mock(vec![
        envelope(thread(json!([first.clone()]), 1)),
        envelope(json!({"clicked":1,"scrolled":false,"postId":"t3_abc123","error":null})),
        envelope(thread(json!([first, second.clone()]), 1)),
        envelope(json!({"clicked":1,"scrolled":false,"postId":"t3_abc123","error":null})),
        envelope(thread(json!([second]), 0)),
    ])
    .await;
    let data = client
        .fetch(&reddit_request(json!({})), deadline())
        .await
        .unwrap();
    assert_eq!(data.metadata.items_collected, 2);
    assert_eq!(data.metadata.reported_total, Some(8));
    assert!(!data.metadata.complete);
    assert_eq!(data.metadata.stop_reason, "noControls");
    let items = &data.json.as_ref().unwrap()["comments"];
    assert_eq!(items[0]["id"], "t1_first");
    assert_eq!(items[1]["parentId"], "t1_first");
    assert_eq!(data.markdown.matches("First comment.").count(), 1);
    assert_eq!(mock.calls.lock().unwrap().last().unwrap().0, "close");
}

#[tokio::test]
async fn reddit_limits_keep_existing_items_and_never_claim_complete() {
    for budget in [json!({"maxItems":1}), json!({"maxRounds":1})] {
        let (client, _) = mock(vec![envelope(thread(
            json!([
                comment("t1_first", "t3_abc123", "Retained."),
                comment("t1_second", "t1_first", "Bounded.")
            ]),
            1,
        ))])
        .await;
        let data = client
            .fetch(&reddit_request(budget.clone()), deadline())
            .await
            .unwrap();
        assert!(!data.metadata.complete);
        if budget.get("maxItems").is_some() {
            assert_eq!(data.metadata.items_collected, 1);
            assert_eq!(data.metadata.stop_reason, "maxItems");
        } else {
            assert_eq!(data.metadata.stop_reason, "maxRounds");
        }
        assert!(data.markdown.contains("Retained."));
    }
}

#[tokio::test]
async fn wrong_thread_identity_is_rejected_and_closed() {
    let mut snapshot = thread(json!([]), 0);
    snapshot["postId"] = json!("t3_other");
    let (client, mock) = mock(vec![envelope(snapshot)]).await;
    assert!(
        client
            .fetch(&reddit_request(json!({})), deadline())
            .await
            .is_err()
    );
    assert_eq!(mock.calls.lock().unwrap().last().unwrap().0, "close");
}

#[tokio::test]
async fn blocked_or_oversized_article_is_not_returned_as_content() {
    for html in [
        "<p>You've been blocked by network security.</p>".to_string(),
        "x".repeat(100_000),
    ] {
        let mut snapshot = article();
        snapshot["contentHtml"] = json!(html);
        let (client, mock) = mock(vec![envelope(snapshot)]).await;
        assert!(
            client
                .fetch(
                    &request(json!({"url":"https://example.com/article"})),
                    deadline()
                )
                .await
                .is_err()
        );
        assert_eq!(mock.calls.lock().unwrap().last().unwrap().0, "close");
    }
}

#[tokio::test]
async fn missing_comment_identity_is_explicitly_partial_without_inventing_ids() {
    let unknown = json!({"id":null,"parentId":"t3_abc123","permalink":null,"author":null,"depth":0,"bodyHtml":"<p>[deleted]</p>"});
    let (client, _) = mock(vec![envelope(thread(json!([unknown]), 0))]).await;
    // One snapshot budget avoids simulating the optional scroll step here.
    let data = client
        .fetch(&reddit_request(json!({"maxRounds":1})), deadline())
        .await
        .unwrap();
    assert!(!data.metadata.complete);
    assert!(data.json.as_ref().unwrap()["comments"][0]["id"].is_null());
    assert!(
        data.warnings
            .iter()
            .any(|warning| warning.contains("identity"))
    );
    assert!(data.markdown.contains("deleted"));
}

#[tokio::test]
async fn total_comment_payload_respects_max_bytes() {
    let (client, _) = mock(vec![envelope(thread(
        json!([
            comment("t1_first", "t3_abc123", "Short retained body."),
            comment("t1_second", "t1_first", &"long body ".repeat(100)),
        ]),
        1,
    ))])
    .await;
    let data = client
        .fetch(&reddit_request(json!({"maxBytes":700})), deadline())
        .await
        .unwrap();
    let bytes = data.markdown.len()
        + serde_json::to_vec(data.json.as_ref().unwrap())
            .unwrap()
            .len();
    assert!(bytes <= 700, "total content exceeded budget: {bytes}");
    assert_eq!(data.metadata.stop_reason, "maxBytes");
    assert_eq!(data.metadata.items_collected, 1);
    assert!(data.markdown.contains("Short retained body."));
}

async fn failing_navigation() -> StatusCode {
    StatusCode::BAD_GATEWAY
}
async fn slow_evaluation() -> Json<Value> {
    tokio::time::sleep(Duration::from_secs(5)).await;
    Json(envelope(article()))
}
async fn exceptional_mock(navigation_failure: bool) -> (BrowserPipelineClient, Mock) {
    let state = Mock {
        calls: Arc::default(),
        evaluations: Arc::new(Mutex::new(vec![envelope(article())])),
        ready_at: Arc::default(),
        active: Arc::default(),
        peak: Arc::default(),
        version: Arc::new(Mutex::new("2.4.8".into())),
        tabs: Arc::default(),
        create_failures: Arc::default(),
        reset_ack: Arc::new(Mutex::new(json!({"ok":true}))),
    };
    let app = Router::new()
        .route("/health", get(health))
        .route("/tabs", post(create).get(tabs))
        .route("/tabs/pipeline-tab/wait", post(wait))
        .route("/tabs/pipeline-tab", delete(close));
    let app = if navigation_failure {
        app.route("/tabs/pipeline-tab/navigate", post(failing_navigation))
            .route("/tabs/pipeline-tab/evaluate", post(evaluate))
    } else {
        app.route("/tabs/pipeline-tab/navigate", post(navigate))
            .route("/tabs/pipeline-tab/evaluate", post(slow_evaluation))
    }
    .with_state(state.clone());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (
        BrowserPipelineClient::new(&format!("http://{address}"), None),
        state,
    )
}

#[tokio::test]
async fn navigation_error_and_deadline_both_reap_known_tab() {
    for navigation_failure in [true, false] {
        let (client, mock) = exceptional_mock(navigation_failure).await;
        let start = std::time::Instant::now();
        let result = client
            .fetch(
                &request(json!({"url":"https://example.com/article"})),
                Deadline::from_request_ms(500),
            )
            .await;
        assert!(result.is_err());
        assert!(start.elapsed() < Duration::from_secs(1));
        assert_eq!(mock.calls.lock().unwrap().last().unwrap().0, "close");
    }
}

#[tokio::test]
async fn expired_deadline_never_creates_a_tab() {
    let (client, mock) = mock(vec![envelope(article())]).await;
    assert!(
        client
            .fetch(
                &request(json!({"url":"https://example.com/article"})),
                Deadline::from_request_ms(0)
            )
            .await
            .is_err()
    );
    assert!(mock.calls.lock().unwrap().is_empty());
}

#[tokio::test]
async fn expansion_settles_before_collecting_delayed_comments() {
    let first = comment("t1_first", "t3_abc123", "Initial comment.");
    let second = comment("t1_second", "t1_first", "Delayed comment.");
    let (client, _) = mock(vec![
        envelope(thread(json!([first.clone()]), 1)),
        {
            let mut expansion =
                envelope(json!({"clicked":1,"scrolled":false,"postId":"t3_abc123","error":null}));
            expansion["scheduleMs"] = json!(100);
            expansion
        },
        json!({
            "beforeReady": envelope(thread(json!([first.clone()]), 0)),
            "afterReady": envelope(thread(json!([first,second]), 0)),
        }),
    ])
    .await;
    let data = client
        .fetch(&reddit_request(json!({"maxRounds":2})), deadline())
        .await
        .unwrap();
    assert_eq!(data.metadata.items_collected, 2);
    assert!(data.markdown.contains("Delayed comment."));
}

#[tokio::test]
async fn verified_thread_can_discuss_a_network_security_block() {
    let mut snapshot = thread(json!([]), 0);
    snapshot["contentHtml"] =
        json!("<p>How do I resolve 'blocked by network security' when accessing Reddit?</p>");
    let (client, _) = mock(vec![envelope(snapshot)]).await;
    let data = client
        .fetch(&reddit_request(json!({"maxRounds":1})), deadline())
        .await
        .unwrap();
    assert!(data.markdown.contains("blocked by network security"));
    assert!(!data.metadata.complete);
}

#[tokio::test]
async fn concurrent_jobs_use_at_most_two_reusable_profiles() {
    let mut response = envelope(article());
    response["evaluationDelayMs"] = json!(60);
    let (client, mock) = mock(vec![response]).await;
    let client = Arc::new(client);
    let mut jobs = tokio::task::JoinSet::new();
    for _ in 0..4 {
        let client = client.clone();
        jobs.spawn(async move {
            client
                .fetch(
                    &request(json!({"url":"https://example.com/article"})),
                    Deadline::from_request_ms(5000),
                )
                .await
        });
    }
    while let Some(result) = jobs.join_next().await {
        result.unwrap().unwrap();
    }
    assert!(mock.peak.load(Ordering::SeqCst) <= 2);
    assert_eq!(mock.active.load(Ordering::SeqCst), 0);
    let users: std::collections::HashSet<_> = mock
        .calls
        .lock()
        .unwrap()
        .iter()
        .filter(|(action, _)| action == "create")
        .map(|(_, body)| body["userId"].as_str().unwrap().to_string())
        .collect();
    assert!(
        users.len() <= 2,
        "jobs must reuse bounded profiles: {users:?}"
    );
}

#[tokio::test]
async fn unguarded_browser_version_is_refused_before_tab_creation() {
    let (client, mock) = mock(vec![envelope(article())]).await;
    *mock.version.lock().unwrap() = "2.4.6".into();
    let result = client
        .fetch(
            &request(json!({"url":"https://example.com/article"})),
            deadline(),
        )
        .await;
    assert!(result.is_err());
    assert!(
        !mock
            .calls
            .lock()
            .unwrap()
            .iter()
            .any(|(action, _)| action == "create")
    );
}

#[tokio::test]
async fn unreaped_scoped_tab_prevents_opening_another() {
    let (client, mock) = mock(vec![envelope(article())]).await;
    // The browser reports success deleting this tab but leaves it present.
    *mock.tabs.lock().unwrap() = vec![json!({"tabId":"pipeline-tab"})];
    let result = client
        .fetch(
            &request(json!({"url":"https://example.com/article"})),
            deadline(),
        )
        .await;
    assert!(result.is_err());
    let calls = mock.calls.lock().unwrap();
    assert!(calls.iter().any(|(action, _)| action == "close"));
    assert!(!calls.iter().any(|(action, _)| action == "create"));
}

#[tokio::test]
async fn overflow_snapshots_drain_late_ids_before_expanding() {
    let mut first = thread(
        json!([comment("t1_first", "t3_abc123", "Early comment.")]),
        1,
    );
    first["overflow"] = json!(true);
    let second = thread(
        json!([comment("t1_late", "t1_first", "Late loaded comment.")]),
        0,
    );
    let (client, mock) = mock(vec![envelope(first), envelope(second)]).await;
    let data = client
        .fetch(&reddit_request(json!({"maxRounds":2})), deadline())
        .await
        .unwrap();
    assert_eq!(data.metadata.items_collected, 2);
    assert!(data.markdown.contains("Late loaded comment."));
    let calls = mock.calls.lock().unwrap();
    let evaluates: Vec<_> = calls
        .iter()
        .filter(|(action, _)| action == "evaluate")
        .collect();
    assert_eq!(
        evaluates.len(),
        2,
        "overflow must reharvest before interacting"
    );
    assert!(
        evaluates[1].1["expression"]
            .as_str()
            .unwrap()
            .contains("t1_first")
    );
    assert!(!data.metadata.complete);
}

#[tokio::test]
async fn failed_blank_create_resets_only_its_own_session_and_retries_once() {
    let (client, mock) = mock(vec![envelope(article())]).await;
    mock.create_failures.store(1, Ordering::SeqCst);
    let data = client
        .fetch(
            &request(json!({"url":"https://example.com/article"})),
            deadline(),
        )
        .await
        .unwrap();
    assert!(data.markdown.contains("Complete small page."));
    let calls = mock.calls.lock().unwrap();
    assert_eq!(calls[0].0, "create");
    assert_eq!(calls[1].0, "reset");
    assert_eq!(calls[2].0, "create");
    assert_eq!(calls[0].1["userId"], calls[1].1["userId"]);
    assert_eq!(calls[0].1["userId"], calls[2].1["userId"]);
    assert!(calls[2].1.get("url").is_none());
    assert_eq!(calls.last().unwrap().0, "close");
}

#[tokio::test]
async fn repeated_create_failure_does_not_loop_or_navigate() {
    let (client, mock) = mock(vec![envelope(article())]).await;
    mock.create_failures.store(5, Ordering::SeqCst);
    assert!(
        client
            .fetch(
                &request(json!({"url":"https://example.com/article"})),
                deadline()
            )
            .await
            .is_err()
    );
    let calls = mock.calls.lock().unwrap();
    assert_eq!(
        calls
            .iter()
            .filter(|(action, _)| action == "create")
            .count(),
        2
    );
    assert_eq!(
        calls.iter().filter(|(action, _)| action == "reset").count(),
        1
    );
    assert!(!calls.iter().any(|(action, _)| action == "navigate"));
}

#[tokio::test]
async fn reset_requires_explicit_acknowledgement_before_retrying_create() {
    let (client, mock) = mock(vec![envelope(article())]).await;
    mock.create_failures.store(1, Ordering::SeqCst);
    *mock.reset_ack.lock().unwrap() = json!({});
    assert!(
        client
            .fetch(
                &request(json!({"url":"https://example.com/article"})),
                deadline()
            )
            .await
            .is_err()
    );
    let calls = mock.calls.lock().unwrap();
    assert_eq!(
        calls
            .iter()
            .filter(|(action, _)| action == "create")
            .count(),
        1
    );
    assert_eq!(
        calls.iter().filter(|(action, _)| action == "reset").count(),
        1
    );
    assert!(!calls.iter().any(|(action, _)| action == "navigate"));
}
