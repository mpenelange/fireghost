package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"web-retrieval/internal/config"
	metricspkg "web-retrieval/internal/metrics"
)

func TestBrowserPipelineRuntimeWiresOptIn(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/browser/scrape" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"success":true,"data":{"markdown":"ok","metadata":{"pipeline":"browser-v1"}}}`)
	}))
	defer local.Close()
	root := t.TempDir()
	values := map[string]string{"ROUTER_LOCAL_URL": local.URL, "ROUTER_BROWSER_PIPELINE_ENABLED": "true", "ROUTER_CACHE_DIR": filepath.Join(root, "cache"), "ROUTER_LEDGER_PATH": filepath.Join(root, "budget.json")}
	cfg, err := config.Parse(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BrowserPipelineEnabled {
		t.Fatal("runtime did not parse BrowserPipelineEnabled")
	}
	handler, err := buildHandler(cfg, metricspkg.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v2/browser/scrape", strings.NewReader(`{"url":"https://example.test"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "browser-v1") {
		t.Fatalf("runtime browser pipeline=%d %s", w.Code, w.Body.String())
	}
}
