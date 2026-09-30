//! Bounded browser extraction, separate from generic full-DOM fetching.

use crw_core::Deadline;
use crw_core::error::{CrwError, CrwResult};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::collections::HashSet;
use std::sync::{Arc, LazyLock, Mutex};
use std::time::{Duration, Instant};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use url::Url;

const SNAPSHOT_LIMIT: usize = 96 * 1024;
const RESPONSE_LIMIT: usize = 256 * 1024;
const CLEANUP_BUDGET: Duration = Duration::from_millis(250);
static PIPELINE_SLOTS: LazyLock<Arc<Semaphore>> = LazyLock::new(|| Arc::new(Semaphore::new(2)));
static AVAILABLE_USERS: LazyLock<Mutex<Vec<usize>>> = LazyLock::new(|| Mutex::new(vec![0, 1]));

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
    http: reqwest::Client,
    base_url: String,
    api_key: Option<String>,
    namespace: String,
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

#[derive(Deserialize)]
struct Evaluation {
    result: Value,
    #[serde(default)]
    truncated: bool,
    #[serde(default)]
    ok: Option<bool>,
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

fn comment_output(post: &str, comments: &[CollectedComment]) -> (String, Value) {
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
    (
        markdown,
        json!({"post":{"markdown":post},"comments":comments}),
    )
}

fn content_bytes(markdown: &str, structured: &Value) -> usize {
    markdown.len()
        + serde_json::to_vec(structured)
            .map(|bytes| bytes.len())
            .unwrap_or(usize::MAX)
}

/// Owns cleanup even if the caller cancels the extraction future.
struct TabCleanup {
    client: BrowserPipelineClient,
    user_id: String,
    tab_id: Option<String>,
    slot: usize,
    permit: Option<OwnedSemaphorePermit>,
}

impl TabCleanup {
    async fn finish(&mut self) {
        self.client
            .cleanup(&self.user_id, self.tab_id.as_deref())
            .await;
        AVAILABLE_USERS
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .push(self.slot);
        self.permit.take();
    }
}

impl Drop for TabCleanup {
    fn drop(&mut self) {
        if let Some(permit) = self.permit.take()
            && let Ok(runtime) = tokio::runtime::Handle::try_current()
        {
            let client = self.client.clone();
            let user = self.user_id.clone();
            let tab = self.tab_id.clone();
            let slot = self.slot;
            runtime.spawn(async move {
                client.cleanup(&user, tab.as_deref()).await;
                AVAILABLE_USERS
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner())
                    .push(slot);
                drop(permit);
            });
        }
    }
}

impl BrowserPipelineClient {
    pub fn new(base_url: &str, api_key: Option<String>) -> Self {
        Self {
            http: reqwest::Client::new(),
            base_url: base_url.trim_end_matches('/').into(),
            api_key,
            namespace: format!("{:032x}", rand::random::<u128>()),
        }
    }

    fn auth(&self, request: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        match &self.api_key {
            Some(key) => request.bearer_auth(key),
            None => request,
        }
    }

    async fn post<T: serde::de::DeserializeOwned>(
        &self,
        path: &str,
        body: Value,
        deadline: Deadline,
    ) -> CrwResult<T> {
        self.decode(
            self.http
                .post(format!("{}{path}", self.base_url))
                .json(&body),
            deadline,
        )
        .await
    }

    async fn get<T: serde::de::DeserializeOwned>(
        &self,
        path: &str,
        deadline: Deadline,
    ) -> CrwResult<T> {
        self.decode(self.http.get(format!("{}{path}", self.base_url)), deadline)
            .await
    }

