package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	budgetpkg "web-retrieval/internal/budget"
	cachepkg "web-retrieval/internal/cache"
	metricspkg "web-retrieval/internal/metrics"
	"web-retrieval/internal/router"
	flightpkg "web-retrieval/internal/singleflight"
)

func TestMCPDisabledReturnsNotFoundWithoutReadingBody(t *testing.T) {
	body := &failOnReadBody{}
	request := httptest.NewRequest(http.MethodPost, "/mcp", body)
	recorder := httptest.NewRecorder()
	router.NewHandler(router.Config{MCPEnabled: false, APIKey: "secret"}, router.Dependencies{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if body.read {
		t.Fatal("disabled MCP route read request body")
	}
}

func TestMCPEnabledRequiresConfiguredBearer(t *testing.T) {
	handler := router.NewHandler(router.Config{MCPEnabled: true, APIKey: "secret"}, router.Dependencies{})
	for _, authorization := range []string{"", "Bearer wrong", "Basic secret"} {
		request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{}`))
		request.Header.Set("Authorization", authorization)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("authorization %q status = %d, want 401", authorization, recorder.Code)
		}
		if got := recorder.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
		}
	}
}

type failOnReadBody struct{ read bool }

func (b *failOnReadBody) Read([]byte) (int, error) {
	b.read = true
	return 0, errors.New("unexpected read")
}

func (*failOnReadBody) Close() error { return nil }

func TestMetricsEndpointIsUnauthenticatedAndReportsRequestStatusAndDuration(t *testing.T) {
	registry := metricspkg.NewRegistry()
	handler := router.NewHandler(router.Config{APIKey: "secret"}, router.Dependencies{Metrics: registry})

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	metricsResponse := httptest.NewRecorder()
	handler.ServeHTTP(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if metricsResponse.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", metricsResponse.Code)
	}
	if got, want := metricsResponse.Header().Get("Content-Type"), "text/plain; version=0.0.4"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	for _, want := range []string{
		`web_retrieval_requests_total{endpoint="health",status_class="2xx"} 1`,
		`web_retrieval_requests_total{endpoint="metrics",status_class="2xx"} 1`,
		`web_retrieval_request_duration_seconds_count{endpoint="health"} 1`,
		`web_retrieval_request_duration_seconds_count{endpoint="metrics"} 1`,
	} {
		if !strings.Contains(metricsResponse.Body.String(), want+"\n") {
			t.Errorf("metrics missing %q:\n%s", want, metricsResponse.Body.String())
		}
	}
}

func TestMetricsDistinguishCacheLocalCloudDenialAndFailures(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"success":false,"error":"blocked"}`))
	}))
	defer local.Close()
	cloudCalls := 0
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalls++
		_, _ = w.Write([]byte(`{"success":true,"data":{"web":[{"url":"https://cloud.example"}]}}`))
	}))
	defer cloud.Close()
	registry := metricspkg.NewRegistry()
	ledger := budgetpkg.NewMemory(2, 0, time.Now)
	handler := router.NewHandler(router.Config{
		LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key", SearchTTL: time.Minute,
	}, router.Dependencies{
		HTTPClient: local.Client(), Budget: ledger, Metrics: registry,
		Cache: &testCache{entries: make(map[string]cachepkg.Entry)},
	})

	for _, body := range []string{`{"query":"fixed"}`, `{"query":"fixed"}`, `{"query":"denied"}`} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(body)))
	}
	text := registry.PrometheusText()
	for _, want := range []string{
		`web_retrieval_requests_total{endpoint="search",status_class="2xx"} 2`,
		`web_retrieval_requests_total{endpoint="search",status_class="5xx"} 1`,
		`web_retrieval_cache_hits_total{endpoint="search"} 1`,
		`web_retrieval_local_attempts_total{endpoint="search"} 2`,
		`web_retrieval_cloud_attempts_total{endpoint="search"} 1`,
		`web_retrieval_cloud_budget_denied_total{endpoint="search"} 1`,
		`web_retrieval_upstream_failures_total{endpoint="search",upstream="local"} 2`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
	if cloudCalls != 1 {
		t.Fatalf("cloud calls = %d, want 1", cloudCalls)
	}
}

func TestConcurrentSearchAndScrapeMetrics(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/scrape" {
			_, _ = w.Write([]byte(`{"success":true,"data":{"markdown":"ok"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"web":[]}}`))
	}))
	defer local.Close()
	registry := metricspkg.NewRegistry()
	handler := router.NewHandler(router.Config{LocalBaseURL: local.URL}, router.Dependencies{
		HTTPClient: local.Client(), Metrics: registry,
	})

	const requestsPerEndpoint = 100
	var group sync.WaitGroup
	for _, path := range []string{"/v2/search", "/v2/scrape"} {
		for range requestsPerEndpoint {
			group.Add(1)
			go func() {
				defer group.Done()
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
				if recorder.Code != http.StatusOK {
					t.Errorf("%s status = %d, want 200", path, recorder.Code)
				}
			}()
		}
	}
	group.Wait()
	text := registry.PrometheusText()
	for _, endpoint := range []string{"search", "scrape"} {
		for _, want := range []string{
			`web_retrieval_requests_total{endpoint="` + endpoint + `",status_class="2xx"} 100`,
			`web_retrieval_local_attempts_total{endpoint="` + endpoint + `"} 100`,
			`web_retrieval_request_duration_seconds_count{endpoint="` + endpoint + `"} 100`,
		} {
			if !strings.Contains(text, want+"\n") {
				t.Errorf("metrics missing %q", want)
			}
		}
	}
}

func TestSearchCloudFallbackReservesEstimatedCreditsAndReturnsWarnedLocalJSONAtLimit(t *testing.T) {
	localBody := []byte(`{"success":false,"error":"local blocked","unknown":{"kept":42}}`)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write(localBody)
	}))
	defer local.Close()
	cloudCalls := 0
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"web":[{"url":"https://cloud.example"}]}}`))
	}))
	defer cloud.Close()

	ledger := budgetpkg.NewMemory(2, 0, func() time.Time { return time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC) })
	handler := router.NewHandler(
		router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"},
		router.Dependencies{HTTPClient: local.Client(), Budget: ledger},
	)
	for i := range 2 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewBufferString(`{"query":"budget"}`)))
		if i == 0 {
			if recorder.Code != http.StatusOK {
				t.Fatalf("first status = %d, want 200", recorder.Code)
			}
			continue
		}
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("denied status = %d, want local %d", recorder.Code, http.StatusBadGateway)
		}
		var body struct {
			Warning string `json:"warning"`
			Unknown struct {
				Kept int `json:"kept"`
			} `json:"unknown"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("denied body is not JSON: %v; body = %q", err, recorder.Body.Bytes())
		}
		if !strings.Contains(body.Warning, "cloud fallback skipped") || !strings.Contains(body.Warning, "budget") {
			t.Fatalf("warning = %q, want explicit cloud budget warning", body.Warning)
		}
		if got := recorder.Header().Get("X-Web-Retrieval-Warning"); got != "" {
			t.Fatalf("X-Web-Retrieval-Warning = %q, want object warning only in body", got)
		}
		if body.Unknown.Kept != 42 {
			t.Fatalf("unknown field = %d, want 42", body.Unknown.Kept)
		}
	}
	if cloudCalls != 1 {
		t.Fatalf("cloud calls = %d, want 1", cloudCalls)
	}
}

func TestFallbackReservationFailureWarnsForNonObjectLocalResponses(t *testing.T) {
	bodyCases := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "plain text", contentType: "text/plain; charset=utf-8", body: "local renderer failed\n"},
		{name: "malformed JSON", contentType: "application/json", body: `{"success":false`},
		{name: "JSON array", contentType: "application/problem+json", body: `["local",{"failure":true}]`},
		{name: "JSON scalar", contentType: "application/json; charset=utf-8", body: `false`},
	}
	reservationCases := []struct {
		name        string
		ledger      func(t *testing.T) budgetpkg.Ledger
		wantWarning string
	}{
		{
			name: "credit exhaustion",
			ledger: func(t *testing.T) budgetpkg.Ledger {
				t.Helper()
				ledger := budgetpkg.NewMemory(1, 0, time.Now)
				if err := ledger.Reserve(context.Background(), 1); err != nil {
					t.Fatal(err)
				}
				return ledger
			},
			wantWarning: "cloud fallback skipped: credit budget exceeded",
		},
		{
			name:        "ledger unavailable",
			ledger:      func(*testing.T) budgetpkg.Ledger { return &errorLedger{err: errors.New("ledger disk unavailable")} },
			wantWarning: "cloud fallback skipped because budget accounting was unavailable",
		},
	}

	for _, endpoint := range []string{"search", "scrape"} {
		requestBody := `{"query":"warning visibility"}`
		if endpoint == "scrape" {
			requestBody = `{"url":"https://example.test","formats":["markdown"]}`
		}
		for _, reservationCase := range reservationCases {
			for _, bodyCase := range bodyCases {
				t.Run(endpoint+"/"+reservationCase.name+"/"+bodyCase.name, func(t *testing.T) {
					local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", bodyCase.contentType)
						w.WriteHeader(http.StatusBadGateway)
						_, _ = io.WriteString(w, bodyCase.body)
					}))
					defer local.Close()
					cloudCalls := 0
					cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { cloudCalls++ }))
					defer cloud.Close()

					recorder := httptest.NewRecorder()
					router.NewHandler(
						router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"},
						router.Dependencies{HTTPClient: local.Client(), Budget: reservationCase.ledger(t)},
					).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/"+endpoint, strings.NewReader(requestBody)))

					if recorder.Code != http.StatusBadGateway {
						t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
					}
					if got := recorder.Header().Get("Content-Type"); got != bodyCase.contentType {
						t.Fatalf("Content-Type = %q, want %q", got, bodyCase.contentType)
					}
					if got := recorder.Body.String(); got != bodyCase.body {
						t.Fatalf("body = %q, want exact %q", got, bodyCase.body)
					}
					if got := recorder.Header().Get("X-Web-Retrieval-Warning"); got != reservationCase.wantWarning {
						t.Fatalf("X-Web-Retrieval-Warning = %q, want %q", got, reservationCase.wantWarning)
					}
					if cloudCalls != 0 {
						t.Fatalf("cloud calls = %d, want 0", cloudCalls)
					}
				})
			}
		}
	}
}

