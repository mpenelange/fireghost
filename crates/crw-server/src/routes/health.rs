use axum::Json;
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use serde_json::{Value, json};

use crate::build_info;
use crate::state::AppState;

/// Liveness — cheap: just confirms the process is up. Hit by the Docker
/// healthcheck every 30s, so this MUST stay sub-millisecond.
pub async fn health(State(state): State<AppState>) -> Json<Value> {
    let jobs = state.crawl_jobs.read().await;
    Json(json!({
        "status": "ok",
        "version": build_info::VERSION,
        "package_version": build_info::PACKAGE_VERSION,
        "revision": build_info::REVISION,
        "build_date": build_info::BUILD_DATE,
        "active_crawl_jobs": jobs.len(),
    }))
}

/// Readiness — runs the renderer health probes (Browser.getVersion per
/// CDP renderer) and returns 503 if any JS renderer is down. Designed for
/// off-host monitoring (healthchecks.io) at ~5 min cadence, not for the
/// hot Docker healthcheck. Keeps `/health` cheap.
pub async fn ready(State(state): State<AppState>) -> impl IntoResponse {
    let renderer_health = state.renderer.check_health().await;
    let all_ok = renderer_health.values().all(|v| *v);
    let body = json!({
        "status": if all_ok { "ready" } else { "degraded" },
        "version": build_info::VERSION,
        "package_version": build_info::PACKAGE_VERSION,
        "revision": build_info::REVISION,
        "build_date": build_info::BUILD_DATE,
        "renderers": renderer_health,
    });
    let status = if all_ok {
        StatusCode::OK
    } else {
        StatusCode::SERVICE_UNAVAILABLE
    };
    (status, Json::<Value>(body))
}
