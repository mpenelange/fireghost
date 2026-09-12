package router_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"web-retrieval/internal/router"
)

func TestMCPInitializePingAndToolDiscovery(t *testing.T) {
	handler := router.NewHandler(router.Config{MCPEnabled: true, APIKey: "secret", ServerVersion: "1.2.3"}, router.Dependencies{})

	initialize := callMCP(t, handler, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if initialize.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body %s", initialize.Code, initialize.Body.String())
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
