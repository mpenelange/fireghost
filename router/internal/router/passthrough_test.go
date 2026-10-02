package router_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	creditspkg "web-retrieval/internal/credits"
	metricspkg "web-retrieval/internal/metrics"
	"web-retrieval/internal/router"
	"web-retrieval/internal/upstream"
)

type recordedRequest struct {
	Method, Path, Query, Authorization, ContentType, Body string
	Host, ForwardedProto                                  string
}

// recordingUpstream records every request and answers credit-usage with a
// configurable balance (or status) and everything else with a fixed reply.
type recordingUpstream struct {
	mu            sync.Mutex
	requests      []recordedRequest
	remaining     int
	balanceStatus int
	server        *httptest.Server
}

func newRecordingUpstream(t *testing.T, remaining int) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{remaining: remaining, balanceStatus: http.StatusOK}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(body), r.Host, r.Header.Get("X-Forwarded-Proto")})
		remaining, status := u.remaining, u.balanceStatus
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/team/credit-usage" {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"success":true,"data":{"remainingCredits":%d,"planCredits":500}}`, remaining)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"success":true,"echo":%q}`, r.Method+" "+r.URL.Path)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *recordingUpstream) recorded() []recordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recordedRequest(nil), u.requests...)
}

func (u *recordingUpstream) paths() []string {
	var paths []string
	for _, request := range u.recorded() {
		paths = append(paths, request.Method+" "+request.Path)
	}
	return paths
}

type passthroughFixture struct {
	local, cloud *recordingUpstream
	metrics      *metricspkg.Registry
	handler      http.Handler
}

func newPassthroughFixture(t *testing.T, remaining int, configure func(*router.Config)) passthroughFixture {
	t.Helper()
	local := newRecordingUpstream(t, 0)
	cloud := newRecordingUpstream(t, remaining)
	config := router.Config{
		LocalBaseURL: local.server.URL, CloudBaseURL: cloud.server.URL, CloudAPIKey: "cloud-key",
		MaxRequestBytes: 1 << 10, MaxParseBytes: 1 << 20, MaxResponseBytes: 1 << 20,
	}
	if configure != nil {
		configure(&config)
	}
	metrics := metricspkg.NewRegistry()
	floor := creditspkg.NewFloor(upstream.NewAuthenticated(cloud.server.URL, nil, "cloud-key"), 50)
	handler := router.NewHandler(config, router.Dependencies{CreditFloor: floor, Metrics: metrics})
	return passthroughFixture{local: local, cloud: cloud, metrics: metrics, handler: handler}
}

func (f passthroughFixture) do(t *testing.T, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func TestLocalRoutesGoOnlyToCRW(t *testing.T) {
	f := newPassthroughFixture(t, 1000, nil)
	cases := []struct{ method, target string }{
		{http.MethodPost, "/v2/map"},
		{http.MethodPost, "/v2/crawl"},
		{http.MethodGet, "/v2/crawl/active"},
		{http.MethodGet, "/v2/crawl/job-1?next=2"},
		{http.MethodDelete, "/v2/crawl/job-1"},
		{http.MethodGet, "/v2/crawl/job-1/errors"},
		{http.MethodPost, "/v2/batch/scrape"},
		{http.MethodGet, "/v2/batch/scrape/job-1"},
		{http.MethodDelete, "/v2/batch/scrape/job-1"},
		{http.MethodGet, "/v2/batch/scrape/job-1/errors"},
		{http.MethodPost, "/v2/extract"},
		{http.MethodGet, "/v2/extract/job-1"},
		{http.MethodGet, "/v2/scrape/job-1"},
	}
	for _, c := range cases {
		body := ""
		if c.method == http.MethodPost {
			body = `{"url":"https://example.com"}`
		}
		recorder := f.do(t, c.method, c.target, body)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("%s %s status = %d, want upstream 201", c.method, c.target, recorder.Code)
		}
	}
	recorded := f.local.recorded()
	if len(recorded) != len(cases) {
		t.Fatalf("local requests = %d, want %d", len(recorded), len(cases))
	}
	if got := recorded[3]; got.Path != "/v2/crawl/job-1" || got.Query != "next=2" {
		t.Fatalf("forwarded %s?%s, want path and query preserved", got.Path, got.Query)
	}
	if got := recorded[0]; got.Body != `{"url":"https://example.com"}` || got.ContentType != "application/json" || got.Authorization != "" {
		t.Fatalf("forwarded request = %+v, want body and content type without credentials", got)
	}
	if paths := f.cloud.paths(); len(paths) != 0 {
		t.Fatalf("cloud requests = %v, want none", paths)
	}
}

