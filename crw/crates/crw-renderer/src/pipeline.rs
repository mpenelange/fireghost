//! Bounded browser extraction, separate from generic full-DOM fetching.

mod browser;

use browser::{BrowserAdapter, SNAPSHOT_LIMIT};

use crw_core::Deadline;
use crw_core::error::{CrwError, CrwResult};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::collections::{HashMap, HashSet};
use std::time::{Duration, Instant};
use url::Url;

fn default_timeout() -> u64 {
    30_000
}
fn default_rounds() -> usize {
    20
}
fn default_items() -> usize {
    200
}
fn default_bytes() -> usize {
    196_608
}

#[derive(Debug, Default, Clone, Copy, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub enum BrowserPipelineProfile {
    #[default]
    Article,
    RedditThread,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct BrowserPipelineRequest {
    pub url: String,
    #[serde(default)]
    pub profile: BrowserPipelineProfile,
    #[serde(default = "default_timeout")]
    pub timeout: u64,
    #[serde(default = "default_rounds")]
    pub max_rounds: usize,
    #[serde(default = "default_items")]
    pub max_items: usize,
    #[serde(default = "default_bytes")]
    pub max_bytes: usize,
}

impl BrowserPipelineRequest {
    pub fn validate(&self) -> CrwResult<()> {
        let url = Url::parse(&self.url)
            .map_err(|_| CrwError::InvalidRequest("pipeline URL must be absolute".into()))?;
        if !matches!(url.scheme(), "http" | "https")
            || url.host_str().is_none()
            || !url.username().is_empty()
            || url.password().is_some()
        {
            return Err(CrwError::InvalidRequest(
                "pipeline URL must use HTTP(S) without credentials".into(),
            ));
        }
        crw_core::url_safety::validate_safe_url(&url).map_err(CrwError::InvalidRequest)?;
        if self.timeout == 0
            || self.timeout > 60_000
            || self.max_rounds == 0
            || self.max_rounds > 100
            || self.max_items == 0
            || self.max_items > 1000
            || self.max_bytes == 0
            || self.max_bytes > 262_144
        {
            return Err(CrwError::InvalidRequest(
                "pipeline budgets exceed supported bounds".into(),
            ));
        }
        if self.profile == BrowserPipelineProfile::RedditThread && reddit_post_id(&url).is_none() {
            return Err(CrwError::InvalidRequest(
                "redditThread requires a Reddit comments URL".into(),
            ));
        }
        Ok(())
    }
}