func TestCacheHitsPropagateResponseWarning(t *testing.T) {
	for _, endpoint := range []string{"search", "scrape"} {
		t.Run(endpoint, func(t *testing.T) {
			requestBody := `{"query":"cached warning"}`
			responseBody := `{"success":true,"data":{"web":[{"url":"https://local.example"}]}}`
			if endpoint == "scrape" {
				requestBody = `{"url":"https://example.test","formats":["markdown"]}`
				responseBody = `{"success":true,"data":{"markdown":"cached"}}`
			}
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, responseBody)
			}))
			defer local.Close()
			responseCache := &testCache{entries: make(map[string]cachepkg.Entry)}
			handler := router.NewHandler(
				router.Config{LocalBaseURL: local.URL, SearchTTL: time.Minute, ScrapeTTL: time.Minute},
				router.Dependencies{HTTPClient: local.Client(), Cache: responseCache},
			)

			first := httptest.NewRecorder()
			handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v2/"+endpoint, strings.NewReader(requestBody)))
			responseCache.mu.Lock()
			for key, entry := range responseCache.entries {
				entry.Warning = "persisted warning"
				responseCache.entries[key] = entry
			}
			responseCache.mu.Unlock()

			second := httptest.NewRecorder()
			handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v2/"+endpoint, strings.NewReader(requestBody)))
			if got := second.Header().Get("X-Web-Retrieval-Warning"); got != "persisted warning" {
				t.Fatalf("cache-hit warning = %q, want persisted warning", got)
			}
			if got := second.Body.String(); got != responseBody {
				t.Fatalf("cache-hit body = %q, want exact %q", got, responseBody)
			}
		})
	}
}

func TestSearchBudgetDenialWithoutLocalResponseReturns503JSON(t *testing.T) {
	closedLocal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	localURL := closedLocal.URL
	closedLocal.Close()
	cloudCalls := 0
	cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { cloudCalls++ }))
	defer cloud.Close()
	ledger := budgetpkg.NewMemory(1, 0, time.Now)

	recorder := httptest.NewRecorder()
	router.NewHandler(
		router.Config{LocalBaseURL: localURL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"},
		router.Dependencies{HTTPClient: cloud.Client(), Budget: ledger},
	).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewBufferString(`{"query":"unavailable"}`)))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %q", recorder.Code, recorder.Body.Bytes())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || !strings.Contains(body["error"], "budget") {
		t.Fatalf("body = %q, want JSON budget error; decode error = %v", recorder.Body.Bytes(), err)
	}
	if cloudCalls != 0 {
		t.Fatalf("cloud calls = %d, want 0", cloudCalls)
	}
}

func TestLedgerOperationalErrorSkipsCloudWithoutClaimingBudgetExhaustion(t *testing.T) {
	for _, endpoint := range []string{"search", "scrape"} {
		endpoint := endpoint
		requestBody := `{"query":"accounting"}`
		if endpoint == "scrape" {
			requestBody = `{"url":"https://example.test","formats":["markdown"]}`
		}

		t.Run(endpoint+" captured local response", func(t *testing.T) {
			localBody := []byte(`{"success":false,"error":"truthful local failure","future":{"kept":true}}`)
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusGatewayTimeout)
				_, _ = w.Write(localBody)
			}))
			defer local.Close()
			cloudCalls := 0
			cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { cloudCalls++ }))
			defer cloud.Close()
			registry := metricspkg.NewRegistry()
			ledger := &errorLedger{err: errors.New("ledger disk unavailable")}

			recorder := httptest.NewRecorder()
			router.NewHandler(
				router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"},
				router.Dependencies{HTTPClient: local.Client(), Budget: ledger, Metrics: registry},
			).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/"+endpoint, strings.NewReader(requestBody)))

			if recorder.Code != http.StatusGatewayTimeout || recorder.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("response = (%d, %q, %q), want preserved local status and type", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Bytes())
			}
			var body struct {
				Warning string `json:"warning"`
				Future  struct {
					Kept bool `json:"kept"`
				} `json:"future"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || !body.Future.Kept {
				t.Fatalf("body = %q, want valid warned local JSON preserving unknown fields; decode error = %v", recorder.Body.Bytes(), err)
			}
			if !strings.Contains(body.Warning, "cloud fallback skipped") || !strings.Contains(body.Warning, "budget accounting was unavailable") || strings.Contains(body.Warning, "exceeded") {
				t.Fatalf("warning = %q, want truthful accounting-unavailable warning", body.Warning)
			}
			assertLedgerOperationalFailure(t, endpoint, ledger, cloudCalls, registry)
		})

		t.Run(endpoint+" no local response", func(t *testing.T) {
			closedLocal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			localURL := closedLocal.URL
			closedLocal.Close()
			cloudCalls := 0
			cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { cloudCalls++ }))
			defer cloud.Close()
			registry := metricspkg.NewRegistry()
			ledger := &errorLedger{err: errors.New("ledger disk unavailable")}

			recorder := httptest.NewRecorder()
			router.NewHandler(
				router.Config{LocalBaseURL: localURL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"},
				router.Dependencies{HTTPClient: cloud.Client(), Budget: ledger, Metrics: registry},
			).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/"+endpoint, strings.NewReader(requestBody)))

			if recorder.Code != http.StatusBadGateway || recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("response = (%d, %q, %q), want JSON 502", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Bytes())
			}
			var body map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || !strings.Contains(body["error"], "budget accounting") || strings.Contains(body["error"], "exceeded") {
				t.Fatalf("body = %q, want explicit budget accounting failure; decode error = %v", recorder.Body.Bytes(), err)
			}
			assertLedgerOperationalFailure(t, endpoint, ledger, cloudCalls, registry)
		})

		t.Run(endpoint+" unreadable local response", func(t *testing.T) {
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(bytes.Repeat([]byte("x"), 129))
			}))
			defer local.Close()
			cloudCalls := 0
			cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { cloudCalls++ }))
			defer cloud.Close()
			registry := metricspkg.NewRegistry()
			ledger := &errorLedger{err: errors.New("ledger disk unavailable")}

			recorder := httptest.NewRecorder()
			router.NewHandler(
				router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key", MaxResponseBytes: 128},
				router.Dependencies{HTTPClient: local.Client(), Budget: ledger, Metrics: registry},
			).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/"+endpoint, strings.NewReader(requestBody)))

			if recorder.Code != http.StatusBadGateway || recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("response = (%d, %q, %q), want JSON 502", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Bytes())
			}
			var body map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || !strings.Contains(body["error"], "budget accounting") || strings.Contains(body["error"], "exceeded") {
				t.Fatalf("body = %q, want explicit budget accounting failure; decode error = %v", recorder.Body.Bytes(), err)
			}
			assertLedgerOperationalFailure(t, endpoint, ledger, cloudCalls, registry)
		})
	}
}

func assertLedgerOperationalFailure(t *testing.T, endpoint string, ledger *errorLedger, cloudCalls int, registry *metricspkg.Registry) {
	t.Helper()
	if ledger.calls != 1 {
		t.Fatalf("ledger calls = %d, want 1", ledger.calls)
	}
	if cloudCalls != 0 {
		t.Fatalf("cloud calls = %d, want 0", cloudCalls)
	}
	want := `web_retrieval_cloud_budget_denied_total{endpoint="` + endpoint + `"} 0` + "\n"
	if metrics := registry.PrometheusText(); !strings.Contains(metrics, want) {
		t.Fatalf("budget-denied metric must remain zero; missing %q:\n%s", strings.TrimSpace(want), metrics)
	}
}

type errorLedger struct {
	err   error
	calls int
}

func (l *errorLedger) Reserve(context.Context, int) error {
	l.calls++
	return l.err
}

func TestScrapeCloudFallbackReservesCreditsAndHandlesBudgetDenial(t *testing.T) {
	t.Run("truthful local response gets warning", func(t *testing.T) {
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte(`{"success":false,"error":"renderer timeout","future":{"kept":true}}`))
		}))
		defer local.Close()
		cloudCalls := 0
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			cloudCalls++
			_, _ = w.Write([]byte(`{"success":true,"data":{"markdown":"cloud"}}`))
		}))
		defer cloud.Close()
		ledger := budgetpkg.NewMemory(1, 0, time.Now)
		handler := router.NewHandler(
			router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"},
			router.Dependencies{HTTPClient: local.Client(), Budget: ledger},
		)
		for range 2 {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewBufferString(`{"url":"https://example.test"}`)))
			if cloudCalls == 1 && recorder.Code == http.StatusOK {
				continue
			}
			var body map[string]json.RawMessage
			if recorder.Code != http.StatusGatewayTimeout || json.Unmarshal(recorder.Body.Bytes(), &body) != nil || body["warning"] == nil || body["future"] == nil {
				t.Fatalf("denied response = (%d, %q), want warned truthful local JSON", recorder.Code, recorder.Body.Bytes())
			}
		}
		if cloudCalls != 1 {
			t.Fatalf("cloud calls = %d, want 1", cloudCalls)
		}
	})

	t.Run("no local response returns 503", func(t *testing.T) {
		closedLocal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		localURL := closedLocal.URL
		closedLocal.Close()
		cloudCalls := 0
		cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { cloudCalls++ }))
		defer cloud.Close()
		ledger := budgetpkg.NewMemory(1, 0, time.Now)
		if err := ledger.Reserve(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		router.NewHandler(
			router.Config{LocalBaseURL: localURL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"},
			router.Dependencies{HTTPClient: cloud.Client(), Budget: ledger},
		).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewBufferString(`{"url":"https://example.test"}`)))
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Content-Type") != "application/json" || cloudCalls != 0 {
			t.Fatalf("response = (%d, %q), cloud calls = %d; want 503 JSON and no cloud call", recorder.Code, recorder.Body.Bytes(), cloudCalls)
		}
	})
}

type testCache struct {
	mu      sync.Mutex
	entries map[string]cachepkg.Entry
}

func (c *testCache) Get(_ context.Context, key string) (cachepkg.Entry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok, nil
}

func (c *testCache) Set(_ context.Context, key string, entry cachepkg.Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry
	return nil
}

type blockingSetCache struct {
	mu         sync.Mutex
	entries    map[string]cachepkg.Entry
	setStarted chan struct{}
	releaseSet chan struct{}
	setOnce    sync.Once
	setCalls   int
}

func (c *blockingSetCache) Get(_ context.Context, key string) (cachepkg.Entry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok, nil
}

func (c *blockingSetCache) Set(_ context.Context, key string, entry cachepkg.Entry) error {
	c.setOnce.Do(func() {
		close(c.setStarted)
		<-c.releaseSet
	})
	c.mu.Lock()
	c.setCalls++
	c.entries[key] = entry
	c.mu.Unlock()
	if len(entry.Body) > 0 {
		entry.Body[0] = 'X'
	}
	return nil
}

func TestHealth(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/health", nil)

	router.NewHandler(router.Config{}, router.Dependencies{}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got, want := recorder.Header().Get("Content-Type"), "application/json"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	if got, want := recorder.Body.String(), "{\"status\":\"ok\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestIngressBearerAuthenticationRejectsBeforeUpstream(t *testing.T) {
	upstreamCalls := 0
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer local.Close()
	handler := router.NewHandler(router.Config{LocalBaseURL: local.URL, APIKey: "expected-secret"}, router.Dependencies{HTTPClient: local.Client()})

	for _, authorization := range []string{"", "Bearer wrong", "Basic expected-secret", "Bearer expected-secret extra"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(`{"query":"x"}`))
		request.Header.Set("Authorization", authorization)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("authorization %q status = %d, want 401", authorization, recorder.Code)
		}
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(`{"query":"x"}`))
	request.Header.Set("Authorization", "Bearer expected-secret")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || upstreamCalls != 1 {
		t.Fatalf("authorized response = %d, upstream calls = %d", recorder.Code, upstreamCalls)
	}
}

func TestRequestBodyLimitReturns413WithoutUpstream(t *testing.T) {
	calls := 0
	local := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer local.Close()
	handler := router.NewHandler(router.Config{LocalBaseURL: local.URL, MaxRequestBytes: 8}, router.Dependencies{HTTPClient: local.Client()})
	for _, path := range []string{"/v2/search", "/v2/scrape"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"query":"too large"}`)))
		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s status = %d, want 413", path, recorder.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestOversizedUpstreamResponseReturns502AfterFallbackWithoutCache(t *testing.T) {
	localCalls, cloudCalls := 0, 0
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		localCalls++
		_, _ = w.Write([]byte(`{"success":false,"error":"this response is too large"}`))
	}))
	defer local.Close()
	cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { cloudCalls++ }))
	defer cloud.Close()
	handler := router.NewHandler(router.Config{
		LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "secret", MaxResponseBytes: 12, SearchTTL: time.Minute,
	}, router.Dependencies{HTTPClient: local.Client(), Cache: &testCache{entries: make(map[string]cachepkg.Entry)}})
	for range 2 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(`{"query":"x"}`)))
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body = %q", recorder.Code, recorder.Body.String())
		}
	}
	if localCalls != 2 || cloudCalls != 2 {
		t.Fatalf("local calls = %d, cloud calls = %d; want 2, 2", localCalls, cloudCalls)
	}
}