    async fn decode<T: serde::de::DeserializeOwned>(
        &self,
        request: reqwest::RequestBuilder,
        deadline: Deadline,
    ) -> CrwResult<T> {
        let budget = deadline.remaining();
        if budget.is_zero() {
            return Err(timeout_error(deadline));
        }
        let operation = async {
            let mut response = self.auth(request).send().await.map_err(|error| {
                pipeline_error(format!("request failed: {}", error.without_url()))
            })?;
            if !response.status().is_success() {
                return Err(pipeline_error(format!(
                    "browser API returned {}",
                    response.status()
                )));
            }
            if response
                .content_length()
                .is_some_and(|length| length > RESPONSE_LIMIT as u64)
            {
                return Err(pipeline_error("response exceeds bounded envelope"));
            }
            let mut bytes = Vec::new();
            while let Some(chunk) = response
                .chunk()
                .await
                .map_err(|error| pipeline_error(error.without_url().to_string()))?
            {
                if chunk.len() > RESPONSE_LIMIT.saturating_sub(bytes.len()) {
                    return Err(pipeline_error("response exceeds bounded envelope"));
                }
                bytes.extend_from_slice(&chunk);
            }
            serde_json::from_slice(&bytes).map_err(|_| pipeline_error("malformed browser response"))
        };
        tokio::time::timeout(budget, operation)
            .await
            .map_err(|_| timeout_error(deadline))?
    }

    async fn evaluate<T: serde::de::DeserializeOwned>(
        &self,
        tab: &str,
        user: &str,
        expression: String,
        deadline: Deadline,
    ) -> CrwResult<T> {
        let response: Evaluation = self.post(&format!("/tabs/{tab}/evaluate"), json!({"userId":user,"expression":expression,"timeout":deadline.remaining().as_millis().clamp(1,5000) as u64}), deadline).await?;
        if response.truncated || response.ok == Some(false) {
            return Err(pipeline_error("evaluation was truncated or unsuccessful"));
        }
        let text = match response.result {
            Value::String(text) => text,
            value @ Value::Object(_) => {
                serde_json::to_string(&value).map_err(|_| pipeline_error("malformed snapshot"))?
            }
            _ => return Err(pipeline_error("snapshot must be a JSON object")),
        };
        if text.len() > SNAPSHOT_LIMIT {
            return Err(pipeline_error("snapshot exceeds 96 KiB limit"));
        }
        serde_json::from_str(&text).map_err(|_| pipeline_error("malformed snapshot"))
    }

    async fn cleanup(&self, user: &str, tab: Option<&str>) {
        let cleanup = async {
            let ids = if let Some(tab) = tab {
                vec![tab.to_string()]
            } else {
                // The create response may have timed out after the browser opened
                // a blank tab. A unique user scope prevents touching another job.
                let Ok(mut scoped_url) = Url::parse(&format!("{}/tabs", self.base_url)) else {
                    return;
                };
                scoped_url.query_pairs_mut().append_pair("userId", user);
                let response = self.auth(self.http.get(scoped_url)).send().await.ok();
                let Some(mut response) = response.filter(|response| response.status().is_success())
                else {
                    return;
                };
                let mut bytes = Vec::new();
                while let Ok(Some(chunk)) = response.chunk().await {
                    if chunk.len() > RESPONSE_LIMIT.saturating_sub(bytes.len()) {
                        return;
                    }
                    bytes.extend_from_slice(&chunk);
                }
                let Ok(value) = serde_json::from_slice::<Value>(&bytes) else {
                    return;
                };
                value
                    .get("tabs")
                    .and_then(Value::as_array)
                    .into_iter()
                    .flatten()
                    .take(4)
                    .filter_map(|tab| {
                        tab.get("tabId")
                            .or_else(|| tab.get("id"))
                            .and_then(Value::as_str)
                    })
                    .filter(|id| safe_tab_id(id))
                    .map(str::to_string)
                    .collect()
            };
            for id in ids {
                let _ = self
                    .auth(self.http.delete(format!("{}/tabs/{id}", self.base_url)))
                    .json(&json!({"userId":user}))
                    .send()
                    .await;
            }
        };
        let _ = tokio::time::timeout(CLEANUP_BUDGET, cleanup).await;
    }

    async fn verify_browser(&self, deadline: Deadline) -> CrwResult<()> {
        let health: Value = self.get("/health", deadline).await?;
        let version = health
            .get("version")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let components: Option<Vec<u32>> = version
            .trim_start_matches('v')
            .split('.')
            .map(|part| part.parse().ok())
            .collect();
        if !components
            .is_some_and(|parts| parts.len() == 3 && (parts[0], parts[1], parts[2]) >= (2, 4, 8))
        {
            return Err(pipeline_error(
                "guarded browser pipeline requires Camofox 2.4.8 or later",
            ));
        }
        Ok(())
    }