func TestLocalRoutesCarryClientHostSoCRWBuildsPublicURLs(t *testing.T) {
	f := newPassthroughFixture(t, 1000, nil)
	request := httptest.NewRequest(http.MethodGet, "/v2/crawl/job-1", nil)
	request.Host = "web-scrape.example.test"
	request.Header.Set("X-Forwarded-Proto", "https")
	f.handler.ServeHTTP(httptest.NewRecorder(), request)
	request = httptest.NewRequest(http.MethodGet, "/v2/agent/job-1", nil)
	request.Host = "web-scrape.example.test"
	request.Header.Set("X-Forwarded-Proto", "https")
	f.handler.ServeHTTP(httptest.NewRecorder(), request)
	if got := f.local.recorded(); len(got) != 1 || got[0].Host != "web-scrape.example.test" || got[0].ForwardedProto != "https" {
		t.Fatalf("local request = %+v, want client host and https", got)
	}
	cloudHost := strings.TrimPrefix(f.cloud.server.URL, "http://")
	if got := f.cloud.recorded(); len(got) != 1 || got[0].Host != cloudHost || got[0].ForwardedProto != "" {
		t.Fatalf("cloud request = %+v, want upstream host without forwarded scheme", got)
	}
}

func TestLocalRouteFailureNeverFallsBackToCloud(t *testing.T) {
	f := newPassthroughFixture(t, 1000, nil)
	f.local.server.Close()
	recorder := f.do(t, http.MethodPost, "/v2/map", `{"url":"https://example.com"}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}
	if paths := f.cloud.paths(); len(paths) != 0 {
		t.Fatalf("cloud requests = %v, want none", paths)
	}
}

func TestParseAllowsLargerBodiesThanOtherRoutes(t *testing.T) {
	f := newPassthroughFixture(t, 1000, nil)
	large := strings.Repeat("x", 4<<10)
	if recorder := f.do(t, http.MethodPost, "/v2/parse", large); recorder.Code != http.StatusCreated {
		t.Fatalf("parse status = %d, want 201", recorder.Code)
	}
	if recorder := f.do(t, http.MethodPost, "/v2/map", large); recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("map status = %d, want 413", recorder.Code)
	}
	if got := f.local.recorded(); len(got) != 1 || len(got[0].Body) != len(large) {
		t.Fatalf("local requests = %d, want only the complete parse upload", len(got))
	}
	if !strings.Contains(f.metrics.PrometheusText(), `web_retrieval_local_attempts_total{endpoint="local_passthrough"} 1`) {
		t.Fatal("rejected oversized body was counted as a local attempt")
	}
}

func TestCloudRoutesUseCloudKeyAndCheckFloorBeforeBillableRequests(t *testing.T) {
	f := newPassthroughFixture(t, 1000, nil)
	if recorder := f.do(t, http.MethodPost, "/v2/agent", `{"prompt":"find prices"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201", recorder.Code)
	}
	if recorder := f.do(t, http.MethodGet, "/v2/agent/job-1", ""); recorder.Code != http.StatusCreated {
		t.Fatalf("GET status = %d, want 201", recorder.Code)
	}
	if recorder := f.do(t, http.MethodDelete, "/v2/agent/job-1", ""); recorder.Code != http.StatusCreated {
		t.Fatalf("DELETE status = %d, want 201", recorder.Code)
	}
	want := []string{"GET /v2/team/credit-usage", "POST /v2/agent", "GET /v2/agent/job-1", "DELETE /v2/agent/job-1"}
	if got := f.cloud.paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("cloud requests = %v, want %v", got, want)
	}
	for _, request := range f.cloud.recorded() {
		if request.Authorization != "Bearer cloud-key" {
			t.Fatalf("%s %s authorization = %q, want cloud key", request.Method, request.Path, request.Authorization)
		}
	}
	if len(f.local.recorded()) != 0 {
		t.Fatal("cloud route reached CRW")
	}
}

func TestCloudFloorRereadsBalanceForEveryBillableRequest(t *testing.T) {
	f := newPassthroughFixture(t, 1000, nil)
	f.do(t, http.MethodPost, "/v2/interact", `{}`)
	f.cloud.mu.Lock()
	f.cloud.remaining = 50
	f.cloud.mu.Unlock()
	recorder := f.do(t, http.MethodPost, "/v2/interact", `{}`)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "floor") {
		t.Fatalf("status = %d body = %s, want 503 floor denial", recorder.Code, recorder.Body.String())
	}
	want := []string{"GET /v2/team/credit-usage", "POST /v2/interact", "GET /v2/team/credit-usage"}
	if got := f.cloud.paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("cloud requests = %v, want %v", got, want)
	}
	if !strings.Contains(f.metrics.PrometheusText(), `web_retrieval_cloud_budget_denied_total{endpoint="cloud_passthrough"} 1`) {
		t.Fatal("floor denial was not counted")
	}
}

func TestCloudFloorStillAllowsReadsAndCancellations(t *testing.T) {
	f := newPassthroughFixture(t, 10, nil)
	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/v2/agent/job-1"},
		{http.MethodDelete, "/v2/interact/session-1"},
		{http.MethodGet, "/v2/team/credit-usage"},
	} {
		if recorder := f.do(t, c.method, c.target, ""); recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
			t.Fatalf("%s %s status = %d, want forwarded", c.method, c.target, recorder.Code)
		}
	}
}

