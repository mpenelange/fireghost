package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"web-retrieval/internal/config"
	metricspkg "web-retrieval/internal/metrics"
)

func TestBuildHandlerCreatesPersistentRuntimeStorage(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		LocalURL: "http://local:3000", CloudURL: "https://cloud.example",
		CacheDir: filepath.Join(root, "cache"), LedgerPath: filepath.Join(root, "ledger", "budget.json"),
		SearchTTL: time.Minute, ScrapeTTL: time.Hour, HTTPTimeout: time.Second,
		MaxRequestBytes: 100, MaxResponseBytes: 200, CacheMaxEntryBytes: 150, CacheMaxBytes: 1024,
		MaxInflight:       64,
		DailyCloudCredits: 1, SearchEstimatedCredits: 2, ScrapeEstimatedCredits: 1,
	}
	if _, err := buildHandler(cfg, metricspkg.NewRegistry()); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{cfg.CacheDir, filepath.Dir(cfg.LedgerPath)} {
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			t.Fatalf("storage directory %q: info=%v err=%v", directory, info, err)
		}
	}
}

func TestServeShutsDownWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.NewServeMux()}
	if err := serve(ctx, server); err != nil {
		t.Fatalf("serve after cancellation: %v", err)
	}
}

func TestNewServerAppliesInboundAndLifecycleTimeouts(t *testing.T) {
	handler := http.NewServeMux()
	cfg := config.Config{
		ListenAddr: "127.0.0.1:9090", HTTPTimeout: 45 * time.Second, ServerReadTimeout: 17 * time.Second,
	}
	server := newServer(cfg, handler)

	if server.Addr != cfg.ListenAddr || server.Handler != handler {
		t.Fatalf("server wiring = %#v", server)
	}
	if server.ReadTimeout != cfg.ServerReadTimeout {
		t.Fatalf("read timeout = %v, want %v", server.ReadTimeout, cfg.ServerReadTimeout)
	}
	if server.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("read header timeout = %v, want 10s", server.ReadHeaderTimeout)
	}
	if server.IdleTimeout != 120*time.Second {
		t.Fatalf("idle timeout = %v, want 2m", server.IdleTimeout)
	}
	if server.WriteTimeout < cfg.HTTPTimeout {
		t.Fatalf("write timeout = %v, shorter than upstream timeout %v", server.WriteTimeout, cfg.HTTPTimeout)
	}
}

func TestNewServerWriteTimeoutCoversBothUpstreamAttemptsWithoutOverflow(t *testing.T) {
	handler := http.NewServeMux()
	cfg := config.Config{
		ListenAddr:        "127.0.0.1:9090",
		HTTPTimeout:       90 * time.Second,
		ServerReadTimeout: 23 * time.Second,
	}
	server := newServer(cfg, handler)

	if server.WriteTimeout != 190*time.Second {
		t.Fatalf("write timeout = %v, want two 90s attempts plus 10s response overhead", server.WriteTimeout)
	}
	if server.ReadTimeout != 23*time.Second {
		t.Fatalf("read timeout = %v, want independently configured 23s", server.ReadTimeout)
	}

	cfg.HTTPTimeout = time.Duration(1<<63 - 1)
	server = newServer(cfg, handler)
	if server.WriteTimeout != time.Duration(1<<63-1) {
		t.Fatalf("overflow-safe write timeout = %v, want maximum duration", server.WriteTimeout)
	}
}

func TestBuildHandlerWiresTotalCacheByteLimit(t *testing.T) {
	localCalls := 0
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		localCalls++
		_, _ = w.Write([]byte(`{"success":true,"data":{"web":[{"url":"https://example.test"}]}}`))
	}))
	defer local.Close()
	root := t.TempDir()
	cfg := config.Config{
		LocalURL: local.URL, CloudURL: "https://cloud.example",
		CacheDir: filepath.Join(root, "cache"), LedgerPath: filepath.Join(root, "budget.json"),
		SearchTTL: time.Minute, ScrapeTTL: time.Hour, HTTPTimeout: time.Second,
		MaxRequestBytes: 1024, MaxResponseBytes: 1024, CacheMaxEntryBytes: 1024, CacheMaxBytes: 1,
		MaxInflight:            64,
		SearchEstimatedCredits: 2, ScrapeEstimatedCredits: 1,
	}
	handler, err := buildHandler(cfg, metricspkg.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewBufferString(`{"query":"cache"}`)))
	}
	if localCalls != 2 {
		t.Fatalf("local calls = %d, want 2 when encoded entry exceeds configured total", localCalls)
	}
}

func TestBuildHandlerWiresMaximumInflightLimit(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var running atomic.Int32
	var maximum atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := running.Add(1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		started <- struct{}{}
		<-release
		running.Add(-1)
		_, _ = w.Write([]byte(`{"success":true,"data":{"web":[{"url":"https://example.test"}]}}`))
	}))
	defer local.Close()
	root := t.TempDir()
	cfg := config.Config{
		LocalURL: local.URL, CloudURL: "https://cloud.example",
		CacheDir: filepath.Join(root, "cache"), LedgerPath: filepath.Join(root, "budget.json"),
		SearchTTL: time.Minute, ScrapeTTL: time.Hour, HTTPTimeout: time.Second,
		MaxRequestBytes: 1024, MaxResponseBytes: 1024, CacheMaxEntryBytes: 1024, CacheMaxBytes: 4096, MaxInflight: 1,
		SearchEstimatedCredits: 2, ScrapeEstimatedCredits: 1,
	}
	handler, err := buildHandler(cfg, metricspkg.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}

	var requests sync.WaitGroup
	requests.Add(2)
	for _, query := range []string{"one", "two"} {
		go func() {
			defer requests.Done()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v2/search", bytes.NewBufferString(`{"query":"`+query+`"}`)))
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach local upstream")
	}
	select {
	case <-started:
		t.Fatal("second unique request reached upstream above configured limit")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	requests.Wait()
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent upstream calls = %d, want 1", got)
	}
}
