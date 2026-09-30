#![cfg(feature = "camofox")]

use axum::http::StatusCode;
use axum_test::TestServer;
use crw_core::config::AppConfig;
use crw_server::{app::create_app, state::AppState};
use serde_json::json;

fn server(config: &str) -> TestServer {
    TestServer::new(create_app(
        AppState::new(toml::from_str::<AppConfig>(config).unwrap()).unwrap(),
    ))
}

#[tokio::test]
async fn browser_pipeline_rejects_invalid_input_before_browser() {
    let s = server("");
    for body in [
        json!({"url":"not-a-url"}),
        json!({"url":"http://127.0.0.1/"}),
        json!({"url":"https://example.com","timeout":60001}),
        json!({"url":"https://example.com","profile":"redditThread"}),
        json!({"url":"https://example.com","actions":[{"type":"executeJavascript"}]}),
    ] {
        let r = s.post("/v2/browser/scrape").json(&body).await;
        r.assert_status(StatusCode::BAD_REQUEST);
        assert_eq!(r.json::<serde_json::Value>()["success"], false);
    }
}

#[tokio::test]
async fn browser_pipeline_requires_camofox_configuration() {
    let s = server("");
    // Numeric public destination keeps this contract test offline.
    let r = s
        .post("/v2/browser/scrape")
        .json(&json!({"url":"https://1.1.1.1/"}))
        .await;
    r.assert_status(StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(r.json::<serde_json::Value>()["success"], false);
    assert_eq!(
        r.json::<serde_json::Value>()["error_code"],
        "browser_unavailable"
    );
}

#[tokio::test]
async fn browser_pipeline_inherits_auth_and_method_rejection() {
    let s = server("[auth]\napi_keys = [\"test-key\"]");
    s.post("/v2/browser/scrape")
        .json(&json!({"url":"https://1.1.1.1/"}))
        .await
        .assert_status(StatusCode::UNAUTHORIZED);
    let s = server("");
    s.get("/v2/browser/scrape")
        .await
        .assert_status(StatusCode::METHOD_NOT_ALLOWED);
}

#[tokio::test]
async fn browser_pipeline_returns_compacted_article_and_closes_tab() {
    let upstream = article_browser(None).await;
    let s = server(&format!(
        "[renderer.camofox]\nbase_url = {:?}",
        upstream.uri()
    ));
    assert_compact_article(&s).await;
    upstream.verify().await;
}

async fn article_browser(api_key: Option<&str>) -> wiremock::MockServer {
    use wiremock::matchers::{header, method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};
    let upstream = MockServer::start().await;
    let mock = |verb: &str, route: &str| {
        let builder = Mock::given(method(verb)).and(path(route));
        if let Some(key) = api_key {
            builder.and(header("Authorization", format!("Bearer {key}")))
        } else {
            builder
        }
    };
    mock("GET", "/health")
        .respond_with(
            ResponseTemplate::new(200).set_body_json(json!({"ok":true,"version":"2.4.8"})),
        )
        .mount(&upstream)
        .await;
    mock("GET", "/tabs")
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"running":true,"tabs":[]})))
        .mount(&upstream)
        .await;
    for (route, response) in [
        ("/tabs", json!({"tabId":"compact-tab"})),
        ("/tabs/compact-tab/navigate", json!({"ok":true})),
        ("/tabs/compact-tab/wait", json!({"ok":true,"ready":true})),
        (
            "/tabs/compact-tab/evaluate",
            json!({"ok":true,"truncated":false,"result":json!({
            "url":"https://1.1.1.1/article", "title":"A compact article",
            "contentHtml":"<article><h1>A compact article</h1><p>Useful article content.</p></article>",
            "items":[],"complete":true,"overflow":false,"outstandingControls":0,
            "advertisedCount":null,"error":null,"postId":null,"commentsLoaded":false
        }).to_string()}),
        ),
    ] {
        mock("POST", route)
            .respond_with(ResponseTemplate::new(200).set_body_json(response))
            .mount(&upstream)
            .await;
    }
    mock("DELETE", "/tabs/compact-tab")
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"ok":true})))
        .expect(1)
        .mount(&upstream)
        .await;
    upstream
}

async fn assert_compact_article(s: &TestServer) {
    let r = s
        .post("/v2/browser/scrape")
        .json(&json!({"url":"https://1.1.1.1/article"}))
        .await;
    r.assert_status_ok();
    let body = r.json::<serde_json::Value>();
    assert_eq!(body["success"], true);
    assert_eq!(body["data"]["metadata"]["pipeline"], "browser-v1");
    assert!(
        body["data"]["markdown"]
            .as_str()
            .unwrap()
            .contains("Useful article content.")
    );
}

#[tokio::test]
async fn browser_pipeline_dedicated_endpoint_works_without_legacy_renderer_or_search() {
    let upstream = article_browser(Some("pipeline-key")).await;
    let config: AppConfig = toml::from_str(&format!(
        "[renderer.browser_pipeline]\nbase_url = {:?}\napi_key = \"pipeline-key\"",
        upstream.uri()
    ))
    .unwrap();
    let state = AppState::new(config).unwrap();
    assert!(
        state.search.is_none(),
        "pipeline endpoint must not enable legacy search"
    );
    assert!(
        !state.renderer.js_renderer_names().contains(&"camofox"),
        "pipeline endpoint must not join the legacy scrape ladder"
    );
    let s = TestServer::new(create_app(state));
    assert_compact_article(&s).await;
    upstream.verify().await;
}

#[tokio::test]
async fn browser_pipeline_dedicated_endpoint_overrides_only_pipeline_browser() {
    let legacy = wiremock::MockServer::start().await;
    let dedicated = article_browser(Some("pipeline-key")).await;
    let config: AppConfig = toml::from_str(&format!(
        "[search]\nenabled = true\n[renderer.camofox]\nbase_url = {:?}\napi_key = \"legacy-key\"\n[renderer.browser_pipeline]\nbase_url = {:?}\napi_key = \"pipeline-key\"",
        legacy.uri(), dedicated.uri()
    ))
    .unwrap();
    let state = AppState::new(config).unwrap();
    assert_eq!(state.search.as_ref().unwrap().base_url(), legacy.uri());
    assert!(state.renderer.js_renderer_names().contains(&"camofox"));
    assert_eq!(
        state
            .config
            .renderer
            .camofox
            .as_ref()
            .unwrap()
            .api_key
            .as_deref(),
        Some("legacy-key")
    );
    let s = TestServer::new(create_app(state));
    assert_compact_article(&s).await;
    dedicated.verify().await;
    assert!(
        legacy.received_requests().await.unwrap().is_empty(),
        "pipeline must never make browser requests against legacy Camofox"
    );
}
