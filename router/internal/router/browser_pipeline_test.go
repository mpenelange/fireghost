package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	budgetpkg "web-retrieval/internal/budget"
	cachepkg "web-retrieval/internal/cache"
	metricspkg "web-retrieval/internal/metrics"
	"web-retrieval/internal/router"
	flightpkg "web-retrieval/internal/singleflight"
)

const pipelinePath = "/v2/browser/scrape"
const pipelineArguments = `{"url":"https://example.test/thread","profile":"redditThread","extension":{"kept":true}}`
const pipelinePartial = `{"success":true,"data":{"markdown":"# Thread\nComment","json":{"comments":[{"id":"t1_c","parentId":"t3_p"}]},"metadata":{"pipeline":"browser-v1","profile":"redditThread","complete":false,"stopReason":"maxItems","itemsCollected":1}},"warnings":["Comments remain unloaded"],"unknown":{"preserved":true}}`

func enableBrowserPipeline(t *testing.T, config *router.Config) {
	t.Helper()
	config.BrowserPipelineEnabled = true
}

func pipelineREST(handler http.Handler, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, pipelinePath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestBrowserPipelineDisabledDoesNotExposeRouteOrTool(t *testing.T) {
	handler := router.NewHandler(router.Config{MCPEnabled: true, APIKey: "secret"}, router.Dependencies{})
	if w := pipelineREST(handler, pipelineArguments, "secret"); w.Code != http.StatusNotFound {
		t.Fatalf("disabled route = %d", w.Code)
	}
	w := callMCP(t, handler, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_scrape","arguments":{}}}`)
	if !strings.Contains(w.Body.String(), `"code":-32602`) {
		t.Fatalf("disabled tool response = %s", w.Body.String())
	}
}

func TestBrowserPipelineForwardsPartialResultsWithoutCacheOrCloud(t *testing.T) {
	var localCalls, cloudCalls atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != pipelinePath || string(body) != pipelineArguments {
			t.Errorf("forwarded request = %s %s", r.URL.Path, body)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("inbound bearer forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pipelinePartial)
	}))
	defer local.Close()
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalls.Add(1)
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer cloud.Close()
	config := router.Config{MCPEnabled: true, APIKey: "secret", LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key", SearchTTL: time.Hour, ScrapeTTL: time.Hour, MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20}
	enableBrowserPipeline(t, &config)
	metrics := metricspkg.NewRegistry()
	cache := &pipelineForbiddenCache{t: t}
	handler := router.NewHandler(config, router.Dependencies{HTTPClient: local.Client(), Cache: cache, Metrics: metrics, Budget: budgetpkg.NewMemory(100, 0, time.Now), FlightGroup: flightpkg.New[cachepkg.Entry]()})
	for range 2 {
		w := pipelineREST(handler, pipelineArguments, "secret")
		if w.Code != http.StatusOK || w.Body.String() != pipelinePartial {
			t.Fatalf("REST result = %d %s", w.Code, w.Body.String())
		}
	}
	w := callMCP(t, handler, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_scrape","arguments":`+pipelineArguments+`}}`)
	var rpc struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
			IsError           bool            `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rpc); err != nil {
		t.Fatal(err)
	}
	if rpc.Result.IsError {
		t.Fatalf("partial usable content marked error: %s", w.Body.String())
	}
	assertJSONEqual(t, rpc.Result.StructuredContent, pipelinePartial)
	if localCalls.Load() != 3 || cloudCalls.Load() != 0 {
		t.Fatalf("local=%d cloud=%d", localCalls.Load(), cloudCalls.Load())
	}
	if !strings.Contains(metrics.PrometheusText(), `web_retrieval_local_attempts_total{endpoint="browser_scrape"} 3`) {
		t.Fatal("missing separate bounded browser metric")
	}
}

type pipelineForbiddenCache struct{ t *testing.T }

func (cache *pipelineForbiddenCache) Get(context.Context, string) (cachepkg.Entry, bool, error) {
	cache.t.Error("browser pipeline consulted cache")
	return cachepkg.Entry{}, false, nil
}
func (cache *pipelineForbiddenCache) Set(context.Context, string, cachepkg.Entry) error {
	cache.t.Error("browser pipeline cached result")
	return nil
}

func TestBrowserPipelineNeverFallsBackAndPreservesFailureBodies(t *testing.T) {
	for _, status := range []int{200, 400, 429, 502} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := `{"success":false,"error":"pipeline unavailable","unknown":{"keep":1}}`
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			defer local.Close()
			var cloudCalls atomic.Int32
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { cloudCalls.Add(1) }))
			defer cloud.Close()
			config := router.Config{MCPEnabled: true, APIKey: "secret", LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key", MaxResponseBytes: 1 << 20}
			enableBrowserPipeline(t, &config)
			handler := router.NewHandler(config, router.Dependencies{HTTPClient: local.Client(), Budget: budgetpkg.NewMemory(100, 0, time.Now)})
			w := pipelineREST(handler, pipelineArguments, "secret")
			if w.Code != status || w.Body.String() != body {
				t.Fatalf("failure not preserved: %d %s", w.Code, w.Body.String())
			}
			mcp := callMCP(t, handler, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_scrape","arguments":`+pipelineArguments+`}}`)
			if !strings.Contains(mcp.Body.String(), `"isError":true`) || !strings.Contains(mcp.Body.String(), "pipeline unavailable") {
				t.Fatalf("MCP failure = %s", mcp.Body.String())
			}
			if cloudCalls.Load() != 0 {
				t.Fatal("browser request reached cloud")
			}
		})
	}
}