func TestSearchForwardsLocalResponseUnchanged(t *testing.T) {
	requestBody := []byte(`{"query":"tdd","futureOption":{"enabled":true}}`)
	responseBody := []byte(`{"success":true,"data":[{"url":"https://example.test","unknown":{"answer":42}}],"futureField":"preserved"}`)

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodPost; got != want {
			t.Errorf("method = %q, want %q", got, want)
		}
		if got, want := r.URL.Path, "/v2/search"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		var gotBody bytes.Buffer
		if _, err := gotBody.ReadFrom(r.Body); err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if !bytes.Equal(gotBody.Bytes(), requestBody) {
			t.Errorf("body = %q, want %q", gotBody.Bytes(), requestBody)
		}

		w.Header().Set("Content-Type", "application/vnd.firecrawl+json; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write(responseBody)
	}))
	defer local.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewReader(requestBody))
	router.NewHandler(
		router.Config{LocalBaseURL: local.URL},
		router.Dependencies{HTTPClient: local.Client()},
	).ServeHTTP(recorder, request)

	if got, want := recorder.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := recorder.Header().Get("Content-Type"), "application/vnd.firecrawl+json; charset=utf-8"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	if !bytes.Equal(recorder.Body.Bytes(), responseBody) {
		t.Fatalf("body = %q, want %q", recorder.Body.Bytes(), responseBody)
	}
}

func TestSearchCachesSemanticallyIdenticalJSON(t *testing.T) {
	responseBody := []byte(`{"success":true,"data":{"web":[{"url":"https://example.test"}]},"unknown":42}`)
	upstreamCalls := 0
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/vnd.firecrawl+json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(responseBody)
	}))
	defer local.Close()

	handler := router.NewHandler(
		router.Config{LocalBaseURL: local.URL, SearchTTL: time.Minute},
		router.Dependencies{
			HTTPClient: local.Client(),
			Cache:      &testCache{entries: make(map[string]cachepkg.Entry)},
			Clock:      func() time.Time { return time.Unix(1_000, 0) },
		},
	)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewBufferString(`{"query":"cache","options":{"limit":3,"lang":"en"}}`)))
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewBufferString(`{"options":{"lang":"en","limit":3},"query":"cache"}`)))

	if got, want := upstreamCalls, 1; got != want {
		t.Fatalf("local upstream calls = %d, want %d", got, want)
	}
	if got, want := second.Code, http.StatusCreated; got != want {
		t.Fatalf("cached status = %d, want %d", got, want)
	}
	if got, want := second.Header().Get("Content-Type"), "application/vnd.firecrawl+json; charset=utf-8"; got != want {
		t.Fatalf("cached Content-Type = %q, want %q", got, want)
	}
	if !bytes.Equal(second.Body.Bytes(), responseBody) {
		t.Fatalf("cached body = %q, want exact bytes %q", second.Body.Bytes(), responseBody)
	}
}

func TestSearchDoesNotCacheZeroWebResults(t *testing.T) {
	responseBody := []byte(`{"success":true,"data":{"web":[]},"future":"preserved"}`)
	localCalls := 0
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		localCalls++
		w.Header().Set("Content-Type", "application/vnd.firecrawl+json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(responseBody)
	}))
	defer local.Close()

	handler := router.NewHandler(
		router.Config{LocalBaseURL: local.URL, SearchTTL: time.Minute},
		router.Dependencies{
			HTTPClient: local.Client(),
			Cache:      &testCache{entries: make(map[string]cachepkg.Entry)},
			Clock:      func() time.Time { return time.Unix(1_000, 0) },
		},
	)

	for range 2 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(`{"query":"empty"}`)))
		if recorder.Code != http.StatusAccepted || recorder.Header().Get("Content-Type") != "application/vnd.firecrawl+json" || !bytes.Equal(recorder.Body.Bytes(), responseBody) {
			t.Fatalf("response = (%d, %q, %q), want exact local response", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Bytes())
		}
	}
	if localCalls != 2 {
		t.Fatalf("local calls = %d, want 2; empty result must not be cached", localCalls)
	}
}

func TestConcurrentIdenticalSearchesShareOneUpstreamCall(t *testing.T) {
	responseBody := []byte(`{"success":true,"data":{"web":[{"url":"https://example.test"}]}}`)
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	upstreamCalls := 0
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		upstreamCalls++
		if upstreamCalls == 1 {
			close(started)
		}
		mu.Unlock()
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(responseBody)
	}))
	defer local.Close()

	handler := router.NewHandler(
		router.Config{LocalBaseURL: local.URL, SearchTTL: time.Minute},
		router.Dependencies{
			HTTPClient:  local.Client(),
			Cache:       &testCache{entries: make(map[string]cachepkg.Entry)},
			Clock:       time.Now,
			FlightGroup: flightpkg.New[cachepkg.Entry](),
		},
	)

	type result struct {
		code        int
		contentType string
		body        []byte
	}
	results := make(chan result, 2)
	request := func(body string) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewBufferString(body)))
		results <- result{recorder.Code, recorder.Header().Get("Content-Type"), bytes.Clone(recorder.Body.Bytes())}
	}
	go request(`{"query":"same","options":{"limit":2}}`)
	<-started
	go request(`{"options":{"limit":2},"query":"same"}`)
	time.Sleep(20 * time.Millisecond)
	close(release)

	for range 2 {
		got := <-results
		if got.code != http.StatusAccepted || got.contentType != "application/json" || !bytes.Equal(got.body, responseBody) {
			t.Fatalf("response = (%d, %q, %q), want independent exact response", got.code, got.contentType, got.body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if upstreamCalls != 1 {
		t.Fatalf("local upstream calls = %d, want 1", upstreamCalls)
	}
}

func TestCacheWriteCompletesInsideSearchAndScrapeSingleflight(t *testing.T) {
	for _, test := range []struct {
		name         string
		path         string
		requestBody  string
		responseBody string
		ttlConfig    func(*router.Config)
	}{
		{
			name:         "search",
			path:         "/v2/search",
			requestBody:  `{"query":"cache before flight completion"}`,
			responseBody: `{"success":true,"data":{"web":[{"url":"https://example.test"}]}}`,
			ttlConfig:    func(config *router.Config) { config.SearchTTL = time.Minute },
		},
		{
			name:         "scrape",
			path:         "/v2/scrape",
			requestBody:  `{"url":"https://example.test","formats":["markdown"]}`,
			responseBody: `{"success":true,"data":{"markdown":"# Cached"}}`,
			ttlConfig:    func(config *router.Config) { config.ScrapeTTL = time.Minute },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var callsMu sync.Mutex
			upstreamCalls := 0
			duplicateCall := make(chan struct{}, 1)
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				callsMu.Lock()
				upstreamCalls++
				if upstreamCalls > 1 {
					duplicateCall <- struct{}{}
				}
				callsMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.responseBody))
			}))
			defer local.Close()

			cache := &blockingSetCache{
				entries:    make(map[string]cachepkg.Entry),
				setStarted: make(chan struct{}),
				releaseSet: make(chan struct{}),
			}
			group := flightpkg.New[cachepkg.Entry]()
			config := router.Config{LocalBaseURL: local.URL}
			test.ttlConfig(&config)
			handler := router.NewHandler(config, router.Dependencies{
				HTTPClient:  local.Client(),
				Cache:       cache,
				Clock:       func() time.Time { return time.Unix(10_000, 0) },
				FlightGroup: group,
			})

			request := func(done chan<- *httptest.ResponseRecorder) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.requestBody)))
				done <- recorder
			}
			leaderDone := make(chan *httptest.ResponseRecorder, 1)
			go request(leaderDone)
			<-cache.setStarted

			followerDone := make(chan *httptest.ResponseRecorder, 1)
			go request(followerDone)
			joinDeadline := time.After(time.Second)
			for group.Participants() < 2 {
				select {
				case <-duplicateCall:
					close(cache.releaseSet)
					t.Fatal("follower started a duplicate upstream call while the leader cache write was blocked")
				case <-joinDeadline:
					close(cache.releaseSet)
					t.Fatal("follower did not join the flight while the leader cache write was blocked")
				default:
					runtime.Gosched()
				}
			}

			callsMu.Lock()
			gotCalls := upstreamCalls
			callsMu.Unlock()
			if gotCalls != 1 {
				close(cache.releaseSet)
				t.Fatalf("upstream calls = %d, want 1", gotCalls)
			}
			close(cache.releaseSet)
			for _, done := range []<-chan *httptest.ResponseRecorder{leaderDone, followerDone} {
				select {
				case recorder := <-done:
					if recorder.Code != http.StatusOK || recorder.Body.String() != test.responseBody {
						t.Fatalf("response = (%d, %q), want exact successful upstream response", recorder.Code, recorder.Body.String())
					}
				case <-time.After(time.Second):
					t.Fatal("request did not complete after cache write was released")
				}
			}
			cache.mu.Lock()
			defer cache.mu.Unlock()
			if cache.setCalls != 1 {
				t.Fatalf("cache Set calls = %d, want 1", cache.setCalls)
			}
			for _, entry := range cache.entries {
				if want := time.Unix(10_000, 0).Add(time.Minute); !entry.ExpiresAt.Equal(want) {
					t.Fatalf("cache expiry = %v, want injected clock expiry %v", entry.ExpiresAt, want)
				}
			}
		})
	}
}

