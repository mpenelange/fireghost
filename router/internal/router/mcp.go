package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const mcpProtocolVersion = "2025-06-18"

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func serveMCP(w http.ResponseWriter, r *http.Request, config Config, runOperation operationRunner) {
	body, err := readRequestBody(r.Body, config.MaxRequestBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		writeMCPError(w, nil, -32700, "Parse error")
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeMCPError(w, nil, -32700, "Parse error")
		return
	}
	var request mcpRequest
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &request) != nil {
		writeMCPError(w, nil, -32600, "Invalid Request")
		return
	}
	if request.JSONRPC != "2.0" || request.Method == "" || !validMCPID(request.ID) {
		writeMCPError(w, nil, -32600, "Invalid Request")
		return
	}
	if len(request.ID) == 0 {
		if request.Method == "notifications/initialized" || request.Method == "notifications/cancelled" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch request.Method {
	case "initialize":
		writeMCPResult(w, request.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "fireghost", "version": normalizedServerVersion(config.ServerVersion)},
		})
	case "ping":
		writeMCPResult(w, request.ID, map[string]any{})
	case "tools/list":
		writeMCPResult(w, request.ID, map[string]any{"tools": mcpTools(config)})
	case "tools/call":
		serveMCPToolCall(w, r, request, config, runOperation)
	default:
		writeMCPError(w, request.ID, -32601, "Method not found")
	}
}

func serveMCPToolCall(w http.ResponseWriter, r *http.Request, request mcpRequest, config Config, runOperation operationRunner) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(request.Params) == 0 || json.Unmarshal(request.Params, &params) != nil ||
		(params.Name != "search" && params.Name != "scrape" && !(config.BrowserPipelineEnabled && params.Name == "browser_scrape")) {
		writeMCPError(w, request.ID, -32602, "Invalid params")
		return
	}
	if len(params.Arguments) == 0 {
		params.Arguments = json.RawMessage("{}")
	}
	if params.Arguments[0] != '{' || !json.Valid(params.Arguments) {
		writeMCPError(w, request.ID, -32602, "Invalid params")
		return
	}
	path := "/v2/" + params.Name
	if params.Name == "browser_scrape" {
		path = browserPipelinePath
	}
	entry, err := runOperation(r.Context(), path, "application/json", params.Arguments)
	if err != nil {
		entry = executionErrorEntry(err)
	}
	compact := bytes.Buffer{}
	if compactErr := json.Compact(&compact, entry.Body); compactErr != nil {
		writeMCPResult(w, request.ID, mcpToolResult(string(entry.Body), nil, true))
		return
	}
	succeeded := cacheableSearchResponse(entry.Status, entry.Body)
	if params.Name == "scrape" {
		succeeded = successfulScrapeOutcome(entry.Status, params.Arguments, entry.Body)
	}
	if params.Name == "browser_scrape" {
		succeeded = successfulBrowserPipelineOutcome(entry.Status, entry.Body)
	}
	isError := err != nil || !succeeded
	var structured json.RawMessage
	if !isError {
		structured = json.RawMessage(bytes.Clone(compact.Bytes()))
	}
	writeMCPResult(w, request.ID, mcpToolResult(compact.String(), structured, isError))
}

func mcpToolResult(text string, structured json.RawMessage, isError bool) map[string]any {
	result := map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	}
	if structured != nil {
		result["structuredContent"] = structured
	}
	if isError {
		result["isError"] = true
	}
	return result
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func validMCPID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	if bytes.Equal(id, []byte("null")) {
		return false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	switch value.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}

func normalizedServerVersion(version string) string {
	if version == "" {
		return "dev"
	}
	return version
}

func mcpTools(config Config) []map[string]any {
	schema := func() map[string]any {
		return map[string]any{"type": "object", "additionalProperties": true}
	}
	tools := []map[string]any{
		{"name": "search", "description": "Search the web using the Fireghost local-first retrieval router.", "inputSchema": schema(), "outputSchema": schema()},
		{"name": "scrape", "description": "Scrape a URL using the Fireghost local-first retrieval router.", "inputSchema": schema(), "outputSchema": schema()},
	}
	if config.BrowserPipelineEnabled {
		tools = append(tools, map[string]any{
			"name":        "browser_scrape",
			"description": "Read an article or public Reddit thread through the local browser interaction pipeline. Results may be partial; inspect metadata.complete, stopReason, and warnings.",
			"inputSchema": browserPipelineInputSchema(), "outputSchema": schema(),
		})
	}
	return tools
}

func writeMCPResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeMCPResponse(w, mcpResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeMCPError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeMCPResponse(w, mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: message}})
}

func writeMCPResponse(w http.ResponseWriter, response mcpResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