func TestBrowserPipelineMCPSuccessRequiresPipelineAndContent(t *testing.T) {
	for _, body := range []string{
		`{"data":{"markdown":"ok","metadata":{"pipeline":"browser-v1"}}}`,
		`{"success":false,"data":{"markdown":"ok","metadata":{"pipeline":"browser-v1"}}}`,
		`{"success":true,"data":{"markdown":"ok"}}`,
		`{"success":true,"data":{"markdown":"ok","metadata":{"pipeline":"generic"}}}`,
		`{"success":true,"data":{"markdown":" ","metadata":{"pipeline":"browser-v1"}}}`,
		`{"success":true,"data":{"web":[{"url":"https://example.test"}],"metadata":{"pipeline":"browser-v1"}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer local.Close()
			config := router.Config{MCPEnabled: true, APIKey: "secret", LocalBaseURL: local.URL}
			enableBrowserPipeline(t, &config)
			handler := router.NewHandler(config, router.Dependencies{HTTPClient: local.Client()})
			w := callMCP(t, handler, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_scrape","arguments":`+pipelineArguments+`}}`)
			if !strings.Contains(w.Body.String(), `"isError":true`) || strings.Contains(w.Body.String(), `"structuredContent"`) {
				t.Fatalf("invalid pipeline response marked success: %s", w.Body.String())
			}
		})
	}
}