func TestScrapeCachesSemanticallyIdenticalJSONWithIndependentTTL(t *testing.T) {
	responseBody := []byte(`{"success":true,"data":{"markdown":"# Exact"},"future":{"kept":true}}`)
	upstreamCalls := 0
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/vnd.firecrawl+json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(responseBody)
	}))
	defer local.Close()

	handler := router.NewHandler(
		router.Config{LocalBaseURL: local.URL, ScrapeTTL: 2 * time.Minute},
		router.Dependencies{
			HTTPClient:  local.Client(),
			Cache:       &testCache{entries: make(map[string]cachepkg.Entry)},
			Clock:       func() time.Time { return time.Unix(2_000, 0) },
			FlightGroup: flightpkg.New[cachepkg.Entry](),
		},
	)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewBufferString(`{"url":"https://example.test","formats":["markdown"]}`)))
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewBufferString(`{"formats":["markdown"],"url":"https://example.test"}`)))

	if upstreamCalls != 1 {
		t.Fatalf("local upstream calls = %d, want 1", upstreamCalls)
	}
	if second.Code != http.StatusCreated || second.Header().Get("Content-Type") != "application/vnd.firecrawl+json" || !bytes.Equal(second.Body.Bytes(), responseBody) {
		t.Fatalf("cached scrape response = (%d, %q, %q), want exact upstream response", second.Code, second.Header().Get("Content-Type"), second.Body.Bytes())
	}
}

func TestScrapeFileCacheRejectsEmptyRequestedMarkdownAcrossRestart(t *testing.T) {
	tests := []struct {
		name      string
		request   string
		wantCalls int
	}{
		{name: "requested markdown is empty", request: `{"url":"https://example.test","formats":["markdown"]}`, wantCalls: 2},
		{name: "formats omitted default markdown is empty", request: `{"url":"https://example.test"}`, wantCalls: 2},
		{name: "empty formats default markdown is empty", request: `{"url":"https://example.test","formats":[]}`, wantCalls: 2},
		{name: "markdown was not requested", request: `{"url":"https://example.test","formats":["html"]}`, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localCalls := 0
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				localCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"success":true,"data":{"markdown":""}}`))
			}))
			defer local.Close()
			cacheDirectory := filepath.Join(t.TempDir(), "cache")
			clock := func() time.Time { return time.Unix(4_000, 0) }

			for range 2 {
				fileCache, err := cachepkg.NewFile(cacheDirectory, clock, 1024, 4096)
				if err != nil {
					t.Fatal(err)
				}
				handler := router.NewHandler(
					router.Config{LocalBaseURL: local.URL, ScrapeTTL: time.Hour},
					router.Dependencies{HTTPClient: local.Client(), Cache: fileCache, Clock: clock},
				)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/scrape", strings.NewReader(tt.request)))
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body = %q", recorder.Code, recorder.Body.Bytes())
				}
			}
			if localCalls != tt.wantCalls {
				t.Fatalf("local calls = %d, want %d", localCalls, tt.wantCalls)
			}
		})
	}
}

func TestSearchFallsBackToCloudForLocalSuccessFalse(t *testing.T) {
	requestBody := []byte("{\n  \"query\": \"preserve this exactly\",\n  \"unknown\": {\"enabled\": true}\n}")
	cloudResponseBody := []byte(`{"success":true,"data":{"web":[{"url":"https://cloud.example"}]},"unknown":"preserved"}`)
	localCalled := make(chan struct{})

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(localCalled)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("local Authorization = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"error":"local failed"}`))
	}))
	defer local.Close()

	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-localCalled:
		default:
			t.Error("cloud was called before local")
		}
		if got, want := r.Method, http.MethodPost; got != want {
			t.Errorf("method = %q, want %q", got, want)
		}
		if got, want := r.URL.Path, "/v2/search"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		var gotBody bytes.Buffer
		if _, err := gotBody.ReadFrom(r.Body); err != nil {
			t.Fatalf("read cloud request body: %v", err)
		}
		if !bytes.Equal(gotBody.Bytes(), requestBody) {
			t.Errorf("body = %q, want exact original %q", gotBody.Bytes(), requestBody)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer configured-cloud-key"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}

		w.Header().Set("Content-Type", "application/vnd.firecrawl+json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(cloudResponseBody)
	}))
	defer cloud.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Authorization", "Bearer incoming-client-secret")
	router.NewHandler(
		router.Config{
			LocalBaseURL: local.URL,
			CloudBaseURL: cloud.URL,
			CloudAPIKey:  "configured-cloud-key",
		},
		router.Dependencies{HTTPClient: local.Client()},
	).ServeHTTP(recorder, request)

	if got, want := recorder.Code, http.StatusCreated; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := recorder.Header().Get("Content-Type"), "application/vnd.firecrawl+json; charset=utf-8"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	if !bytes.Equal(recorder.Body.Bytes(), cloudResponseBody) {
		t.Fatalf("body = %q, want %q", recorder.Body.Bytes(), cloudResponseBody)
	}
}

func TestSearchFallsBackToCloudForLocalTransportError(t *testing.T) {
	requestBody := []byte(`{"query":"transport failure"}`)
	cloudResponseBody := []byte(`{"success":true,"data":{"web":[{"url":"https://cloud.example"}]},"unknown":{"preserved":true}}`)

	local := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("closed local server should not receive a request")
	}))
	localURL := local.URL
	local.Close()

	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer cloud-key"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(cloudResponseBody)
	}))
	defer cloud.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewReader(requestBody))
	router.NewHandler(
		router.Config{LocalBaseURL: localURL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key"},
		router.Dependencies{HTTPClient: cloud.Client()},
	).ServeHTTP(recorder, request)

	if got, want := recorder.Code, http.StatusAccepted; got != want {
		t.Fatalf("status = %d, want %d; body = %q", got, want, recorder.Body.Bytes())
	}
	if !bytes.Equal(recorder.Body.Bytes(), cloudResponseBody) {
		t.Fatalf("body = %q, want exact cloud bytes %q", recorder.Body.Bytes(), cloudResponseBody)
	}
}

func TestSearchFallsBackForLocalNon2xxIncludingNotFoundAndGone(t *testing.T) {
	tests := []struct {
		name          string
		localStatus   int
		wantStatus    int
		wantCloudCall bool
	}{
		{name: "server error", localStatus: http.StatusBadGateway, wantStatus: http.StatusOK, wantCloudCall: true},
		{name: "not found", localStatus: http.StatusNotFound, wantStatus: http.StatusOK, wantCloudCall: true},
		{name: "gone", localStatus: http.StatusGone, wantStatus: http.StatusOK, wantCloudCall: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localBody := []byte(`{"success":false,"error":"local status","unknown":"local"}`)
			cloudBody := []byte(`{"success":true,"data":{"web":[{"url":"https://cloud.example"}]},"unknown":"cloud"}`)
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.localStatus)
				_, _ = w.Write(localBody)
			}))
			defer local.Close()

			cloudCalls := 0
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cloudCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(cloudBody)
			}))
			defer cloud.Close()

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewReader([]byte(`{"query":"status"}`)))
			router.NewHandler(
				router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key"},
				router.Dependencies{HTTPClient: local.Client()},
			).ServeHTTP(recorder, request)

			if got := recorder.Code; got != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", got, tt.wantStatus, recorder.Body.Bytes())
			}
			if got := cloudCalls > 0; got != tt.wantCloudCall {
				t.Fatalf("cloud called = %v, want %v", got, tt.wantCloudCall)
			}
			wantBody := localBody
			if tt.wantCloudCall {
				wantBody = cloudBody
			}
			if !bytes.Equal(recorder.Body.Bytes(), wantBody) {
				t.Fatalf("body = %q, want exact bytes %q", recorder.Body.Bytes(), wantBody)
			}
		})
	}
}

