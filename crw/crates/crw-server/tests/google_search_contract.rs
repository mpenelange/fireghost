//! Offline HTTP contracts: real AppState/search backend with a mocked browser.

use std::sync::{Arc, Mutex};

use axum::http::StatusCode;
use axum_test::TestServer;
use crw_core::config::AppConfig;
use crw_server::{app::create_app, state::AppState};
use serde_json::{Value, json};
use wiremock::matchers::{method, path};
use wiremock::{Mock, MockServer, Request, Respond, ResponseTemplate};

#[derive(Clone)]
struct Browser {
    url: Arc<Mutex<String>>,
    origin: Arc<Mutex<u64>>,
    challenge_google: bool,
}

impl Respond for Browser {
    fn respond(&self, request: &Request) -> ResponseTemplate {
        let mut observed = self.url.lock().unwrap();
        let reply = match (request.method.as_str(), request.url.path()) {
            ("POST", "/tabs/search-tab/navigate") => {
                let body: Value = request.body_json().unwrap();
                let destination = body["url"].as_str().unwrap();
                *observed = if self.challenge_google && destination.contains("google.com/search") {
                    "https://www.google.com/sorry/index?continue=private-query".into()
                } else {
                    destination.into()
                };
                *self.origin.lock().unwrap() += 100;
                json!({"ok": true, "url": &*observed})
            }
            ("GET", "/tabs") => {
                json!({"ok": true, "tabs": [{"tabId": "search-tab", "url": &*observed}]})
            }
            ("POST", "/tabs/search-tab/evaluate") => {
                let body: Value = request.body_json().unwrap();
                let expression = body["expression"].as_str().unwrap();
                if let Some(at) = expression.find("location.assign(") {
                    let destination = serde_json::Deserializer::from_str(
                        &expression[at + "location.assign(".len()..],
                    )
                    .into_iter::<String>()
                    .next()
                    .unwrap()
                    .unwrap();
                    *observed =
                        if self.challenge_google && destination.contains("google.com/search") {
                            "https://www.google.com/sorry/index?continue=private-query".into()
                        } else {
                            destination
                        };
                    let mut origin = self.origin.lock().unwrap();
                    let previous = *origin;
                    *origin += 100;
                    return ResponseTemplate::new(200).set_body_json(json!({
                        "ok":true,"result":previous,"truncated":false
                    }));
                }
                let rows = if observed.contains("en.wikipedia.org") {
                    json!([{"url": "https://en.wikipedia.org/wiki/Rust_(programming_language)", "title": "Rust (programming language)", "content": "Rust is a programming language."}])
                } else {
                    json!([])
                };
                let result = if expression.contains("timeOrigin") {
                    json!({"url":&*observed,"timeOrigin":*self.origin.lock().unwrap(),"rows":rows})
                        .to_string()
                } else {
                    rows.to_string()
                };
                json!({"ok": true, "result": result, "truncated": false})
            }
            _ => panic!("unexpected mock-browser route"),
        };
        ResponseTemplate::new(200).set_body_json(reply)
    }
}

async fn test_app(challenge_google: bool) -> (TestServer, MockServer) {
    let upstream = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/tabs"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(json!({"ok": true, "tabId": "search-tab"})),
        )
        .expect(1)
        .mount(&upstream)
        .await;
    let browser = Browser {
        url: Arc::new(Mutex::new("about:blank".into())),
        origin: Arc::new(Mutex::new(100)),
        challenge_google,
    };
    for (verb, route) in [
        ("GET", "/tabs"),
        ("POST", "/tabs/search-tab/navigate"),
        ("POST", "/tabs/search-tab/evaluate"),
    ] {
        Mock::given(method(verb))
            .and(path(route))
            .respond_with(browser.clone())
            .mount(&upstream)
            .await;
    }
    let config: AppConfig = toml::from_str(&format!(
        "[search]\nenabled = true\ntimeout_ms = 1500\n[renderer.camofox]\nbase_url = {:?}",
        upstream.uri()
    ))
    .unwrap();
    let state = AppState::new(config).unwrap();
    (TestServer::new(create_app(state)), upstream)
}

#[tokio::test]
async fn google_challenge_returns_typed_502_instead_of_empty_success() {
    let (server, upstream) = test_app(true).await;
    let response = server
        .post("/v2/search")
        .json(&json!({"query": "rust", "engines": ["google"]}))
        .await;
    response.assert_status(StatusCode::BAD_GATEWAY);
    let body: Value = response.json();
    assert_eq!(body["success"], false);
    assert_eq!(body["error_code"], "search_blocked");
    assert!(body["error"].as_str().unwrap().contains("google"));
    assert!(!body.to_string().contains("private-query"));
    upstream.verify().await;
}

#[tokio::test]
async fn blocked_google_with_wikipedia_results_is_partial_success_with_warnings() {
    let (server, upstream) = test_app(true).await;
    let response = server
        .post("/v2/search")
        .json(&json!({"query": "rust", "engines": ["google", "wikipedia"]}))
        .await;
    response.assert_status_ok();
    let body: Value = response.json();
    assert_eq!(body["success"], true);
    assert_eq!(body["data"]["web"].as_array().unwrap().len(), 1);
    assert_eq!(
        body["data"]["web"][0]["title"],
        "Rust (programming language)"
    );
    let warnings = body["warnings"]
        .as_array()
        .expect("partial engine failure must survive v2 reshaping");
    assert!(warnings.iter().any(|warning| {
        let message = warning.as_str().unwrap();
        message.contains("google") && message.contains("blocked")
    }));
    assert!(body.get("error_code").is_none());
    assert!(!body.to_string().contains("private-query"));
    upstream.verify().await;
}

#[tokio::test]
async fn valid_google_empty_array_remains_success_with_neutral_warning() {
    let (server, upstream) = test_app(false).await;
    let response = server
        .post("/v2/search")
        .json(&json!({"query": "rust", "engines": ["google"]}))
        .await;
    response.assert_status_ok();
    let body: Value = response.json();
    assert_eq!(body["success"], true);
    assert_eq!(body["data"]["web"], json!([]));
    let warnings = body["warnings"]
        .as_array()
        .expect("neutral empty-result notice must survive v2 reshaping");
    assert!(
        warnings
            .iter()
            .any(|warning| warning.as_str().unwrap().contains("returned no results"))
    );
    assert!(
        warnings
            .iter()
            .all(|warning| !warning.as_str().unwrap().contains("blocked"))
    );
    assert!(body.get("error_code").is_none());
    upstream.verify().await;
}
