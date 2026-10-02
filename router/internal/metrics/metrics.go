// Package metrics provides bounded, dependency-free Prometheus counters.
package metrics

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Endpoint uint8

const (
	EndpointSearch Endpoint = iota
	EndpointScrape
	EndpointHealth
	EndpointMetrics
	EndpointMCP
	EndpointLocalPassthrough
	EndpointCloudPassthrough
	endpointCount
)

type StatusClass uint8

const (
	Status2xx StatusClass = iota
	Status3xx
	Status4xx
	Status5xx
	statusCount
)

type Upstream uint8

const (
	UpstreamLocal Upstream = iota
	UpstreamCloud
	upstreamCount
)

var endpointNames = [...]string{"search", "scrape", "health", "metrics", "mcp", "local_passthrough", "cloud_passthrough"}
var statusNames = [...]string{"2xx", "3xx", "4xx", "5xx"}
var upstreamNames = [...]string{"local", "cloud"}

type Registry struct {
	requests          [endpointCount][statusCount]atomic.Uint64
	cacheHits         [endpointCount]atomic.Uint64
	localAttempts     [endpointCount]atomic.Uint64
	cloudAttempts     [endpointCount]atomic.Uint64
	cloudBudgetDenied [endpointCount]atomic.Uint64
	upstreamFailures  [endpointCount][upstreamCount]atomic.Uint64
	durationCount     [endpointCount]atomic.Uint64
	durationNanos     [endpointCount]atomic.Uint64
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) IncRequest(e Endpoint, s StatusClass) { r.requests[e][s].Add(1) }
func (r *Registry) IncCacheHit(e Endpoint)               { r.cacheHits[e].Add(1) }
func (r *Registry) IncLocalAttempt(e Endpoint)           { r.localAttempts[e].Add(1) }
func (r *Registry) IncCloudAttempt(e Endpoint)           { r.cloudAttempts[e].Add(1) }
func (r *Registry) IncCloudBudgetDenied(e Endpoint)      { r.cloudBudgetDenied[e].Add(1) }
func (r *Registry) IncUpstreamFailure(e Endpoint, u Upstream) {
	r.upstreamFailures[e][u].Add(1)
}
func (r *Registry) ObserveRequestDuration(e Endpoint, d time.Duration) {
	r.durationCount[e].Add(1)
	if d > 0 {
		r.durationNanos[e].Add(uint64(d))
	}
}

func (r *Registry) PrometheusText() string {
	var b strings.Builder
	writeHelpType(&b, "web_retrieval_requests_total", "Router requests by endpoint and HTTP status class", "counter")
	for e := Endpoint(0); e < endpointCount; e++ {
		for s := StatusClass(0); s < statusCount; s++ {
			writeSample(&b, "web_retrieval_requests_total{endpoint=\""+endpointNames[e]+"\",status_class=\""+statusNames[s]+"\"}", r.requests[e][s].Load())
		}
	}
	writeEndpointCounters(&b, "web_retrieval_cache_hits_total", "Cache hits", &r.cacheHits)
	writeEndpointCounters(&b, "web_retrieval_local_attempts_total", "Local upstream attempts", &r.localAttempts)
	writeEndpointCounters(&b, "web_retrieval_cloud_attempts_total", "Cloud upstream attempts", &r.cloudAttempts)
	writeEndpointCounters(&b, "web_retrieval_cloud_budget_denied_total", "Cloud attempts denied by budget", &r.cloudBudgetDenied)
	writeHelpType(&b, "web_retrieval_upstream_failures_total", "Upstream failures", "counter")
	for e := Endpoint(0); e < endpointCount; e++ {
		for u := Upstream(0); u < upstreamCount; u++ {
			writeSample(&b, "web_retrieval_upstream_failures_total{endpoint=\""+endpointNames[e]+"\",upstream=\""+upstreamNames[u]+"\"}", r.upstreamFailures[e][u].Load())
		}
	}
	writeHelpType(&b, "web_retrieval_request_duration_seconds", "Router request duration in seconds", "summary")
	for e := Endpoint(0); e < endpointCount; e++ {
		labels := "{endpoint=\"" + endpointNames[e] + "\"}"
		writeSample(&b, "web_retrieval_request_duration_seconds_count"+labels, r.durationCount[e].Load())
		b.WriteString("web_retrieval_request_duration_seconds_sum" + labels + " ")
		b.WriteString(strconv.FormatFloat(float64(r.durationNanos[e].Load())/float64(time.Second), 'g', -1, 64))
		b.WriteByte('\n')
	}
	return b.String()
}

func writeEndpointCounters(b *strings.Builder, name, help string, values *[endpointCount]atomic.Uint64) {
	writeHelpType(b, name, help, "counter")
	for e := Endpoint(0); e < endpointCount; e++ {
		writeSample(b, name+"{endpoint=\""+endpointNames[e]+"\"}", values[e].Load())
	}
}

func writeHelpType(b *strings.Builder, name, help, metricType string) {
	b.WriteString("# HELP " + name + " " + help + "\n# TYPE " + name + " " + metricType + "\n")
}

func writeSample(b *strings.Builder, name string, value uint64) {
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(value, 10))
	b.WriteByte('\n')
}