func TestSearchZeroWebResultsFallBackRegardlessOfWarning(t *testing.T) {
	tests := []struct {
		name          string
		localBody     string
		wantCloudCall bool
	}{
		{name: "unresponsive", localBody: `{"success":true,"data":{"web":[]},"warning":"Target was UNRESPONSIVE"}`, wantCloudCall: true},
		{name: "blocked", localBody: `{"success":true,"data":{"web":[]},"error":"request blocked upstream"}`, wantCloudCall: true},
		{name: "bot", localBody: `{"success":true,"data":{"web":[]},"warning":"anti-bot protection"}`, wantCloudCall: true},
		{name: "challenge", localBody: `{"success":true,"data":{"web":[]},"warning":"browser challenge shown"}`, wantCloudCall: true},
		{name: "consent", localBody: `{"success":true,"data":{"web":[]},"warning":"consent page returned"}`, wantCloudCall: true},
		{name: "captcha", localBody: `{"success":true,"data":{"web":[]},"error":"CAPTCHA required"}`, wantCloudCall: true},
		{name: "rate limit", localBody: `{"success":true,"data":{"web":[]},"warning":"rate-limit exceeded"}`, wantCloudCall: true},
		{name: "benign empty", localBody: `{"success":true,"data":{"web":[]},"warning":"no matches found"}`, wantCloudCall: true},
		{name: "results suppress fallback", localBody: `{"success":true,"data":{"web":[{"url":"https://local.example"}]},"warning":"blocked mirror ignored"}`, wantCloudCall: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localBody := []byte(tt.localBody)
			cloudBody := []byte(`{"success":true,"data":{"web":[{"url":"https://cloud.example"}]},"future":"exact"}`)
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(localBody)
			}))
			defer local.Close()

			cloudCalls := 0
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cloudCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(cloudBody)
			}))
			defer cloud.Close()

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewReader([]byte(`{"query":"classification"}`)))
			router.NewHandler(
				router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key"},
				router.Dependencies{HTTPClient: local.Client()},
			).ServeHTTP(recorder, request)

			if got := cloudCalls > 0; got != tt.wantCloudCall {
				t.Fatalf("cloud called = %v, want %v; response = %q", got, tt.wantCloudCall, recorder.Body.Bytes())
			}
			wantBody := localBody
			if tt.wantCloudCall {
				wantBody = cloudBody
			}
			if !bytes.Equal(recorder.Body.Bytes(), wantBody) {
				t.Fatalf("body = %q, want exact bytes %q", recorder.Body.Bytes(), wantBody)
			}
		})
	}
}

func TestSearchWebContainerValidationControlsFallbackAndCaching(t *testing.T) {
	tests := []struct {
		name           string
		localBody      string
		wantLocalCalls int
		wantCloudCalls int
	}{
		{name: "missing data", localBody: `{"success":true}`, wantLocalCalls: 2, wantCloudCalls: 2},
		{name: "null data", localBody: `{"success":true,"data":null}`, wantLocalCalls: 2, wantCloudCalls: 2},
		{name: "missing web", localBody: `{"success":true,"data":{}}`, wantLocalCalls: 2, wantCloudCalls: 2},
		{name: "null web", localBody: `{"success":true,"data":{"web":null}}`, wantLocalCalls: 2, wantCloudCalls: 2},
		{name: "empty web", localBody: `{"success":true,"data":{"web":[]}}`, wantLocalCalls: 2, wantCloudCalls: 2},
		{name: "nonempty web", localBody: `{"success":true,"data":{"web":[{"url":"https://local.example"}]}}`, wantLocalCalls: 1},
		{name: "malformed JSON remains tolerant", localBody: `{"success":true,"data":{"web":[]`, wantLocalCalls: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localCalls := 0
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				localCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.localBody))
			}))
			defer local.Close()

			cloudCalls := 0
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cloudCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"success":true,"data":{"web":[]}}`))
			}))
			defer cloud.Close()

			handler := router.NewHandler(
				router.Config{
					LocalBaseURL: local.URL,
					CloudBaseURL: cloud.URL,
					CloudAPIKey:  "cloud-key",
					SearchTTL:    time.Minute,
				},
				router.Dependencies{
					HTTPClient: local.Client(),
					Cache:      &testCache{entries: make(map[string]cachepkg.Entry)},
					Clock:      func() time.Time { return time.Unix(1_000, 0) },
				},
			)

			for range 2 {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(`{"query":"containers"}`)))
				if got := recorder.Body.String(); got != tt.localBody {
					t.Fatalf("body = %q, want exact local body %q", got, tt.localBody)
				}
			}

			if localCalls != tt.wantLocalCalls {
				t.Fatalf("local calls = %d, want %d", localCalls, tt.wantLocalCalls)
			}
			if cloudCalls != tt.wantCloudCalls {
				t.Fatalf("cloud calls = %d, want %d", cloudCalls, tt.wantCloudCalls)
			}
		})
	}
}

func TestSearchZeroWebResultsWithoutWarningFallsBackToCloud(t *testing.T) {
	localBody := []byte(`{"success":true,"data":{"web":[]},"future":"local"}`)
	cloudBody := []byte(`{"success":true,"data":{"web":[{"url":"https://cloud.example"}]},"future":"cloud"}`)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(localBody)
	}))
	defer local.Close()

	cloudCalls := 0
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cloudBody)
	}))
	defer cloud.Close()

	recorder := httptest.NewRecorder()
	router.NewHandler(
		router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key"},
		router.Dependencies{HTTPClient: local.Client()},
	).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(`{"query":"empty"}`)))

	if cloudCalls != 1 {
		t.Fatalf("cloud calls = %d, want 1; response = %q", cloudCalls, recorder.Body.Bytes())
	}
	if !bytes.Equal(recorder.Body.Bytes(), cloudBody) {
		t.Fatalf("body = %q, want exact cloud bytes %q", recorder.Body.Bytes(), cloudBody)
	}
}

func TestSearchEmptyCloudFallbackPreservesLocalResponseOrReturnsJSON502(t *testing.T) {
	cloudBody := []byte(`{"success":true,"data":{"web":[]},"source":"cloud"}`)

	t.Run("zero-result local response is preserved", func(t *testing.T) {
		localBody := []byte(`{"success":true,"data":{"web":[]},"source":"local"}`)
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write(localBody)
		}))
		defer local.Close()
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cloudBody)
		}))
		defer cloud.Close()

		recorder := httptest.NewRecorder()
		router.NewHandler(
			router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"},
			router.Dependencies{HTTPClient: local.Client()},
		).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(`{"query":"empty"}`)))

		if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" || !bytes.Equal(recorder.Body.Bytes(), localBody) {
			t.Fatalf("response = (%d, %q, %q), want exact local response", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Bytes())
		}
	})

	for _, tt := range []struct {
		name             string
		maxResponseBytes int64
		localHandler     http.HandlerFunc
	}{
		{
			name: "local transport failure",
			localHandler: func(w http.ResponseWriter, _ *http.Request) {
				hijacker := w.(http.Hijacker)
				connection, _, err := hijacker.Hijack()
				if err == nil {
					_ = connection.Close()
				}
			},
		},
		{
			name:             "local read failure",
			maxResponseBytes: 128,
			localHandler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(bytes.Repeat([]byte("x"), 129))
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			local := httptest.NewServer(tt.localHandler)
			defer local.Close()
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(cloudBody)
			}))
			defer cloud.Close()

			recorder := httptest.NewRecorder()
			router.NewHandler(
				router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key", MaxResponseBytes: tt.maxResponseBytes},
				router.Dependencies{HTTPClient: local.Client()},
			).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(`{"query":"empty"}`)))

			if got, want := recorder.Code, http.StatusBadGateway; got != want {
				t.Fatalf("status = %d, want %d; body = %q", got, want, recorder.Body.Bytes())
			}
			if got, want := recorder.Header().Get("Content-Type"), "application/json"; got != want {
				t.Fatalf("Content-Type = %q, want %q", got, want)
			}
			if got, want := recorder.Body.String(), "{\"error\":\"local and cloud upstream requests failed\"}\n"; got != want {
				t.Fatalf("body = %q, want %q", got, want)
			}
		})
	}
}

func TestSearchCloudTransportFailureReturnsLocalResponseOrJSON502(t *testing.T) {
	t.Run("captured local response", func(t *testing.T) {
		localBody := []byte(`{"success":false,"error":"truthful local failure","unknown":{"kept":1}}`)
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(localBody)
		}))
		defer local.Close()

		closedCloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		closedCloudURL := closedCloud.URL
		closedCloud.Close()

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewReader([]byte(`{"query":"local truth"}`)))
		router.NewHandler(
			router.Config{LocalBaseURL: local.URL, CloudBaseURL: closedCloudURL, CloudAPIKey: "cloud-key"},
			router.Dependencies{HTTPClient: local.Client()},
		).ServeHTTP(recorder, request)

		if got, want := recorder.Code, http.StatusServiceUnavailable; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if got, want := recorder.Header().Get("Content-Type"), "application/problem+json"; got != want {
			t.Fatalf("Content-Type = %q, want %q", got, want)
		}
		if !bytes.Equal(recorder.Body.Bytes(), localBody) {
			t.Fatalf("body = %q, want exact local bytes %q", recorder.Body.Bytes(), localBody)
		}
	})

	t.Run("no local response", func(t *testing.T) {
		closedLocal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		closedLocalURL := closedLocal.URL
		closedLocal.Close()
		closedCloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		closedCloudURL := closedCloud.URL
		closedCloud.Close()

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewReader([]byte(`{"query":"both unavailable"}`)))
		router.NewHandler(
			router.Config{LocalBaseURL: closedLocalURL, CloudBaseURL: closedCloudURL, CloudAPIKey: "cloud-key"},
			router.Dependencies{HTTPClient: http.DefaultClient},
		).ServeHTTP(recorder, request)

		if got, want := recorder.Code, http.StatusBadGateway; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if got, want := recorder.Header().Get("Content-Type"), "application/json"; got != want {
			t.Fatalf("Content-Type = %q, want %q", got, want)
		}
		if got, want := recorder.Body.String(), "{\"error\":\"local and cloud upstream requests failed\"}\n"; got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
	})
}

func TestSearchFailedCloudResponseReturnsExactCapturedLocalResponse(t *testing.T) {
	tests := []struct {
		name        string
		cloudStatus int
		cloudBody   string
	}{
		{name: "non-2xx", cloudStatus: http.StatusServiceUnavailable, cloudBody: `{"success":true,"data":{"web":[]}}`},
		{name: "malformed JSON", cloudStatus: http.StatusOK, cloudBody: `{"success":true`},
		{name: "success false", cloudStatus: http.StatusOK, cloudBody: `{"success":false,"error":"cloud rejected search"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localBody := []byte(`{"success":false,"error":"truthful local response","unknown":{"kept":true}}`)
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write(localBody)
			}))
			defer local.Close()

			cloudCalls := 0
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cloudCalls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.cloudStatus)
				_, _ = w.Write([]byte(tt.cloudBody))
			}))
			defer cloud.Close()

			registry := metricspkg.NewRegistry()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(`{"query":"local truth"}`))
			router.NewHandler(
				router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key"},
				router.Dependencies{HTTPClient: local.Client(), Metrics: registry},
			).ServeHTTP(recorder, request)

			if cloudCalls != 1 {
				t.Fatalf("cloud calls = %d, want 1", cloudCalls)
			}
			if got, want := recorder.Code, http.StatusNotFound; got != want {
				t.Fatalf("status = %d, want %d; body = %q", got, want, recorder.Body.Bytes())
			}
			if got, want := recorder.Header().Get("Content-Type"), "application/problem+json; charset=utf-8"; got != want {
				t.Fatalf("Content-Type = %q, want %q", got, want)
			}
			if !bytes.Equal(recorder.Body.Bytes(), localBody) {
				t.Fatalf("body = %q, want exact local bytes %q", recorder.Body.Bytes(), localBody)
			}
			if got := registry.PrometheusText(); !strings.Contains(got, `web_retrieval_upstream_failures_total{endpoint="search",upstream="cloud"} 1`+"\n") {
				t.Fatalf("cloud upstream failure metric missing:\n%s", got)
			}
		})
	}
}

