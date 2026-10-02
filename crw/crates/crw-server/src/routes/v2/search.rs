//! `POST /v2/search` — reuses the v1 `search_inner` engine, reshaping the
//! response into the v2 envelope `{ success, data: {web,news,images}, creditsUsed, id }`.

use axum::Json;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use serde::Serialize;
use serde_json::Value;
use uuid::Uuid;

use crw_core::error::CrwError;
use crw_core::types::{ImageResult, SearchData, SearchRequest, SearchResult};

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
    // v2 `sources` entries may be objects (`{"type":"web"}`, which is what the
    // Firecrawl SDKs and CLI send) and may name sources CRW cannot serve, such
    // as Firecrawl's `alexandria` catalogue. Keep the supported type names;
    // when none remain, drop the field so the request runs as a plain web
    // search, which `shape` still reports under `data.web`. Rejecting the
    // request instead makes Firecrawl-compatible clients fail outright.
    if let Some(Value::Array(sources)) = v.get("sources").cloned() {
        let kept: Vec<Value> = sources
            .iter()
            .filter_map(|source| {
                let name = match source {
                    Value::String(name) => name.as_str(),
                    Value::Object(m) => m.get("type")?.as_str()?,
                    _ => return None,
                };
                matches!(name, "web" | "news" | "images").then(|| Value::String(name.to_string()))
            })
            .collect();
        if let Some(object) = v.as_object_mut() {
            if kept.is_empty() {
                object.remove("sources");
            } else {
                object.insert("sources".to_string(), Value::Array(kept));
            }
        }
    }
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
    let data = resp.data.map(|d| shape(d.results)).unwrap_or_default();

    Ok(Json(V2SearchResponse {
        success: true,
        data,
        credits_used: 0,
        id: Uuid::new_v4().to_string(),
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crw_core::types::SearchSource;
    use serde_json::json;

    fn sources(body: Value) -> Option<Vec<SearchSource>> {
        let req: SearchRequest = serde_json::from_value(normalize_search_body(body)).unwrap();
        req.sources
    }

    #[test]
    fn object_sources_from_firecrawl_sdk_are_accepted() {
        let got = sources(json!({"query": "q", "sources": [{"type": "web"}, {"type": "news"}]}));
        assert_eq!(got, Some(vec![SearchSource::Web, SearchSource::News]));
    }

    #[test]
    fn unsupported_sources_are_dropped() {
        let got = sources(json!({"query": "q", "sources": ["web", {"type": "alexandria"}]}));
        assert_eq!(got, Some(vec![SearchSource::Web]));
    }

    #[test]
    fn only_unsupported_sources_become_a_plain_search() {
        let got = sources(json!({"query": "q", "sources": [{"type": "alexandria"}]}));
        assert_eq!(got, None);
    }

    #[test]
    fn string_sources_are_unchanged() {
        assert_eq!(
            sources(json!({"query": "q", "sources": ["images"]})),
            Some(vec![SearchSource::Images])
        );
        assert_eq!(
            sources(json!({"query": "q", "sources": "web,news"})),
            Some(vec![SearchSource::Web, SearchSource::News])
        );
        assert_eq!(sources(json!({"query": "q"})), None);
    }

    #[test]
    fn firecrawl_cli_search_body_deserializes() {
        let body = json!({
            "query": "lightpanda headless browser", "limit": 2, "integration": "cli",
            "toolDetail": "compact", "domainTools": false, "sources": [{"type": "web"}],
            "ignoreInvalidURLs": false,
            "scrapeOptions": {"formats": [{"type": "markdown"}], "onlyMainContent": true},
            "origin": "js-sdk@4.40.0"
        });
        assert_eq!(sources(body), Some(vec![SearchSource::Web]));
    }
}
