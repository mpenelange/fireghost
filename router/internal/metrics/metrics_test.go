package metrics_test

import (
	"strings"
	"testing"
	"time"

	"web-retrieval/internal/metrics"
)

func TestRegistryFormatsPrometheusTextWithFixedLabels(t *testing.T) {
	registry := metrics.NewRegistry()
	registry.IncRequest(metrics.EndpointSearch, metrics.Status2xx)
	registry.IncCacheHit(metrics.EndpointSearch)
	registry.IncLocalAttempt(metrics.EndpointSearch)
	registry.IncCloudAttempt(metrics.EndpointSearch)
	registry.IncCloudBudgetDenied(metrics.EndpointSearch)
	registry.IncUpstreamFailure(metrics.EndpointSearch, metrics.UpstreamLocal)
	registry.ObserveRequestDuration(metrics.EndpointSearch, 1500*time.Millisecond)

	text := registry.PrometheusText()
	for _, want := range []string{
		`web_retrieval_requests_total{endpoint="search",status_class="2xx"} 1`,
		`web_retrieval_cache_hits_total{endpoint="search"} 1`,
		`web_retrieval_local_attempts_total{endpoint="search"} 1`,
		`web_retrieval_cloud_attempts_total{endpoint="search"} 1`,
		`web_retrieval_cloud_budget_denied_total{endpoint="search"} 1`,
		`web_retrieval_upstream_failures_total{endpoint="search",upstream="local"} 1`,
		`web_retrieval_request_duration_seconds_count{endpoint="search"} 1`,
		`web_retrieval_request_duration_seconds_sum{endpoint="search"} 1.5`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("metrics output missing %q:\n%s", want, text)
		}
	}
}

func TestRegistryReportsMCPRequestsAndDuration(t *testing.T) {
	registry := metrics.NewRegistry()
	registry.IncRequest(metrics.EndpointMCP, metrics.Status2xx)
	registry.ObserveRequestDuration(metrics.EndpointMCP, 250*time.Millisecond)
	text := registry.PrometheusText()
	for _, want := range []string{
		`web_retrieval_requests_total{endpoint="mcp",status_class="2xx"} 1`,
		`web_retrieval_request_duration_seconds_count{endpoint="mcp"} 1`,
		`web_retrieval_request_duration_seconds_sum{endpoint="mcp"} 0.25`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("metrics output missing %q:\n%s", want, text)
		}
	}
}
