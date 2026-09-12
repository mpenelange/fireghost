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

func serveMCP(w http.ResponseWriter, r *http.Request, config Config) {
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
			"capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]string{"name": "fireghost", "version": normalizedServerVersion(config.ServerVersion)},
		})
	case "ping":
		writeMCPResult(w, request.ID, map[string]any{})
	case "tools/list":
		writeMCPResult(w, request.ID, map[string]any{"tools": mcpTools()})
	default:
		writeMCPError(w, request.ID, -32601, "Method not found")
	}
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

func mcpTools() []map[string]any {
	schema := func() map[string]any {
		return map[string]any{"type": "object", "additionalProperties": true}
	}
	return []map[string]any{
		{"name": "search", "description": "Search the web using the Fireghost local-first retrieval router.", "inputSchema": schema(), "outputSchema": schema()},
		{"name": "scrape", "description": "Scrape a URL using the Fireghost local-first retrieval router.", "inputSchema": schema(), "outputSchema": schema()},
	}
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