func TestSearchMalformedLocalJSONOn2xxDoesNotFallBack(t *testing.T) {
	localBody := []byte(`{"success":true,"data":{"web":[],"future":"unterminated"`)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(localBody)
	}))
	defer local.Close()

	cloudCalls := 0
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalls++
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer cloud.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewReader([]byte(`{"query":"malformed response"}`)))
	router.NewHandler(
		router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key"},
		router.Dependencies{HTTPClient: local.Client()},
	).ServeHTTP(recorder, request)

	if cloudCalls != 0 {
		t.Fatalf("cloud calls = %d, want 0", cloudCalls)
	}
	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := recorder.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	if !bytes.Equal(recorder.Body.Bytes(), localBody) {
		t.Fatalf("body = %q, want exact malformed local bytes %q", recorder.Body.Bytes(), localBody)
	}
}

func TestSearchSingleflightSurvivesCanceledLeaderForActiveWaiter(t *testing.T) {
	upstreamStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	var once sync.Once
	localCalls := 0
	var callsMu sync.Mutex
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		localCalls++
		callsMu.Unlock()
		once.Do(func() { close(upstreamStarted) })
		select {
		case <-releaseUpstream:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"web":[{"url":"https://local.example"}]}}`))
		case <-r.Context().Done():
		}
	}))
	defer local.Close()

	group := flightpkg.New[cachepkg.Entry]()
	handler := router.NewHandler(
		router.Config{LocalBaseURL: local.URL, HTTPTimeout: time.Second},
		router.Dependencies{HTTPClient: local.Client(), FlightGroup: group},
	)
	requestBody := `{"query":"shared cancellation"}`
	leaderContext, cancelLeader := context.WithCancel(context.Background())
	leaderRequest := httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(requestBody)).WithContext(leaderContext)
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		handler.ServeHTTP(httptest.NewRecorder(), leaderRequest)
	}()
	<-upstreamStarted

	waiterRecorder := httptest.NewRecorder()
	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		handler.ServeHTTP(waiterRecorder, httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(requestBody)))
	}()
	joinDeadline := time.After(time.Second)
	for group.Participants() < 2 {
		select {
		case <-joinDeadline:
			t.Fatal("waiter did not join the active flight")
		default:
			runtime.Gosched()
		}
	}
	cancelLeader()
	select {
	case <-leaderDone:
	case <-time.After(time.Second):
		t.Fatal("canceled leader did not exit while shared work remained active")
	}
	close(releaseUpstream)
	select {
	case <-waiterDone:
	case <-time.After(time.Second):
		t.Fatal("active waiter did not receive shared result")
	}

	if got, want := waiterRecorder.Code, http.StatusOK; got != want {
		t.Fatalf("waiter status = %d, want %d; body = %q", got, want, waiterRecorder.Body.Bytes())
	}
	callsMu.Lock()
	gotCalls := localCalls
	callsMu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("local upstream calls = %d, want 1", gotCalls)
	}
	if got := group.Len(); got != 0 {
		t.Fatalf("retained flights = %d, want 0", got)
	}
}

func TestSingleflightDeadlineAllowsLocalTimeoutThenCloudSuccess(t *testing.T) {
	for _, test := range []struct {
		name      string
		path      string
		body      string
		cloudBody string
	}{
		{name: "search", path: "/v2/search", body: `{"query":"deadline"}`, cloudBody: `{"success":true,"data":{"web":[{"url":"https://cloud.example"}]}}`},
		{name: "scrape", path: "/v2/scrape", body: `{"url":"https://example.test","formats":["markdown"]}`, cloudBody: `{"success":true,"data":{"markdown":"cloud"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			const attemptTimeout = 40 * time.Millisecond
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(2 * attemptTimeout):
				}
			}))
			defer local.Close()

			cloudCalls := 0
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cloudCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.cloudBody))
			}))
			defer cloud.Close()

			handler := router.NewHandler(
				router.Config{
					LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key",
					HTTPTimeout: attemptTimeout,
				},
				router.Dependencies{
					HTTPClient:  &http.Client{Timeout: attemptTimeout},
					FlightGroup: flightpkg.New[cachepkg.Entry](),
				},
			)

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body)))

			if got, want := recorder.Code, http.StatusOK; got != want {
				t.Fatalf("status = %d, want %d; body = %q", got, want, recorder.Body.Bytes())
			}
			if cloudCalls != 1 {
				t.Fatalf("cloud calls = %d, want 1", cloudCalls)
			}
			if !bytes.Equal(recorder.Body.Bytes(), []byte(test.cloudBody)) {
				t.Fatalf("body = %q, want %q", recorder.Body.Bytes(), test.cloudBody)
			}
		})
	}
}

func TestScrapeForwardsLocalResponseUnchanged(t *testing.T) {
	requestBody := []byte("{\n  \"url\": \"https://example.test\",\n  \"formats\": [\"markdown\"],\n  \"future\": true\n}")
	responseBody := []byte(`{"success":true,"data":{"markdown":"# exact","unknown":42},"future":"preserved"}`)

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodPost; got != want {
			t.Errorf("method = %q, want %q", got, want)
		}
		if got, want := r.URL.Path, "/v2/scrape"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("local Authorization = %q, want empty", got)
		}
		gotBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !bytes.Equal(gotBody, requestBody) {
			t.Errorf("body = %q, want exact bytes %q", gotBody, requestBody)
		}
		w.Header().Set("Content-Type", "application/vnd.firecrawl+json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(responseBody)
	}))
	defer local.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Authorization", "Bearer incoming-secret")
	router.NewHandler(router.Config{LocalBaseURL: local.URL}, router.Dependencies{HTTPClient: local.Client()}).ServeHTTP(recorder, request)

	if got, want := recorder.Code, http.StatusCreated; got != want {
		t.Fatalf("status = %d, want %d; body = %q", got, want, recorder.Body.Bytes())
	}
	if got, want := recorder.Header().Get("Content-Type"), "application/vnd.firecrawl+json; charset=utf-8"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	if !bytes.Equal(recorder.Body.Bytes(), responseBody) {
		t.Fatalf("body = %q, want exact bytes %q", recorder.Body.Bytes(), responseBody)
	}
}

func TestScrapeFallsBackToCloudForLocalTransportError(t *testing.T) {
	requestBody := []byte(`{"url":"https://example.test","formats":["markdown"],"unknown":true}`)
	cloudBody := []byte(`{"success":true,"data":{"markdown":"cloud"},"future":42}`)
	closedLocal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	localURL := closedLocal.URL
	closedLocal.Close()

	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer configured-key"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		gotBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read cloud body: %v", err)
		}
		if !bytes.Equal(gotBody, requestBody) {
			t.Errorf("body = %q, want exact bytes %q", gotBody, requestBody)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(cloudBody)
	}))
	defer cloud.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer client-secret")
	router.NewHandler(router.Config{LocalBaseURL: localURL, CloudBaseURL: cloud.URL, CloudAPIKey: "configured-key"}, router.Dependencies{HTTPClient: cloud.Client()}).ServeHTTP(recorder, request)

	if got, want := recorder.Code, http.StatusAccepted; got != want {
		t.Fatalf("status = %d, want %d; body = %q", got, want, recorder.Body.Bytes())
	}
	if got, want := recorder.Header().Get("Content-Type"), "application/problem+json"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	if !bytes.Equal(recorder.Body.Bytes(), cloudBody) {
		t.Fatalf("body = %q, want exact bytes %q", recorder.Body.Bytes(), cloudBody)
	}
}

func TestScrapeLocalTransportErrorRejectsInvalidCloudResponse(t *testing.T) {
	tests := []struct {
		name             string
		cloudStatus      int
		cloudBody        string
		maxResponseBytes int64
	}{
		{name: "HTTP 500", cloudStatus: http.StatusInternalServerError, cloudBody: `{"success":true,"data":{"markdown":"cloud"}}`},
		{name: "success false", cloudStatus: http.StatusOK, cloudBody: `{"success":false,"error":"cloud scrape failed"}`},
		{name: "malformed JSON", cloudStatus: http.StatusOK, cloudBody: `{"success":true`},
		{name: "oversized response", cloudStatus: http.StatusOK, cloudBody: `{"success":true,"data":{"markdown":"response exceeds configured limit"}}`, maxResponseBytes: 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closedLocal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			localURL := closedLocal.URL
			closedLocal.Close()
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.cloudStatus)
				_, _ = w.Write([]byte(tt.cloudBody))
			}))
			defer cloud.Close()

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewReader([]byte(`{"url":"https://example.test"}`)))
			router.NewHandler(router.Config{
				LocalBaseURL:     localURL,
				CloudBaseURL:     cloud.URL,
				CloudAPIKey:      "configured-key",
				MaxResponseBytes: tt.maxResponseBytes,
			}, router.Dependencies{HTTPClient: cloud.Client()}).ServeHTTP(recorder, request)

			if got, want := recorder.Code, http.StatusBadGateway; got != want {
				t.Fatalf("status = %d, want %d; body = %q", got, want, recorder.Body.Bytes())
			}
			if got, want := recorder.Header().Get("Content-Type"), "application/json"; got != want {
				t.Fatalf("Content-Type = %q, want %q", got, want)
			}
			if got, want := recorder.Body.String(), "{\"error\":\"local and cloud upstream requests failed\"}\n"; got != want {
				t.Fatalf("body = %q, want %q", got, want)
			}
		})
	}
}

