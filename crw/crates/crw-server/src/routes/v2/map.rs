//! `POST /v2/map` — discover URLs, returning v2 link OBJECTS (the headline
//! v1→v2 delta: v1 returned `links: string[]`, v2 returns
//! `links: [{url, title?, description?}]`).

use std::time::Duration;

use axum::Json;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use serde::{Deserialize, Serialize};

use crw_core::error::CrwError;
use crw_crawl::crawl::{DiscoverOptions, discover_urls};

use super::adapters::V2Link;
use crate::error::AppError;
use crate::state::AppState;

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct V2MapRequest {
    pub url: String,
    #[serde(default)]
    pub limit: Option<usize>,
    #[serde(default)]
    pub include_paths: Vec<String>,
    #[serde(default)]
    pub exclude_paths: Vec<String>,
    #[serde(default)]
    pub search: Option<String>,
    /// "include" (default) | "only" | "skip".
    #[serde(default = "default_sitemap")]
    pub sitemap: String,
    #[serde(default)]
    pub max_discovery_depth: Option<u32>,
    /// v2 `timeout` is milliseconds.
    #[serde(default)]
    pub timeout: Option<u64>,
}

fn default_sitemap() -> String {
    "include".to_string()
}

#[derive(Debug, Serialize)]
pub struct V2MapResponse {
    pub success: bool,
    pub links: Vec<V2Link>,
}

pub async fn map(
    State(state): State<AppState>,
    body: Result<Json<V2MapRequest>, JsonRejection>,
) -> Result<Json<V2MapResponse>, AppError> {
    let Json(req) = body.map_err(AppError::from)?;
    let parsed_url = url::Url::parse(&req.url)
        .map_err(|e| CrwError::InvalidRequest(format!("Invalid URL: {e}")))?;
    crw_core::url_safety::validate_safe_url_resolved(&parsed_url)
        .await
        .map_err(CrwError::InvalidRequest)?;

    let use_sitemap = !req.sitemap.eq_ignore_ascii_case("skip");
    let crawl_fallback = !req.sitemap.eq_ignore_ascii_case("only");
    let max_depth = req
        .max_discovery_depth
        .unwrap_or(state.config.crawler.default_max_depth);
    let timeout_secs = req
        .timeout
        .map(|ms| (ms / 1000).max(1))
        .unwrap_or(120)
        .min(300);

    let narrows = !req.include_paths.is_empty()
        || !req.exclude_paths.is_empty()
        || req.search.as_deref().is_some_and(|s| !s.is_empty());

    let fut = discover_urls(DiscoverOptions {
        base_url: &req.url,
        max_depth,
        use_sitemap,
        renderer: &state.renderer,
        max_concurrency: state.config.crawler.max_concurrency,
        requests_per_second: state.config.crawler.requests_per_second,
        user_agent: &state.config.crawler.user_agent,
        proxy: state.config.crawler.proxy.clone(),
        deadline_ms_per_page: state.config.effective_deadline_ms(None, None),
        per_host_max_concurrent: state.config.crawler.per_host_max_concurrent,
        crawl_fallback,
        url_filter: state.url_filter.clone(),
        // Push the caller's limit INTO discovery so a large `limit` actually
        // discovers that many URLs. When include/exclude/search will narrow
        // the set, discover at least the default amount instead: capping
        // discovery at `limit` first would filter only the first few URLs
        // (often matching none). The `truncate` below then applies `limit`.
        max_urls: discovery_cap(req.limit, narrows),
        // Stop just before the backstop below and return partials, instead of
        // letting the backstop drop the future and lose every discovered URL.
        overall_deadline: crw_crawl::crawl::discovery_deadline(Duration::from_secs(timeout_secs)),
        respect_robots: state.config.crawler.respect_robots_txt,
    });

    let result = match tokio::time::timeout(Duration::from_secs(timeout_secs), fut).await {
        Ok(r) => r?,
        Err(_) => return Err(AppError(CrwError::Timeout(timeout_secs * 1000))),
    };

    let mut urls = result.urls;
    if !req.include_paths.is_empty() {
        urls.retain(|u| req.include_paths.iter().any(|p| u.contains(p.as_str())));
    }
    if !req.exclude_paths.is_empty() {
        urls.retain(|u| !req.exclude_paths.iter().any(|p| u.contains(p.as_str())));
    }
    if let Some(s) = req.search.as_ref().filter(|s| !s.is_empty()) {
        let needle = s.to_lowercase();
        urls.retain(|u| u.to_lowercase().contains(&needle));
    }
    // `0` means unbounded (matches discovery); only truncate for a positive cap.
    if let Some(limit) = req.limit.filter(|l| *l > 0) {
        urls.truncate(limit);
    }

    let links = urls
        .into_iter()
        .map(|url| V2Link {
            url,
            title: None,
            description: None,
        })
        .collect();

    Ok(Json(V2MapResponse {
        success: true,
        links,
    }))
}

/// URLs to discover for a map request. `0` stays unbounded.
fn discovery_cap(limit: Option<usize>, narrows: bool) -> usize {
    let default = crw_crawl::crawl::DEFAULT_MAX_DISCOVERED_URLS;
    match limit {
        Some(limit) if limit == 0 || !narrows => limit,
        Some(limit) => limit.max(default),
        None => default,
    }
}

#[cfg(test)]
mod discovery_cap_tests {
    use super::discovery_cap;
    use crw_crawl::crawl::DEFAULT_MAX_DISCOVERED_URLS as DEFAULT;

    #[test]
    fn limit_bounds_discovery_without_filters() {
        assert_eq!(discovery_cap(Some(5), false), 5);
        assert_eq!(discovery_cap(None, false), DEFAULT);
    }

    #[test]
    fn filters_discover_beyond_a_small_limit() {
        assert_eq!(discovery_cap(Some(5), true), DEFAULT);
        assert_eq!(discovery_cap(Some(DEFAULT + 1), true), DEFAULT + 1);
    }

    #[test]
    fn zero_stays_unbounded() {
        assert_eq!(discovery_cap(Some(0), true), 0);
        assert_eq!(discovery_cap(Some(0), false), 0);
    }
}