fn reddit_post_id(url: &Url) -> Option<String> {
    if !matches!(
        url.host_str()?,
        "reddit.com" | "www.reddit.com" | "old.reddit.com" | "new.reddit.com"
    ) {
        return None;
    }
    let parts: Vec<_> = url.path_segments()?.collect();
    let at = parts.iter().position(|part| *part == "comments")?;
    let id = *parts.get(at + 1)?;
    (!id.is_empty() && id.bytes().all(|b| b.is_ascii_alphanumeric()))
        .then(|| id.to_ascii_lowercase())
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct BrowserPipelineMetadata {
    pub pipeline: String,
    pub profile: BrowserPipelineProfile,
    #[serde(rename = "sourceURL")]
    pub source_url: String,
    pub url: String,
    pub title: String,
    pub rendered_with: String,
    pub items_collected: usize,
    pub reported_total: Option<usize>,
    pub complete: bool,
    pub stop_reason: String,
    pub rounds: usize,
    pub elapsed_ms: u64,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct BrowserPipelineData {
    pub markdown: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub json: Option<Value>,
    pub metadata: BrowserPipelineMetadata,
    pub warnings: Vec<String>,
}

#[derive(Clone)]
pub struct BrowserPipelineClient {
    browser: BrowserAdapter,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Snapshot {
    url: String,
    title: String,
    content_html: String,
    items: Vec<Comment>,
    complete: bool,
    overflow: bool,
    outstanding_controls: usize,
    advertised_count: Option<usize>,
    error: Option<String>,
    post_id: Option<String>,
    comments_loaded: bool,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Comment {
    id: Option<String>,
    parent_id: Option<String>,
    permalink: Option<String>,
    author: Option<String>,
    depth: usize,
    body_html: String,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct CollectedComment {
    id: Option<String>,
    parent_id: Option<String>,
    permalink: Option<String>,
    author: Option<String>,
    depth: usize,
    markdown: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Expansion {
    clicked: usize,
    #[serde(default)]
    scrolled: bool,
    post_id: Option<String>,
    error: Option<String>,
}

fn pipeline_error(message: impl Into<String>) -> CrwError {
    CrwError::RendererError(format!("browser pipeline: {}", message.into()))
}

fn timeout_error(deadline: Deadline) -> CrwError {
    CrwError::Timeout(deadline.overrun().as_millis().max(1) as u64)
}

fn check_content(html: &str, detect_block: bool) -> CrwResult<String> {
    let wrapped = format!("<html><body>{html}</body></html>");
    let blocked = crw_extract::antibot::classify(Some(200), &wrapped);
    if detect_block
        && (!matches!(
            blocked.signal,
            crw_extract::antibot::AntibotSignal::None
                | crw_extract::antibot::AntibotSignal::StructuralFailure
        ) || crate::detector::looks_like_generic_bot_wall(&wrapped)
            || crate::detector::looks_like_cloudflare_challenge(&wrapped))
    {
        return Err(pipeline_error(
            "snapshot contains a challenge or blocked page",
        ));
    }
    let markdown = crw_extract::markdown::html_to_markdown(html);
    if markdown.trim().is_empty() {
        return Err(pipeline_error("snapshot contains no readable content"));
    }
    Ok(markdown)
}

fn check_thread(url: &str, post_id: Option<&str>, expected: &str) -> CrwResult<()> {
    let actual = Url::parse(url).ok().and_then(|url| reddit_post_id(&url));
    let id = post_id.map(|id| id.strip_prefix("t3_").unwrap_or(id));
    if actual.as_deref() != Some(expected) || id != Some(expected) {
        return Err(pipeline_error("snapshot changed Reddit thread identity"));
    }
    Ok(())
}

fn comment_depths(comments: &[CollectedComment], expected_thread: &str) -> CrwResult<Vec<usize>> {
    let known: HashMap<_, _> = comments
        .iter()
        .enumerate()
        .filter_map(|(index, comment)| comment.id.as_deref().map(|id| (id, index)))
        .collect();
    let post_id = format!("t3_{expected_thread}");
    let mut depths = vec![None; comments.len()];
    let mut visiting = vec![false; comments.len()];
    for start in 0..comments.len() {
        let mut chain = Vec::new();
        let mut current = start;
        loop {
            if depths[current].is_some() {
                break;
            }
            if visiting[current] {
                return Err(pipeline_error("comments contain cyclic parent links"));
            }
            visiting[current] = true;
            chain.push(current);
            let comment = &comments[current];
            if comment.parent_id.as_deref() == Some(post_id.as_str()) {
                depths[current] = Some(0);
                break;
            }
            if let Some(parent) = comment.parent_id.as_deref().and_then(|id| known.get(id)) {
                current = *parent;
            } else {
                // Unloaded ancestry remains explicit. A continuation's view
                // depth is only replaced when a collected parent proves it.
                depths[current] = Some(comment.depth);
                break;
            }
        }
        for index in chain.into_iter().rev() {
            let depth = if let Some(depth) = depths[index] {
                depth
            } else {
                let parent = comments[index]
                    .parent_id
                    .as_deref()
                    .and_then(|id| known.get(id))
                    .and_then(|parent| depths[*parent])
                    .ok_or_else(|| pipeline_error("comment parent depth could not be resolved"))?;
                parent
                    .checked_add(1)
                    .ok_or_else(|| pipeline_error("comment depth exceeds supported bound"))?
            };
            if depth > 256 {
                return Err(pipeline_error("comment depth exceeds supported bound"));
            }
            depths[index] = Some(depth);
            visiting[index] = false;
        }
    }
    depths
        .into_iter()
        .map(|depth| depth.ok_or_else(|| pipeline_error("comment depth could not be resolved")))
        .collect()
}

fn comment_output(
    post: &str,
    comments: &[CollectedComment],
    expected_thread: &str,
) -> CrwResult<(String, Value)> {
    // Resolve the merged graph before budgeting JSON. Later continuation
    // views can reset DOM depths and introduce previously unloaded parents.
    let depths = comment_depths(comments, expected_thread)?;
    let mut markdown = post.to_string();
    for comment in comments {
        markdown.push_str("\n\n");
        if let Some(author) = &comment.author {
            markdown.push_str("**");
            // Author metadata is plaintext, never injected as HTML or JS.
            markdown.push_str(&author.replace(['\n', '\r', '*'], " "));
            markdown.push_str("**\n\n");
        }
        markdown.push_str(&comment.markdown);
    }
    let rows: Vec<_> = comments
        .iter()
        .zip(depths)
        .map(|(comment, depth)| {
            let mut row = json!(comment);
            row["depth"] = json!(depth);
            row
        })
        .collect();
    Ok((markdown, json!({"post":{"markdown":post},"comments":rows})))
}

fn content_bytes(markdown: &str, structured: &Value) -> usize {
    markdown.len()
        + serde_json::to_vec(structured)
            .map(|bytes| bytes.len())
            .unwrap_or(usize::MAX)
}

impl BrowserPipelineClient {
    pub fn new(base_url: &str, api_key: Option<String>) -> Self {
        Self {
            browser: BrowserAdapter::new(base_url, api_key),
        }
    }

    pub async fn fetch(
        &self,
        request: &BrowserPipelineRequest,
        caller_deadline: Deadline,
    ) -> CrwResult<BrowserPipelineData> {
        request.validate()?;
        let start = Instant::now();
        let deadline = Deadline::now_plus(
            caller_deadline
                .remaining()
                .min(Duration::from_millis(request.timeout)),
        );
        if deadline.expired() {
            return Err(timeout_error(deadline));
        }
        let mut cleanup = self.browser.lease(deadline).await?;
        let user = cleanup.user_id.clone();
        let result = async {
            // Reap a prior timed-out tab before reusing this profile. This
            // scope belongs exclusively to this client and the leased slot.
            self.browser.verify_browser(deadline).await?;
            self.browser.reap_before_create(&user, deadline).await?;
            // An omitted URL creates a blank tab internally. Explicit
            // about:blank is rejected by Camofox's destination safety guard.
            let tab = self.browser.create_blank(&user, deadline).await?;
            cleanup.tab_id = Some(tab.clone());
            self.browser
                .navigate(&tab, &user, &request.url, deadline)
                .await?;
            let wait = self.browser.wait(&tab, &user, deadline).await?;
            self.collect(
                request,
                &tab,
                &user,
                deadline,
                start,
                wait.get("ready") == Some(&Value::Bool(false)),
            )
            .await
        }
        .await;
        cleanup.finish().await;
        result
    }

    async fn collect(
        &self,
        request: &BrowserPipelineRequest,
        tab: &str,
        user: &str,
        deadline: Deadline,
        start: Instant,
        not_ready: bool,
    ) -> CrwResult<BrowserPipelineData> {
        let expected_thread = Url::parse(&request.url)
            .ok()
            .and_then(|url| reddit_post_id(&url));
        let mut comments = Vec::<CollectedComment>::new();
        let mut seen = HashSet::new();
        let mut warnings = Vec::new();
        if not_ready {
            warnings.push("browser readiness was not confirmed".into());
        }
        let mut post = String::new();
        let mut title = String::new();
        let mut current_url = request.url.clone();
        let mut total = None;
        let mut rounds = 0;
        let mut stagnant = 0;
        let mut complete = false;
        let mut stop = "maxRounds";
        for round in 0..request.max_rounds {
            if deadline.expired() {
                stop = "deadline";
                break;
            }
            let expression = match request.profile {
                BrowserPipelineProfile::Article => {
                    crate::pipeline_scripts::article_snapshot(SNAPSHOT_LIMIT)
                }
                BrowserPipelineProfile::RedditThread => {
                    let seen_ids: Vec<_> = comments
                        .iter()
                        .filter_map(|comment| comment.id.clone())
                        .collect();
                    crate::pipeline_scripts::reddit_snapshot(
                        &request.url,
                        request.max_items,
                        SNAPSHOT_LIMIT,
                        &seen_ids,
                    )
                }
            };
            let snapshot: Snapshot =
                match self.browser.evaluate(tab, user, expression, deadline).await {
                    Err(CrwError::Timeout(_)) if !post.is_empty() => {
                        stop = "deadline";
                        break;
                    }
                    result => result?,
                };
            rounds += 1;
            if let Some(error) = snapshot.error {
                return Err(pipeline_error(error));
            }
            let parsed_url = Url::parse(&snapshot.url)
                .map_err(|_| pipeline_error("snapshot returned invalid URL"))?;
            crw_core::url_safety::validate_safe_url(&parsed_url).map_err(pipeline_error)?;
            current_url = snapshot.url;
            title = snapshot.title;
            if let Some(count) = snapshot.advertised_count {
                total = Some(count);
            }
            if request.profile == BrowserPipelineProfile::RedditThread {
                let expected = expected_thread
                    .as_deref()
                    .ok_or_else(|| pipeline_error("missing expected thread identity"))?;
                check_thread(&current_url, snapshot.post_id.as_deref(), expected)?;
                if !snapshot.comments_loaded {
                    warnings.push("comment container was not confirmed loaded".into());
                }
            }
            if post.is_empty() {
                post = check_content(
                    &snapshot.content_html,
                    request.profile == BrowserPipelineProfile::Article,
                )?;
            }
            if request.profile == BrowserPipelineProfile::Article {
                if post.len() > request.max_bytes {
                    return Err(pipeline_error("article exceeds maxBytes"));
                }
                complete = snapshot.complete && !snapshot.overflow && !not_ready;
                stop = if snapshot.overflow {
                    "snapshotLimit"
                } else {
                    "snapshot"
                };
                break;
            }
            let (initial_md, initial_json) = comment_output(
                &post,
                &comments,
                expected_thread.as_deref().unwrap_or_default(),
            )?;
            if content_bytes(&initial_md, &initial_json) > request.max_bytes {
                return Err(pipeline_error("post exceeds maxBytes"));
            }
            let before = comments.len();
            let mut reached_budget = false;
            for item in snapshot.items {
                let known_id = item.id.filter(|id| !id.is_empty());
                let key = match &known_id {
                    Some(id) => format!("id:{id}"),
                    None => serde_json::to_string(&json!([
                        item.parent_id,
                        item.permalink,
                        item.depth,
                        item.body_html
                    ]))
                    .map_err(|_| pipeline_error("invalid comment identity"))?,
                };
                if seen.contains(&key) {
                    continue;
                }
                if comments.len() == request.max_items {
                    stop = "maxItems";
                    reached_budget = true;
                    break;
                }
                let markdown = crw_extract::markdown::html_to_markdown(&item.body_html);
                if markdown.trim().is_empty() {
                    warnings.push("a comment had no readable body".into());
                    continue;
                }
                let missing_identity = known_id.is_none();
                comments.push(CollectedComment {
                    id: known_id,
                    parent_id: item.parent_id,
                    permalink: item.permalink,
                    author: item.author,
                    depth: item.depth,
                    markdown,
                });
                let (markdown, structured) = comment_output(
                    &post,
                    &comments,
                    expected_thread.as_deref().unwrap_or_default(),
                )?;
                if content_bytes(&markdown, &structured) > request.max_bytes {
                    comments.pop();
                    stop = "maxBytes";
                    reached_budget = true;
                    break;
                }
                seen.insert(key);
                if missing_identity {
                    warnings.push("comment identity missing; conservative deduplication may merge deleted markers".into());
                }
            }
            if reached_budget {
                break;
            }
            if comments.len() >= request.max_items {
                stop = "maxItems";
                break;
            }
            stagnant = if comments.len() == before {
                stagnant + 1
            } else {
                0
            };
            if snapshot.overflow {
                // Drain already-loaded comments beyond the compact snapshot
                // window before invoking more browser interactions.
                if comments.len() == before {
                    stop = "snapshotLimit";
                    break;
                }
                continue;
            }
            if round > 0 && snapshot.outstanding_controls == 0 && comments.len() == before {
                stop = "noControls";
                break;
            }
            if stagnant >= 2 {
                stop = "progressStalled";
                break;
            }
            if rounds == request.max_rounds {
                break;
            }
            if deadline.expired() {
                stop = "deadline";
                break;
            }
            let expansion: Expansion = match self
                .browser
                .evaluate(
                    tab,
                    user,
                    crate::pipeline_scripts::reddit_expand(&request.url),
                    deadline,
                )
                .await
            {
                Err(CrwError::Timeout(_)) => {
                    stop = "deadline";
                    break;
                }
                result => result?,
            };
            if let Some(error) = expansion.error {
                return Err(pipeline_error(error));
            }
            let expected = expected_thread.as_deref().unwrap_or_default();
            check_thread(&current_url, expansion.post_id.as_deref(), expected)?;
            if expansion.clicked == 0 && !expansion.scrolled && snapshot.outstanding_controls > 0 {
                stop = "progressStalled";
                break;
            }
            // Expansion may launch asynchronous network work. A bounded settle
            // delay prevents immediate snapshots from mistaking it for a stall.
            tokio::time::sleep(Duration::from_millis(200).min(deadline.remaining())).await;
        }
        if post.is_empty() {
            return Err(timeout_error(deadline));
        }
        let (markdown, structured) = if request.profile == BrowserPipelineProfile::RedditThread {
            warnings.push(
                "thread completeness cannot be established from loaded browser comments".into(),
            );
            let (markdown, structured) = comment_output(
                &post,
                &comments,
                expected_thread.as_deref().unwrap_or_default(),
            )?;
            (markdown, Some(structured))
        } else {
            (post, None)
        };
        if !complete {
            warnings.push(format!("partial browser result: {stop}"));
        }
        warnings.sort();
        warnings.dedup();
        Ok(BrowserPipelineData {
            markdown,
            json: structured,
            warnings,
            metadata: BrowserPipelineMetadata {
                pipeline: "browser-v1".into(),
                profile: request.profile,
                source_url: request.url.clone(),
                url: current_url,
                title,
                rendered_with: "camofox".into(),
                items_collected: comments.len(),
                reported_total: total,
                complete,
                stop_reason: stop.into(),
                rounds,
                elapsed_ms: start.elapsed().as_millis() as u64,
            },
        })
    }
}
