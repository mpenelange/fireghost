//! `POST /v2/search` — reuses the v1 `search_inner` engine, reshaping the
//! response into the v2 envelope `{ success, data: {web,news,images}, creditsUsed, id }`.

use axum::Json;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use serde::Serialize;
use serde_json::Value;
use uuid::Uuid;

use crw_core::error::CrwError;
use crw_core::types::{ImageResult, SearchData, SearchRequest, SearchResponse, SearchResult};

use crate::error::AppError;
use crate::routes::search::search_inner;
use crate::state::AppState;

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct V2SearchResponse {
    pub success: bool,
    pub data: V2SearchData,
    pub credits_used: u32,
    pub id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub warning: Option<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub warnings: Vec<String>,
}

#[derive(Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct V2SearchData {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub web: Option<Vec<SearchResult>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub news: Option<Vec<SearchResult>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub images: Option<Vec<ImageResult>>,
}

/// v2 `scrapeOptions.formats` may be objects; the v1 `SearchRequest` only
/// accepts string formats. Rewrite the formats array to strings (lifting a
/// `json` schema to `jsonSchema`) before deserializing into `SearchRequest`.
fn normalize_search_body(mut v: Value) -> Value {
    if let Some(opts) = v.get_mut("scrapeOptions").and_then(Value::as_object_mut)
        && let Some(Value::Array(arr)) = opts.get("formats").cloned()
    {
        let mut strs = Vec::new();
        let mut schema: Option<Value> = None;
        for f in arr {
            match f {
                Value::String(s) => strs.push(Value::String(s)),
                Value::Object(m) => {
                    if let Some(t) = m.get("type").and_then(Value::as_str) {
                        strs.push(Value::String(t.to_string()));
                        if t == "json"
                            && let Some(s) = m.get("schema")
                        {
                            schema = Some(s.clone());
                        }
                    }
                }
                _ => {}
            }
        }
        opts.insert("formats".to_string(), Value::Array(strs));
        if let Some(s) = schema {
            opts.entry("jsonSchema".to_string()).or_insert(s);
        }
    }
    v
}

fn shape(results: SearchData) -> V2SearchData {
    match results {
        SearchData::Flat(v) => V2SearchData {
            web: Some(v),
            ..Default::default()
        },
        SearchData::Grouped(g) => V2SearchData {
            web: g.web,
            news: g.news,
            images: g.images,
        },
    }
}

pub async fn search(
    State(state): State<AppState>,
    body: Result<Json<Value>, JsonRejection>,
) -> Result<Json<V2SearchResponse>, AppError> {
    let Json(raw) = body.map_err(AppError::from)?;
    let normalized = normalize_search_body(raw);
    let req: SearchRequest = serde_json::from_value(normalized)
        .map_err(|e| CrwError::InvalidRequest(format!("Invalid search request: {e}")))?;

    let resp = search_inner(&state, req).await?;
    Ok(Json(response_from_v1(resp)))
}

fn response_from_v1(resp: SearchResponse) -> V2SearchResponse {
    let (data, warnings) = resp
        .data
        .map(|d| (shape(d.results), d.warnings))
        .unwrap_or_default();
    V2SearchResponse {
        success: true,
        data,
        credits_used: 0,
        id: Uuid::new_v4().to_string(),
        warning: resp.warning,
        warnings,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crw_core::types::{ApiResponse, SearchResponseData};
    use serde_json::json;

    fn v1_response(results: Vec<SearchResult>) -> SearchResponse {
        ApiResponse::ok(SearchResponseData {
            results: SearchData::Flat(results),
            answer: None,
            citations: Vec::new(),
            llm_usage: None,
            warnings: Vec::new(),
        })
    }

    #[test]
    fn v2_response_preserves_partial_engine_warnings_and_enrichment_warning() {
        let row: SearchResult = serde_json::from_value(json!({
            "url": "https://example.com/", "title": "Example Domain", "description": "Documentation examples", "position": 1
        })).unwrap();
        let mut partial = v1_response(vec![row]);
        partial.warning = Some("scrape enrichment partially failed".into());
        partial
            .data
            .as_mut()
            .unwrap()
            .warnings
            .push("search engine 'google' blocked the request".into());
        let serialized = serde_json::to_value(response_from_v1(partial)).unwrap();
        assert_eq!(serialized["success"], true);
        assert_eq!(serialized["data"]["web"].as_array().unwrap().len(), 1);
        assert_eq!(serialized["warning"], "scrape enrichment partially failed");
        assert_eq!(
            serialized["warnings"],
            json!(["search engine 'google' blocked the request"])
        );
        assert_eq!(serialized["creditsUsed"], 0);
        assert!(serialized["id"].as_str().is_some());
    }

    #[test]
    fn v2_response_genuine_empty_success_does_not_invent_failure_or_warning_fields() {
        let serialized = serde_json::to_value(response_from_v1(v1_response(Vec::new()))).unwrap();
        assert_eq!(serialized["success"], true);
        assert_eq!(serialized["data"]["web"], json!([]));
        assert!(serialized.get("warning").is_none());
        assert!(serialized.get("warnings").is_none());
        assert!(serialized.get("error_code").is_none());
    }
}
