package router_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	budgetpkg "web-retrieval/internal/budget"
	cachepkg "web-retrieval/internal/cache"
	"web-retrieval/internal/router"
	flightpkg "web-retrieval/internal/singleflight"
)

func TestMCPInitializePingAndToolDiscovery(t *testing.T) {
	handler := router.NewHandler(router.Config{MCPEnabled: true, APIKey: "secret", ServerVersion: "1.2.3"}, router.Dependencies{})

	initialize := callMCP(t, handler, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if initialize.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body %s", initialize.Code, initialize.Body.String())
	}
	if got := initialize.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("initialize Content-Type = %q, want application/json", got)
	}
	var initialized struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools map[string]any `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initialize.Body.Bytes(), &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.JSONRPC != "2.0" || initialized.ID != 1 || initialized.Result.ProtocolVersion != "2025-06-18" {
		t.Fatalf("initialize response = %#v", initialized)
	}
	if initialized.Result.ServerInfo.Name != "fireghost" || initialized.Result.ServerInfo.Version != "1.2.3" {
		t.Fatalf("server info = %#v", initialized.Result.ServerInfo)
	}
	if initialized.Result.Capabilities.Tools == nil {
		t.Fatal("initialize response did not advertise tools capability")
	}

	ping := callMCP(t, handler, `{"jsonrpc":"2.0","id":"ping-id","method":"ping"}`)
	assertJSONEqual(t, ping.Body.Bytes(), `{"jsonrpc":"2.0","id":"ping-id","result":{}}`)

	list := callMCP(t, handler, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	var discovered struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &discovered); err != nil {
		t.Fatal(err)
	}
	if len(discovered.Result.Tools) != 2 || discovered.Result.Tools[0].Name != "search" || discovered.Result.Tools[1].Name != "scrape" {
		t.Fatalf("discovered tools = %#v", discovered.Result.Tools)
	}
	for _, tool := range discovered.Result.Tools {
		if tool.InputSchema["type"] != "object" || tool.InputSchema["additionalProperties"] != true {
			t.Fatalf("%s input schema is not permissive: %#v", tool.Name, tool.InputSchema)
		}
	}
}

func TestMCPToolsShareExactRESTOperationsAndCache(t *testing.T) {
	searchBody := `{"success":true,"data":{"web":[{"url":"https://example.test"}]},"unknown":{"kept":true}}`
	scrapeBody := `{"success":true,"data":{"markdown":"hello"},"unknown":[1,2]}`
	calls := map[string]int{}
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.Path]++
		request, _ := io.ReadAll(r.Body)
		if !bytes.Contains(request, []byte(`"extension":"preserved"`)) {
			t.Errorf("upstream body lost unknown argument: %s", request)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/search" {
			_, _ = io.WriteString(w, searchBody)
		} else {
			_, _ = io.WriteString(w, scrapeBody)
		}
	}))
	defer local.Close()
	handler := router.NewHandler(router.Config{
		MCPEnabled: true, APIKey: "secret", LocalBaseURL: local.URL,
		SearchTTL: time.Minute, ScrapeTTL: time.Minute, MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20,
	}, router.Dependencies{
		HTTPClient: local.Client(), Cache: &testCache{entries: make(map[string]cachepkg.Entry)},
		FlightGroup: flightpkg.New[cachepkg.Entry](),
	})

	for _, test := range []struct {
		name, path, upstreamBody string
	}{
		{name: "search", path: "/v2/search", upstreamBody: searchBody},
		{name: "scrape", path: "/v2/scrape", upstreamBody: scrapeBody},
	} {
		arguments := `{"query":"x","url":"https://example.test","extension":"preserved"}`
		restRequest := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(arguments))
		restRequest.Header.Set("Authorization", "Bearer secret")
		rest := httptest.NewRecorder()
		handler.ServeHTTP(rest, restRequest)
		if rest.Body.String() != test.upstreamBody {
			t.Fatalf("%s REST body = %q, want exact %q", test.name, rest.Body.String(), test.upstreamBody)
		}

		mcp := callMCP(t, handler, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"`+test.name+`","arguments":`+arguments+`}}`)
		var response struct {
			Result struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				StructuredContent json.RawMessage `json:"structuredContent"`
				IsError           bool            `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(mcp.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Result.IsError || len(response.Result.Content) != 1 || response.Result.Content[0].Type != "text" || response.Result.Content[0].Text != test.upstreamBody {
			t.Fatalf("%s MCP result = %#v; body %s", test.name, response.Result, mcp.Body.String())
		}
		assertJSONEqual(t, response.Result.StructuredContent, test.upstreamBody)
		if calls[test.path] != 1 {
			t.Fatalf("%s upstream calls = %d, want shared REST/MCP cache hit", test.name, calls[test.path])
		}
	}
}

func TestMCPToolErrorsAndInvalidCalls(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"success":false,"error":"upstream unavailable"}`)
	}))
	defer local.Close()
	handler := router.NewHandler(router.Config{MCPEnabled: true, APIKey: "secret", LocalBaseURL: local.URL, MaxResponseBytes: 1 << 20}, router.Dependencies{HTTPClient: local.Client()})

	failed := callMCP(t, handler, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"x"}}}`)
	var toolResponse struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(failed.Body.Bytes(), &toolResponse); err != nil {
		t.Fatal(err)
	}
	if !toolResponse.Result.IsError || len(toolResponse.Result.Content) != 1 || !strings.Contains(toolResponse.Result.Content[0].Text, "upstream unavailable") {
		t.Fatalf("failed tool result = %#v; body %s", toolResponse.Result, failed.Body.String())
	}

	for _, body := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"missing","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":"bad"}}`,
	} {
		response := callMCP(t, handler, body)
		var rpc struct {
			Error *mcpTestError `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &rpc); err != nil {
			t.Fatal(err)
		}
		if rpc.Error == nil || rpc.Error.Code != -32602 {
			t.Fatalf("invalid call response = %s, want -32602", response.Body.String())
		}
	}
}

type mcpTestError struct {
	Code int `json:"code"`
}

func TestMCPToolsPreserveFallbackBudgetAndResponseLimits(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"success":false,"error":"blocked"}`)
	}))
	defer local.Close()
	cloudCalls := 0
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalls++
		_, _ = io.WriteString(w, `{"success":true,"data":{"web":[{"url":"https://cloud.test"}]}}`)
	}))
	defer cloud.Close()

	success := router.NewHandler(router.Config{
		MCPEnabled: true, APIKey: "secret", LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key",
		SearchEstimatedCredits: 2, MaxResponseBytes: 1 << 20,
	}, router.Dependencies{HTTPClient: local.Client(), Budget: budgetpkg.NewMemory(2, 0, time.Now)})
	fallback := callMCP(t, success, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"x"}}}`)
	if !strings.Contains(fallback.Body.String(), `https://cloud.test`) || strings.Contains(fallback.Body.String(), `"isError":true`) || cloudCalls != 1 {
		t.Fatalf("fallback response = %s, cloud calls = %d", fallback.Body.String(), cloudCalls)
	}

	denied := router.NewHandler(router.Config{
		MCPEnabled: true, APIKey: "secret", LocalBaseURL: local.URL, CloudBaseURL: cloud.URL, CloudAPIKey: "cloud-key",
		SearchEstimatedCredits: 2, MaxResponseBytes: 1 << 20,
	}, router.Dependencies{HTTPClient: local.Client(), Budget: budgetpkg.NewMemory(1, 0, time.Now)})
	denial := callMCP(t, denied, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"denied"}}}`)
	if !strings.Contains(denial.Body.String(), `"isError":true`) || !strings.Contains(denial.Body.String(), "credit budget exceeded") || cloudCalls != 1 {
		t.Fatalf("budget denial response = %s, cloud calls = %d", denial.Body.String(), cloudCalls)
	}

	limited := router.NewHandler(router.Config{MCPEnabled: true, APIKey: "secret", LocalBaseURL: cloud.URL, MaxResponseBytes: 8}, router.Dependencies{HTTPClient: cloud.Client()})
	oversize := callMCP(t, limited, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"large"}}}`)
	if !strings.Contains(oversize.Body.String(), `"isError":true`) || !strings.Contains(oversize.Body.String(), "local upstream response failed") {
		t.Fatalf("oversize response = %s", oversize.Body.String())
	}
}

