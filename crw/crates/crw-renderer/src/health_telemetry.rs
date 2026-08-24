//! Process-wide health telemetry sampler. Spawns once on the first
//! `FallbackRenderer::new` call. Every 60s emits a structured tracing log
//! with snapshot of live CDP connections, pending requests, and event
//! subscribers — enough to spot accumulation without per-fetch overhead.
//!
//! Cheap by design: the registry insert is one Mutex push per `connect`,
//! and the sampler reads via `Weak::upgrade()` so it never extends the
//! lifetime of a connection that's already shutting down.

use std::sync::OnceLock;
use std::time::Duration;

use crate::cdp_conn::snapshot_live_conns;

const SAMPLE_INTERVAL: Duration = Duration::from_secs(60);

static STARTED: OnceLock<()> = OnceLock::new();

/// Spawn the sampler once per process. Subsequent calls are no-ops.
pub fn spawn_once() {
    // No runtime ⇒ silently skip (tests construct FallbackRenderer outside one).
    if tokio::runtime::Handle::try_current().is_err() {
        return;
    }
    if STARTED.set(()).is_err() {
        return;
    }
    tokio::spawn(async move {
        let mut tick = tokio::time::interval(SAMPLE_INTERVAL);
        // Skip the immediate-fire first tick — registry is empty at startup.
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        tick.tick().await;
        loop {
            tick.tick().await;
            let (live, pending, subs) = snapshot_live_conns();
            let m = crw_core::metrics::metrics();
            m.cdp_live_connections.set(live as i64);
            m.cdp_pending_requests.set(pending as i64);
            tracing::info!(
                cdp_live = live,
                pending_total = pending,
                subscribers_total = subs,
                "cdp_telemetry"
            );
        }
    });
}
