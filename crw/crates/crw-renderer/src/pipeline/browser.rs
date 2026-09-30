//! Camofox HTTP and tab-lifecycle compatibility boundary for browser pipelines.
//!
//! The supported guarded contract is Camofox 2.4.8 or later. `/health` supplies
//! its version; browser connectivity may be false before lazy startup. Scoped
//! `/tabs?userId=...` listing and tab deletion verify cleanup before new work.
//!
//! `POST /tabs` omits `url` to create a blank tab: an explicit `about:blank` is
//! rejected by the destination guard. Navigation then uses `/tabs/:id/navigate`.
//! A create HTTP 5xx may follow Firefox's last-tab context closure; an awaited,
//! acknowledged `DELETE /sessions/:userId` permits exactly one scoped retry.
//! Transport failures and timeouts are never retried here.
//!
//! Wait readiness and evaluation stay separate. `/evaluate` accepts only
//! server-owned extraction expressions and returns an object or JSON string;
//! truncated results are rejected. Both response reading and decoding are
//! bounded, and evaluation supplies the upstream operation timeout.
//!
//! Two exclusive profile leases are retained through best-effort 250 ms cleanup,
//! including cancellation. These checks and quirks belong here so browser image
//! upgrades can be assessed without changing profile extraction or collection.

use super::{pipeline_error, timeout_error};
use crw_core::Deadline;
use crw_core::error::CrwResult;
use serde::Deserialize;
use serde_json::{Value, json};
use std::sync::{Arc, LazyLock, Mutex};
use std::time::Duration;
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use url::Url;

pub(super) const SNAPSHOT_LIMIT: usize = 96 * 1024;
const RESPONSE_LIMIT: usize = 256 * 1024;
const CLEANUP_BUDGET: Duration = Duration::from_millis(250);
static PIPELINE_SLOTS: LazyLock<Arc<Semaphore>> = LazyLock::new(|| Arc::new(Semaphore::new(2)));
static AVAILABLE_USERS: LazyLock<Mutex<Vec<usize>>> = LazyLock::new(|| Mutex::new(vec![0, 1]));

#[derive(Clone)]
pub(super) struct BrowserAdapter {
    http: reqwest::Client,
    base_url: String,
    api_key: Option<String>,
    namespace: String,
}

#[derive(Deserialize)]
struct Evaluation {
    result: Value,
    #[serde(default)]
    truncated: bool,
    #[serde(default)]
    ok: Option<bool>,
}

/// Owns cleanup even if the caller cancels the extraction future.
pub(super) struct TabCleanup {
    client: BrowserAdapter,
    pub(super) user_id: String,
    pub(super) tab_id: Option<String>,
    slot: usize,
    permit: Option<OwnedSemaphorePermit>,
}

impl TabCleanup {
    pub(super) async fn finish(&mut self) {
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

impl BrowserAdapter {
    pub(super) fn new(base_url: &str, api_key: Option<String>) -> Self {
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
        let (status, bytes) = self.response(request, deadline).await?;
        let value = Self::decode_response(status, &bytes)?;
        if deadline.expired() {
            return Err(timeout_error(deadline));
        }
        Ok(value)
    }

    fn decode_response<T: serde::de::DeserializeOwned>(
        status: reqwest::StatusCode,
        bytes: &[u8],
    ) -> CrwResult<T> {
        if !status.is_success() {
            return Err(pipeline_error(format!("browser API returned {status}")));
        }
        serde_json::from_slice(bytes).map_err(|_| pipeline_error("malformed browser response"))
    }

    async fn response(
        &self,
        request: reqwest::RequestBuilder,
        deadline: Deadline,
    ) -> CrwResult<(reqwest::StatusCode, Vec<u8>)> {
        let budget = deadline.remaining();
        if budget.is_zero() {
            return Err(timeout_error(deadline));
        }
        let operation = async {
            let mut response = self.auth(request).send().await.map_err(|error| {
                pipeline_error(format!("request failed: {}", error.without_url()))
            })?;
            let status = response.status();
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
            Ok((status, bytes))
        };
        tokio::time::timeout(budget, operation)
            .await
            .map_err(|_| timeout_error(deadline))?
    }

    pub(super) async fn create_blank(&self, user: &str, deadline: Deadline) -> CrwResult<String> {
        let create = || {
            self.http
                .post(format!("{}/tabs", self.base_url))
                .json(&json!({"userId":user,"sessionKey":"pipeline"}))
        };
        let mut response = self.response(create(), deadline).await?;
        if response.0.is_server_error() {
            // Firefox can close the last-tab context just after tab deletion.
            // Reset only this exclusively leased profile, await its teardown,
            // then retry blank creation once under the original deadline.
            let reset: Value = self
                .decode(
                    self.http
                        .delete(format!("{}/sessions/{user}", self.base_url))
                        .json(&json!({"userId":user})),
                    deadline,
                )
                .await?;
            if reset.get("ok") != Some(&Value::Bool(true)) {
                return Err(pipeline_error("scoped session reset failed"));
            }
            response = self.response(create(), deadline).await?;
        }
        if deadline.expired() {
            return Err(timeout_error(deadline));
        }
        let create: Value = Self::decode_response(response.0, &response.1)?;
        create
            .get("tabId")
            .and_then(Value::as_str)
            .filter(|id| safe_tab_id(id))
            .ok_or_else(|| pipeline_error("create did not return a valid tab ID"))
            .map(str::to_string)
    }

    pub(super) async fn evaluate<T: serde::de::DeserializeOwned>(
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

    pub(super) async fn verify_browser(&self, deadline: Deadline) -> CrwResult<()> {
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

    pub(super) async fn reap_before_create(&self, user: &str, deadline: Deadline) -> CrwResult<()> {
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

    pub(super) async fn lease(&self, deadline: Deadline) -> CrwResult<TabCleanup> {
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
        Ok(TabCleanup {
            client: self.clone(),
            user_id: user.clone(),
            tab_id: None,
            slot,
            permit: Some(permit),
        })
    }

    pub(super) async fn navigate(
        &self,
        tab: &str,
        user: &str,
        url: &str,
        deadline: Deadline,
    ) -> CrwResult<()> {
        let navigation: Value = self
            .post(
                &format!("/tabs/{tab}/navigate"),
                json!({"userId":user,"url":url}),
                deadline,
            )
            .await?;
        if navigation.get("ok") == Some(&Value::Bool(false)) {
            return Err(pipeline_error("navigation failed"));
        }
        Ok(())
    }

    pub(super) async fn wait(&self, tab: &str, user: &str, deadline: Deadline) -> CrwResult<Value> {
        let wait: Value = self
            .post(
                &format!("/tabs/{tab}/wait"),
                json!({"userId":user,"timeout":deadline.remaining().as_millis().min(2000) as u64}),
                deadline,
            )
            .await?;
        if wait.get("ok") == Some(&Value::Bool(false)) {
            return Err(pipeline_error("readiness wait failed"));
        }
        Ok(wait)
    }
}

fn safe_tab_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
}
