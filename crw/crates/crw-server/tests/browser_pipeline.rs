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
    use wiremock::matchers::{method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};
    let upstream = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/health"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(json!({"ok":true,"version":"2.4.8"})),
        )
        .mount(&upstream)
        .await;
    Mock::given(method("GET"))
        .and(path("/tabs"))
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
        Mock::given(method("POST"))
            .and(path(route))
            .respond_with(ResponseTemplate::new(200).set_body_json(response))
            .mount(&upstream)
            .await;
    }
    Mock::given(method("DELETE"))
        .and(path("/tabs/compact-tab"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"ok":true})))
        .expect(1)
        .mount(&upstream)
        .await;
    let s = server(&format!(
        "[renderer.camofox]\nbase_url = {:?}",
        upstream.uri()
    ));
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
    upstream.verify().await;
}