    async fn reap_before_create(&self, user: &str, deadline: Deadline) -> CrwResult<()> {
        let deadline = Deadline::now_plus(deadline.remaining().min(Duration::from_secs(2)));
        let path = format!("/tabs?userId={user}");
        let listed: Value = self.get(&path, deadline).await?;
        let tabs = listed
            .get("tabs")
            .and_then(Value::as_array)
            .ok_or_else(|| pipeline_error("cannot confirm scoped tab ownership"))?;
        if tabs.len() > 16 {
            return Err(pipeline_error("too many stale scoped tabs to reap safely"));
        }
        for tab in tabs {
            let id = tab
                .get("tabId")
                .or_else(|| tab.get("id"))
                .and_then(Value::as_str)
                .filter(|id| safe_tab_id(id))
                .ok_or_else(|| pipeline_error("invalid scoped stale tab ID"))?;
            if tab
                .get("userId")
                .and_then(Value::as_str)
                .is_some_and(|owner| owner != user)
            {
                return Err(pipeline_error("tab listing included a foreign user scope"));
            }
            let deleted: Value = self
                .decode(
                    self.http
                        .delete(format!("{}/tabs/{id}", self.base_url))
                        .json(&json!({"userId":user})),
                    deadline,
                )
                .await?;
            if deleted.get("ok") == Some(&Value::Bool(false)) {
                return Err(pipeline_error("stale scoped tab could not be deleted"));
            }
        }
        let verified: Value = self.get(&path, deadline).await?;
        if verified
            .get("tabs")
            .and_then(Value::as_array)
            .is_none_or(|tabs| !tabs.is_empty())
        {
            return Err(pipeline_error(
                "scoped tabs remain; refusing to open another tab",
            ));
        }
        Ok(())
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
        let permit =
            tokio::time::timeout(deadline.remaining(), PIPELINE_SLOTS.clone().acquire_owned())
                .await
                .map_err(|_| timeout_error(deadline))?
                .map_err(|_| pipeline_error("pipeline limiter closed"))?;
        let slot = AVAILABLE_USERS
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .pop()
            .ok_or_else(|| pipeline_error("pipeline user pool exhausted"))?;
        // Reuse exactly two isolated profiles. The lease remains held through
        // cleanup, including cleanup scheduled when this future is cancelled.
        let user = format!("crw-pipeline-{}-{slot}", self.namespace);
        let mut cleanup = TabCleanup {
            client: self.clone(),
            user_id: user.clone(),
            tab_id: None,
            slot,
            permit: Some(permit),
        };
        let result = async {
            // Reap a prior timed-out tab before reusing this profile. This
            // scope belongs exclusively to this client and the leased slot.
            self.verify_browser(deadline).await?;
            self.reap_before_create(&user, deadline).await?;
            let create: Value = self.post("/tabs", json!({"userId":user,"sessionKey":"pipeline","url":"about:blank"}), deadline).await?;
            let tab = create.get("tabId").and_then(Value::as_str).filter(|id| safe_tab_id(id))
                .ok_or_else(|| pipeline_error("create did not return a valid tab ID"))?.to_string();
            cleanup.tab_id = Some(tab.clone());
            let navigation: Value = self.post(&format!("/tabs/{tab}/navigate"), json!({"userId":user,"url":request.url}), deadline).await?;
            if navigation.get("ok") == Some(&Value::Bool(false)) { return Err(pipeline_error("navigation failed")); }
            let wait: Value = self.post(&format!("/tabs/{tab}/wait"), json!({"userId":user,"timeout":deadline.remaining().as_millis().min(2000) as u64}), deadline).await?;
            if wait.get("ok") == Some(&Value::Bool(false)) { return Err(pipeline_error("readiness wait failed")); }
            self.collect(request, &tab, &user, deadline, start, wait.get("ready") == Some(&Value::Bool(false))).await
        }.await;
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
            let snapshot: Snapshot = match self.evaluate(tab, user, expression, deadline).await {
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
            let (initial_md, initial_json) = comment_output(&post, &comments);
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
                let (markdown, structured) = comment_output(&post, &comments);
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
            let (markdown, structured) = comment_output(&post, &comments);
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

fn safe_tab_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
}