func TestBrowserPipelineToolPublishesTypedInputSchema(t *testing.T) {
	config := router.Config{MCPEnabled: true, APIKey: "secret"}
	enableBrowserPipeline(t, &config)
	handler := router.NewHandler(config, router.Dependencies{})
	w := callMCP(t, handler, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var rpc struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					AdditionalProperties bool     `json:"additionalProperties"`
					Required             []string `json:"required"`
					Properties           map[string]struct {
						Type    string   `json:"type"`
						Maximum int      `json:"maximum"`
						Default string   `json:"default"`
						Enum    []string `json:"enum"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rpc); err != nil {
		t.Fatal(err)
	}
	if len(rpc.Result.Tools) != 3 || rpc.Result.Tools[2].Name != "browser_scrape" {
		t.Fatalf("tools = %s", w.Body.String())
	}
	schema := rpc.Result.Tools[2].InputSchema
	if schema.AdditionalProperties {
		t.Fatal("browser pipeline schema accepts unknown fields")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "url" || schema.Properties["url"].Type != "string" {
		t.Fatalf("URL schema = %#v", schema)
	}
	profile := schema.Properties["profile"]
	if profile.Type != "string" || profile.Default != "article" || !reflect.DeepEqual(profile.Enum, []string{"article", "redditThread"}) {
		t.Fatalf("profile schema = %#v", profile)
	}
	for name, maximum := range map[string]int{"timeout": 60000, "maxRounds": 100, "maxItems": 1000, "maxBytes": 262144} {
		if field := schema.Properties[name]; field.Type != "integer" || field.Maximum != maximum {
			t.Fatalf("%s schema = %#v", name, field)
		}
	}
}

func TestBrowserPipelineAuthenticationRequestResponseLimitsAndTimeout(t *testing.T) {
	for _, test := range []struct {
		name   string
		config router.Config
		body   string
		key    string
		want   int
		delay  time.Duration
	}{
		{name: "auth", config: router.Config{APIKey: "secret"}, body: pipelineArguments, want: 401},
		{name: "request bound", config: router.Config{MaxRequestBytes: 8}, body: pipelineArguments, want: 413},
		{name: "response bound", config: router.Config{MaxResponseBytes: 8}, body: pipelineArguments, want: 502},
		{name: "timeout", config: router.Config{HTTPTimeout: 5 * time.Millisecond}, body: pipelineArguments, want: 502, delay: 50 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cloudCalls atomic.Int32
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(test.delay)
				_, _ = io.WriteString(w, pipelinePartial)
			}))
			defer local.Close()
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { cloudCalls.Add(1) }))
			defer cloud.Close()
			config := test.config
			config.LocalBaseURL = local.URL
			config.CloudBaseURL = cloud.URL
			config.CloudAPIKey = "cloud-key"
			enableBrowserPipeline(t, &config)
			handler := router.NewHandler(config, router.Dependencies{HTTPClient: local.Client(), Budget: budgetpkg.NewMemory(100, 0, time.Now)})
			w := pipelineREST(handler, test.body, test.key)
			if w.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, test.want, w.Body.String())
			}
			if cloudCalls.Load() != 0 {
				t.Fatal("limited browser request reached cloud")
			}
		})
	}
}

func TestBrowserPipelineAttemptCannotExceedSixtySeconds(t *testing.T) {
	client := &http.Client{Transport: pipelineRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 60*time.Second {
			t.Errorf("browser deadline=%v present=%t exceeds60s", deadline, ok)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(pipelinePartial))}, nil
	})}
	config := router.Config{LocalBaseURL: "http://local.test", HTTPTimeout: 2 * time.Hour}
	enableBrowserPipeline(t, &config)
	handler := router.NewHandler(config, router.Dependencies{HTTPClient: client})
	w := pipelineREST(handler, pipelineArguments, "")
	if w.Code != 200 {
		t.Fatalf("pipeline attempt=%d %s", w.Code, w.Body.String())
	}
}

type pipelineRoundTripper func(*http.Request) (*http.Response, error)

func (transport pipelineRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func TestBrowserPipelineCoalescesConcurrentRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = io.WriteString(w, pipelinePartial)
	}))
	defer local.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	config := router.Config{LocalBaseURL: local.URL, HTTPTimeout: time.Second}
	enableBrowserPipeline(t, &config)
	flights := flightpkg.New[cachepkg.Entry]()
	handler := router.NewHandler(config, router.Dependencies{HTTPClient: local.Client(), FlightGroup: flights})
	done := make(chan *httptest.ResponseRecorder, 2)
	go func() { done <- pipelineREST(handler, pipelineArguments, "") }()
	<-started
	go func() { done <- pipelineREST(handler, pipelineArguments, "") }()
	deadline := time.Now().Add(time.Second)
	for flights.Participants() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("follower did not join pipeline flight")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for range 2 {
		w := <-done
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), []byte(pipelinePartial)) {
			t.Fatalf("coalesced result=%d %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want1", calls.Load())
	}
}