func TestScrapeKeepsAttemptContextAliveWhileReadingStreamingResponse(t *testing.T) {
	localBody := []byte(`{"success":true,"data":{"markdown":"# streamed Wikipedia article"}}`)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(25 * time.Millisecond)
		_, _ = w.Write(localBody)
	}))
	defer local.Close()

	recorder := httptest.NewRecorder()
	router.NewHandler(
		router.Config{LocalBaseURL: local.URL, HTTPTimeout: time.Second, MaxResponseBytes: 1 << 20},
		router.Dependencies{HTTPClient: local.Client()},
	).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/scrape", strings.NewReader(`{"url":"https://en.wikipedia.org/wiki/Test","formats":["markdown"]}`)))

	if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), localBody) {
		t.Fatalf("response = (%d, %q), want (200, %q)", recorder.Code, recorder.Body.Bytes(), localBody)
	}
}

func TestScrapeNon2xxFallbackExcludesTerminalStatusesAndErrors(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantCloudCall bool
	}{
		{name: "server error", status: 502, body: `{"success":false,"error":"upstream unavailable"}`, wantCloudCall: true},
		{name: "too many requests", status: 429, body: `{"success":false,"error":"rate limit"}`, wantCloudCall: true},
		{name: "bad request", status: 400, body: `{"success":false,"error":"bad request"}`},
		{name: "bad request anti-bot block", status: 400, body: `{"success":false,"error":"403-like anti-bot challenge"}`, wantCloudCall: true},
		{name: "bad request timeout", status: 400, body: `{"success":false,"error":"renderer timed out"}`, wantCloudCall: true},
		{name: "bad request malformed", status: 400, body: `{"success":false,"error":"malformed request body"}`},
		{name: "bad request unsupported", status: 400, body: `{"success":false,"error":"unsupported content type"}`},
		{name: "unprocessable unsupported content type code", status: 422, body: `{"success":false,"error":"Unsupported content type: application/vnd.openxmlformats-officedocument.wordprocessingml.document","error_code":"unsupported_content_type"}`, wantCloudCall: true},
		{name: "unprocessable unsupported URL scheme", status: 422, body: `{"success":false,"error":"Invalid URL: unsupported scheme ftp","error_code":"invalid_request"}`},
		{name: "bad request unsupported format", status: 400, body: `{"success":false,"error":"unsupported format: screenshot@fullPage","error_code":"invalid_request"}`},
		{name: "unprocessable malformed with content type code", status: 422, body: `{"success":false,"error":"malformed response","error_code":"unsupported_content_type_mismatch"}`},
		{name: "unauthorized", status: 401, body: `{"success":false,"error":"unauthorized"}`},
		{name: "not found", status: 404, body: `{"success":false,"error":"not found"}`},
		{name: "gone", status: 410, body: `{"success":false,"error":"gone"}`},
		{name: "unprocessable", status: 422, body: `{"success":false,"error":"invalid options"}`},
		{name: "unprocessable rate limit", status: 422, body: `{"success":false,"error":"rate-limit exceeded"}`, wantCloudCall: true},
		{name: "unprocessable renderer exhausted", status: 422, body: `{"success":false,"error":"renderer exhausted"}`, wantCloudCall: true},
		{name: "unprocessable invalid extraction", status: 422, body: `{"success":false,"error":"invalid extraction result"}`, wantCloudCall: true},
		{name: "unprocessable connection reset", status: 422, body: `{"success":false,"error":"connection reset by peer"}`, wantCloudCall: true},
		{name: "unprocessable invalid URL", status: 422, body: `{"success":false,"error":"Invalid URL supplied"}`},
		{name: "unprocessable robots", status: 422, body: `{"success":false,"error":"Blocked by robots.txt"}`},
		{name: "unprocessable login required", status: 422, body: `{"success":false,"error":"Login required: reddit.com served a sign-in page instead of the requested content","error_code":"login_required"}`},
		{name: "robots", status: 403, body: `{"success":false,"error":"Blocked by robots.txt"}`},
		{name: "invalid URL", status: 500, body: `{"success":false,"error":"Invalid URL supplied"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localBody := []byte(tt.body)
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tt.status)
				_, _ = w.Write(localBody)
			}))
			defer local.Close()
			cloudCalls := 0
			cloudBody := []byte(`{"success":true,"data":{"markdown":"cloud"}}`)
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cloudCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(cloudBody)
			}))
			defer cloud.Close()

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewReader([]byte(`{"url":"https://example.test"}`)))
			router.NewHandler(router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"}, router.Dependencies{HTTPClient: local.Client()}).ServeHTTP(recorder, request)

			if got := cloudCalls > 0; got != tt.wantCloudCall {
				t.Fatalf("cloud called = %v, want %v; response = %q", got, tt.wantCloudCall, recorder.Body.Bytes())
			}
			wantStatus, wantBody := tt.status, localBody
			if tt.wantCloudCall {
				wantStatus, wantBody = http.StatusOK, cloudBody
			}
			if recorder.Code != wantStatus || !bytes.Equal(recorder.Body.Bytes(), wantBody) {
				t.Fatalf("response = (%d, %q), want (%d, %q)", recorder.Code, recorder.Body.Bytes(), wantStatus, wantBody)
			}
		})
	}
}

func TestScrapeSuccessFalseFallsBackOnlyForRetryableIndicators(t *testing.T) {
	tests := []struct {
		name          string
		localBody     string
		wantCloudCall bool
	}{
		{name: "anti-bot", localBody: `{"success":false,"error":"anti-bot detected"}`, wantCloudCall: true},
		{name: "blocked", localBody: `{"success":false,"warning":"navigation BLOCKED"}`, wantCloudCall: true},
		{name: "Cloudflare", localBody: `{"success":false,"error":"Cloudflare interstitial"}`, wantCloudCall: true},
		{name: "captcha", localBody: `{"success":false,"warning":"captcha required"}`, wantCloudCall: true},
		{name: "challenge", localBody: `{"success":false,"error":"browser challenge"}`, wantCloudCall: true},
		{name: "rate limit", localBody: `{"success":false,"error":"rate-limit exceeded"}`, wantCloudCall: true},
		{name: "timeout", localBody: `{"success":false,"warning":"renderer timeout"}`, wantCloudCall: true},
		{name: "renderer exhausted", localBody: `{"success":false,"error":"renderer exhausted"}`, wantCloudCall: true},
		{name: "invalid extraction", localBody: `{"success":false,"error":"invalid extraction result"}`, wantCloudCall: true},
		{name: "connection reset", localBody: `{"success":false,"error":"connection reset by peer"}`, wantCloudCall: true},
		{name: "nonretryable failure", localBody: `{"success":false,"error":"unsupported content type"}`},
		{name: "CRW 1.5 empty result", localBody: `{"success":false,"error":"No content could be extracted from the page","data":{"markdown":"","metadata":{"statusCode":200}}}`, wantCloudCall: true},
		{name: "CRW 1.5 structural failure", localBody: `{"success":false,"error":"No usable content could be extracted (structural_failure)","data":{"markdown":"Loading...","metadata":{"statusCode":200}}}`, wantCloudCall: true},
		{name: "CRW 1.5 parked domain", localBody: `{"success":false,"error":"No usable content could be extracted (parked_domain)","data":{"metadata":{"statusCode":200}}}`, wantCloudCall: true},
		{name: "unindicated failure with empty markdown", localBody: `{"success":false,"error":"extraction produced nothing","data":{"markdown":"  "}}`, wantCloudCall: true},
		{name: "unindicated failure with missing data", localBody: `{"success":false,"error":"extraction produced nothing"}`, wantCloudCall: true},
		{name: "unindicated failure keeps nonempty markdown", localBody: `{"success":false,"error":"extraction produced nothing","data":{"markdown":"# partial"}}`},
		{name: "target not found with empty markdown", localBody: `{"success":false,"error":"Target returned HTTP 404","data":{"markdown":"","metadata":{"statusCode":404}}}`},
		{name: "target gone with empty markdown", localBody: `{"success":false,"error":"Target returned HTTP 410","data":{"metadata":{"statusCode":410}}}`},
		{name: "target unauthorized with empty markdown", localBody: `{"success":false,"error":"Target returned HTTP 401","data":{"metadata":{"statusCode":401}}}`},
		{name: "success true ignores warning", localBody: `{"success":true,"warning":"blocked asset","data":{"markdown":"ok"}}`},
		{name: "malformed JSON", localBody: `{"success":false,"error":"timeout"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localBody := []byte(tt.localBody)
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(localBody)
			}))
			defer local.Close()
			cloudCalls := 0
			cloudBody := []byte(`{"success":true,"data":{"markdown":"cloud"}}`)
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cloudCalls++
				_, _ = w.Write(cloudBody)
			}))
			defer cloud.Close()

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewReader([]byte(`{"url":"https://example.test"}`)))
			router.NewHandler(router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"}, router.Dependencies{HTTPClient: local.Client()}).ServeHTTP(recorder, request)

			if got := cloudCalls > 0; got != tt.wantCloudCall {
				t.Fatalf("cloud called = %v, want %v; response = %q", got, tt.wantCloudCall, recorder.Body.Bytes())
			}
			wantBody := localBody
			if tt.wantCloudCall {
				wantBody = cloudBody
			}
			if !bytes.Equal(recorder.Body.Bytes(), wantBody) {
				t.Fatalf("body = %q, want exact bytes %q", recorder.Body.Bytes(), wantBody)
			}
		})
	}
}

func TestScrapeSuccessFalseDeterministicErrorDoesNotFallbackForEmptyMarkdown(t *testing.T) {
	for _, localBody := range [][]byte{
		[]byte(`{"success":false,"error":"Invalid URL supplied"}`),
		[]byte(`{"success":false,"error":"unsupported content type"}`),
	} {
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(localBody)
		}))
		cloudCalls := 0
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			cloudCalls++
			_, _ = w.Write([]byte(`{"success":true,"data":{"markdown":"cloud"}}`))
		}))

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v2/scrape", strings.NewReader(`{"url":"not a URL","formats":["markdown"]}`))
		router.NewHandler(
			router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"},
			router.Dependencies{HTTPClient: local.Client()},
		).ServeHTTP(recorder, request)

		cloud.Close()
		local.Close()
		if cloudCalls != 0 {
			t.Errorf("local body %q: cloud calls = %d, want 0", localBody, cloudCalls)
		}
		if !bytes.Equal(recorder.Body.Bytes(), localBody) {
			t.Errorf("body = %q, want exact local bytes %q", recorder.Body.Bytes(), localBody)
		}
	}
}