func TestCloudFloorFailsClosedWhenBalanceUnavailable(t *testing.T) {
	f := newPassthroughFixture(t, 1000, nil)
	f.cloud.balanceStatus = http.StatusInternalServerError
	recorder := f.do(t, http.MethodPost, "/v2/agent", `{}`)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "unavailable") {
		t.Fatalf("status = %d body = %s, want 503 unavailable", recorder.Code, recorder.Body.String())
	}
	if got := f.cloud.paths(); len(got) != 1 {
		t.Fatalf("cloud requests = %v, want only the balance read", got)
	}
}

func TestCloudRoutesRequireConfiguredCloudKey(t *testing.T) {
	f := newPassthroughFixture(t, 1000, func(config *router.Config) { config.CloudAPIKey = "" })
	if recorder := f.do(t, http.MethodGet, "/v2/agent", ""); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestUnlistedRoutesAreNotForwarded(t *testing.T) {
	f := newPassthroughFixture(t, 1000, nil)
	for _, c := range []struct{ method, target string }{
		{http.MethodPost, "/v2/monitor"},
		{http.MethodGet, "/v2/monitor"},
		{http.MethodPost, "/v2/monitor/m-1/run"},
		{http.MethodPut, "/v2/team/threat-protection"},
		{http.MethodPost, "/v2/feedback"},
		{http.MethodPost, "/v2/search/job-1/feedback"},
		{http.MethodPost, "/v2/support/ask"},
		{http.MethodGet, "/v2/parse/formats"},
		{http.MethodPost, "/v2/team/credit-usage"},
		{http.MethodGet, "/v1/scrape"},
	} {
		recorder := f.do(t, c.method, c.target, "")
		if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status = %d, want 404 or 405", c.method, c.target, recorder.Code)
		}
	}
	if got := append(f.local.paths(), f.cloud.paths()...); len(got) != 0 {
		t.Fatalf("upstream requests = %v, want none", got)
	}
}

func TestPassthroughRejectsEncodedPathTraversal(t *testing.T) {
	f := newPassthroughFixture(t, 1000, nil)
	for _, target := range []string{"/v2/agent/x%2F..%2Fteam", "/v2/crawl/a%2Fb", "/v2/agent/..%2E"} {
		recorder := f.do(t, http.MethodGet, target, "")
		if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want rejection", target, recorder.Code)
		}
	}
	if got := append(f.local.paths(), f.cloud.paths()...); len(got) != 0 {
		t.Fatalf("upstream requests = %v, want none", got)
	}
}

func TestPassthroughRequiresRouterBearer(t *testing.T) {
	f := newPassthroughFixture(t, 1000, func(config *router.Config) { config.APIKey = "secret" })
	request := httptest.NewRequest(http.MethodPost, "/v2/agent", bytes.NewBufferString(`{}`))
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if got := f.cloud.paths(); len(got) != 0 {
		t.Fatalf("cloud requests = %v, want none", got)
	}
}

func TestPassthroughRequestsAreMeasured(t *testing.T) {
	f := newPassthroughFixture(t, 1000, nil)
	f.do(t, http.MethodPost, "/v2/map", `{}`)
	f.do(t, http.MethodGet, "/v2/agent", "")
	text := f.metrics.PrometheusText()
	for _, want := range []string{
		`web_retrieval_requests_total{endpoint="local_passthrough",status_class="2xx"} 1`,
		`web_retrieval_requests_total{endpoint="cloud_passthrough",status_class="2xx"} 1`,
		`web_retrieval_local_attempts_total{endpoint="local_passthrough"} 1`,
		`web_retrieval_cloud_attempts_total{endpoint="cloud_passthrough"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
}

func TestScrapeFallbackRespectsCreditFloor(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"success":false,"error":"blocked by anti-bot"}`)
	}))
	defer local.Close()
	cloud := newRecordingUpstream(t, 50)
	floor := creditspkg.NewFloor(upstream.NewAuthenticated(cloud.server.URL, nil, "cloud-key"), 50)
	handler := router.NewHandler(router.Config{
		LocalBaseURL: local.URL, CloudBaseURL: cloud.server.URL, CloudAPIKey: "cloud-key",
		MaxRequestBytes: 1 << 10, MaxResponseBytes: 1 << 20,
	}, router.Dependencies{CreditFloor: floor})
	request := httptest.NewRequest(http.MethodPost, "/v2/scrape", strings.NewReader(`{"url":"https://example.com"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "credit budget exceeded") {
		t.Fatalf("status = %d body = %s, want local result with budget warning", recorder.Code, recorder.Body.String())
	}
	if got := cloud.paths(); strings.Join(got, ",") != "GET /v2/team/credit-usage" {
		t.Fatalf("cloud requests = %v, want only the balance read", got)
	}
}