func TestMCPNotificationsAreAcceptedWithoutResponse(t *testing.T) {
	handler := router.NewHandler(router.Config{MCPEnabled: true, APIKey: "secret"}, router.Dependencies{})
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"done"}}`,
	} {
		response := callMCP(t, handler, body)
		if response.Code != http.StatusAccepted || response.Body.Len() != 0 {
			t.Fatalf("notification response = %d %q, want 202 empty", response.Code, response.Body.String())
		}
	}
}

func TestMCPJSONRPCValidation(t *testing.T) {
	handler := router.NewHandler(router.Config{MCPEnabled: true, APIKey: "secret", MaxRequestBytes: 256}, router.Dependencies{})
	tests := []struct {
		name, body string
		code       int
	}{
		{name: "malformed", body: `{`, code: -32700},
		{name: "batch unsupported", body: `[]`, code: -32600},
		{name: "wrong version", body: `{"jsonrpc":"1.0","id":1,"method":"ping"}`, code: -32600},
		{name: "null id", body: `{"jsonrpc":"2.0","id":null,"method":"ping"}`, code: -32600},
		{name: "object id", body: `{"jsonrpc":"2.0","id":{},"method":"ping"}`, code: -32600},
		{name: "unknown method", body: `{"jsonrpc":"2.0","id":1,"method":"unknown"}`, code: -32601},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := callMCP(t, handler, test.body)
			if response.Code != http.StatusOK {
				t.Fatalf("HTTP status = %d, want 200", response.Code)
			}
			var rpc struct {
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &rpc); err != nil {
				t.Fatal(err)
			}
			if rpc.Error.Code != test.code {
				t.Fatalf("error code = %d, want %d; body %s", rpc.Error.Code, test.code, response.Body.String())
			}
		})
	}

	tooLarge := callMCP(t, handler, `{"jsonrpc":"2.0","id":1,"method":"ping","padding":"`+string(bytes.Repeat([]byte("x"), 300))+`"}`)
	if tooLarge.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413", tooLarge.Code)
	}
}

func callMCP(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("invalid response JSON %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(gotValue)
	wantJSON, _ := json.Marshal(wantValue)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("JSON = %s, want %s", gotJSON, wantJSON)
	}
}