func TestScrapeEmptyMarkdownFallsBackOnlyWhenMarkdownRequested(t *testing.T) {
	tests := []struct {
		name          string
		requestBody   string
		localBody     string
		wantCloudCall bool
	}{
		{name: "string markdown", requestBody: `{"url":"https://example.test","formats":["markdown"]}`, localBody: `{"success":true,"data":{"markdown":""}}`, wantCloudCall: true},
		{name: "object markdown", requestBody: `{"url":"https://example.test","formats":[{"type":"markdown","options":{"future":true}}]}`, localBody: `{"success":true,"data":{}}`, wantCloudCall: true},
		{name: "whitespace is empty", requestBody: `{"formats":["html","markdown"]}`, localBody: `{"success":true,"data":{"markdown":"  \n\t"}}`, wantCloudCall: true},
		{name: "markdown not requested", requestBody: `{"formats":["html"]}`, localBody: `{"success":true,"data":{"markdown":""}}`},
		{name: "formats absent defaults to markdown", requestBody: `{"url":"https://example.test"}`, localBody: `{"success":true,"data":{}}`, wantCloudCall: true},
		{name: "empty formats defaults to markdown", requestBody: `{"url":"https://example.test","formats":[]}`, localBody: `{"success":true,"data":{}}`, wantCloudCall: true},
		{name: "nonempty markdown", requestBody: `{"formats":["markdown"]}`, localBody: `{"success":true,"data":{"markdown":"# local"}}`},
		{name: "malformed request JSON", requestBody: `{"formats":["markdown"]`, localBody: `{"success":true,"data":{"markdown":""}}`},
		{name: "malformed local JSON", requestBody: `{"formats":["markdown"]}`, localBody: `{"success":true,"data":{"markdown":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localBody := []byte(tt.localBody)
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(localBody)
			}))
			defer local.Close()
			cloudCalls := 0
			cloudBody := []byte(`{"success":true,"data":{"markdown":"cloud"}}`)
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cloudCalls++
				_, _ = w.Write(cloudBody)
			}))
			defer cloud.Close()

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewReader([]byte(tt.requestBody)))
			router.NewHandler(router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"}, router.Dependencies{HTTPClient: local.Client()}).ServeHTTP(recorder, request)

			if got := cloudCalls > 0; got != tt.wantCloudCall {
				t.Fatalf("cloud called = %v, want %v; response = %q", got, tt.wantCloudCall, recorder.Body.Bytes())
			}
			wantBody := localBody
			if tt.wantCloudCall {
				wantBody = cloudBody
			}
			if !bytes.Equal(recorder.Body.Bytes(), wantBody) {
				t.Fatalf("body = %q, want exact bytes %q", recorder.Body.Bytes(), wantBody)
			}
		})
	}
}

func TestScrapeCloudEmptyMarkdownIsInvalidForDefaultFormats(t *testing.T) {
	tests := []struct {
		name        string
		requestBody string
		wantCloud   bool
	}{
		{name: "formats omitted", requestBody: `{"url":"https://example.test"}`},
		{name: "formats empty", requestBody: `{"url":"https://example.test","formats":[]}`},
		{name: "explicit html", requestBody: `{"url":"https://example.test","formats":["html"]}`, wantCloud: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localBody := []byte(`{"success":false,"error":"renderer timeout","source":"local"}`)
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write(localBody)
			}))
			defer local.Close()
			cloudBody := []byte(`{"success":true,"data":{"html":"<main>cloud</main>"}}`)
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(cloudBody)
			}))
			defer cloud.Close()

			recorder := httptest.NewRecorder()
			router.NewHandler(
				router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"},
				router.Dependencies{HTTPClient: local.Client()},
			).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/scrape", strings.NewReader(tt.requestBody)))

			wantBody := localBody
			if tt.wantCloud {
				wantBody = cloudBody
			}
			if !bytes.Equal(recorder.Body.Bytes(), wantBody) {
				t.Fatalf("body = %q, want %q", recorder.Body.Bytes(), wantBody)
			}
		})
	}
}

func TestScrapeCloudFailureReturnsCapturedLocalResponseExactly(t *testing.T) {
	localBody := []byte(`{"success":false,"error":"renderer timeout","unknown":{"truth":1}}`)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(localBody)
	}))
	defer local.Close()
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"success":false,"error":"cloud unavailable"}`))
	}))
	defer cloud.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewReader([]byte(`{"url":"https://example.test"}`)))
	router.NewHandler(router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"}, router.Dependencies{HTTPClient: local.Client()}).ServeHTTP(recorder, request)

	if got, want := recorder.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := recorder.Header().Get("Content-Type"), "application/problem+json; charset=utf-8"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	if !bytes.Equal(recorder.Body.Bytes(), localBody) {
		t.Fatalf("body = %q, want exact local bytes %q", recorder.Body.Bytes(), localBody)
	}
}

func TestScrapeCloudSuccessFalseReturnsCapturedLocalResponseExactly(t *testing.T) {
	localBody := []byte(`{"success":false,"error":"local timeout","source":"local"}`)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write(localBody)
	}))
	defer local.Close()
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"error":"cloud scrape failed"}`))
	}))
	defer cloud.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewReader([]byte(`{"url":"https://example.test"}`)))
	router.NewHandler(router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"}, router.Dependencies{HTTPClient: local.Client()}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusGatewayTimeout || recorder.Header().Get("Content-Type") != "application/problem+json" || !bytes.Equal(recorder.Body.Bytes(), localBody) {
		t.Fatalf("response = (%d, %q, %q), want exact local response", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Bytes())
	}
}

func TestScrapeMalformedCloudJSONReturnsCapturedLocalResponseExactly(t *testing.T) {
	localBody := []byte(`{"success":false,"error":"local timeout","source":"local"}`)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write(localBody)
	}))
	defer local.Close()
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true`))
	}))
	defer cloud.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v2/scrape", bytes.NewReader([]byte(`{"url":"https://example.test"}`)))
	router.NewHandler(router.Config{LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "key"}, router.Dependencies{HTTPClient: local.Client()}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusGatewayTimeout || recorder.Header().Get("Content-Type") != "application/problem+json" || !bytes.Equal(recorder.Body.Bytes(), localBody) {
		t.Fatalf("response = (%d, %q, %q), want exact local response", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Bytes())
	}
}

func TestOversizedLocalResponseUsesCloudOrReturnsJSONBadGateway(t *testing.T) {
	tests := []struct {
		name             string
		path             string
		request          string
		cloudBody        string
		cloud            bool
		denyBudget       bool
		cloudValid       bool
		wantBudgetDenied bool
		wantBody         string
	}{
		{name: "search valid cloud", path: "/v2/search", request: `{"query":"large"}`, cloud: true, cloudValid: true, cloudBody: `{"success":true,"data":{"web":[{}]}}`, wantBody: `{"success":true,"data":{"web":[{}]}}`},
		{name: "search invalid cloud", path: "/v2/search", request: `{"query":"large"}`, cloud: true, cloudBody: `{"success":false}`, wantBody: "{\"error\":\"local and cloud upstream requests failed\"}\n"},
		{name: "search budget denied", path: "/v2/search", request: `{"query":"large"}`, cloud: true, denyBudget: true, wantBudgetDenied: true, wantBody: "{\"error\":\"local upstream response failed; cloud fallback skipped: credit budget exceeded\"}\n"},
		{name: "search cloud disabled", path: "/v2/search", request: `{"query":"large"}`, wantBody: "{\"error\":\"local upstream response failed\"}\n"},
		{name: "scrape valid cloud", path: "/v2/scrape", request: `{"url":"https://example.test","formats":["markdown"]}`, cloud: true, cloudValid: true, cloudBody: `{"success":true,"data":{"markdown":"ok"}}`, wantBody: `{"success":true,"data":{"markdown":"ok"}}`},
		{name: "scrape invalid cloud", path: "/v2/scrape", request: `{"url":"https://example.test","formats":["markdown"]}`, cloud: true, cloudBody: `{"success":false}`, wantBody: "{\"error\":\"local and cloud upstream requests failed\"}\n"},
		{name: "scrape budget denied", path: "/v2/scrape", request: `{"url":"https://example.test","formats":["markdown"]}`, cloud: true, denyBudget: true, wantBudgetDenied: true, wantBody: "{\"error\":\"local upstream response failed; cloud fallback skipped: credit budget exceeded\"}\n"},
		{name: "scrape cloud disabled", path: "/v2/scrape", request: `{"url":"https://example.test","formats":["markdown"]}`, wantBody: "{\"error\":\"local upstream response failed\"}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := metricspkg.NewRegistry()
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(bytes.Repeat([]byte("x"), 129))
			}))
			defer local.Close()
			cloudCalls := 0
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cloudCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.cloudBody))
			}))
			defer cloud.Close()

			config := router.Config{LocalBaseURL: local.URL, MaxResponseBytes: 128}
			var ledger budgetpkg.Ledger
			if tt.cloud {
				config.CloudBaseURL, config.CloudAPIKey = cloud.URL, "key"
			}
			if tt.denyBudget {
				memoryLedger := budgetpkg.NewMemory(1, 1, time.Now)
				if err := memoryLedger.Reserve(context.Background(), 1); err != nil {
					t.Fatal(err)
				}
				ledger = memoryLedger
			}
			recorder := httptest.NewRecorder()
			router.NewHandler(config, router.Dependencies{HTTPClient: local.Client(), Budget: ledger, Metrics: registry}).ServeHTTP(
				recorder, httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.request)),
			)

			wantStatus := http.StatusBadGateway
			if tt.cloudValid {
				wantStatus = http.StatusOK
			}
			if recorder.Code != wantStatus || recorder.Header().Get("Content-Type") != "application/json" || recorder.Body.String() != tt.wantBody {
				t.Fatalf("response = (%d, %q, %q), want (%d, JSON, %q)", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String(), wantStatus, tt.wantBody)
			}
			wantCloudCalls := 0
			if tt.cloud && !tt.denyBudget {
				wantCloudCalls = 1
			}
			if cloudCalls != wantCloudCalls {
				t.Fatalf("cloud calls = %d, want %d", cloudCalls, wantCloudCalls)
			}
			if tt.wantBudgetDenied {
				endpoint := strings.TrimPrefix(tt.path, "/v2/")
				wantMetric := `web_retrieval_cloud_budget_denied_total{endpoint="` + endpoint + `"} 1` + "\n"
				if metrics := registry.PrometheusText(); !strings.Contains(metrics, wantMetric) {
					t.Fatalf("metrics missing %q:\n%s", strings.TrimSpace(wantMetric), metrics)
				}
			}
		})
	}
}
