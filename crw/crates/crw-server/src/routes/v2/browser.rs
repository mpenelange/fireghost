//! Dedicated, opt-in compact browser extraction contract.

use axum::Json;
use axum::extract::{State, rejection::JsonRejection};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use crw_core::{Deadline, error::CrwError, types::ApiResponse};
use crw_renderer::pipeline::{BrowserPipelineData, BrowserPipelineRequest};

use crate::{error::AppError, state::AppState};

pub struct BrowserError(Box<Response>);

impl From<CrwError> for BrowserError {
    fn from(error: CrwError) -> Self {
        Self(Box::new(AppError(error).into_response()))
    }
}

impl IntoResponse for BrowserError {
    fn into_response(self) -> Response {
        *self.0
    }
}

pub async fn scrape(
    State(state): State<AppState>,
    body: Result<Json<BrowserPipelineRequest>, JsonRejection>,
) -> Result<Json<ApiResponse<BrowserPipelineData>>, BrowserError> {
    let Json(request) =
        body.map_err(|error| BrowserError(Box::new(AppError::from(error).into_response())))?;
    request.validate()?;
    let deadline = Deadline::from_request_ms(request.timeout);
    let url = url::Url::parse(&request.url)
        .map_err(|e| CrwError::InvalidRequest(format!("Invalid URL: {e}")))?;
    tokio::time::timeout(
        deadline.remaining(),
        crw_core::url_safety::validate_safe_url_resolved(&url),
    )
    .await
    .map_err(|_| CrwError::Timeout(request.timeout))?
    .map_err(CrwError::InvalidRequest)?;
    let client = state.browser_pipeline.as_ref().ok_or_else(|| {
        BrowserError(Box::new(
            (
                StatusCode::SERVICE_UNAVAILABLE,
                Json(ApiResponse::<()>::err_with_code(
                    "browser pipeline requires renderer.camofox configuration",
                    "browser_unavailable",
                )),
            )
                .into_response(),
        ))
    })?;
    let data = client.fetch(&request, deadline).await?;
    Ok(Json(ApiResponse::ok(data)))
}
