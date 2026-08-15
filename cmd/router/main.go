package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	budgetpkg "web-retrieval/internal/budget"
	cachepkg "web-retrieval/internal/cache"
	"web-retrieval/internal/config"
	metricspkg "web-retrieval/internal/metrics"
	"web-retrieval/internal/router"
	flightpkg "web-retrieval/internal/singleflight"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Parse(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	handler, err := buildHandler(cfg, metricspkg.NewRegistry())
	if err != nil {
		return err
	}
	server := newServer(cfg, handler)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, server)
}

func newServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadTimeout:       cfg.ServerReadTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      serverWriteTimeout(cfg.HTTPTimeout),
		IdleTimeout:       2 * time.Minute,
	}
}

func serverWriteTimeout(httpTimeout time.Duration) time.Duration {
	const (
		responseOverhead = 10 * time.Second
		maxDuration      = time.Duration(1<<63 - 1)
	)
	if httpTimeout > (maxDuration-responseOverhead)/2 {
		return maxDuration
	}
	return 2*httpTimeout + responseOverhead
}

func serve(ctx context.Context, server *http.Server) error {
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve router: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		if err := <-serverErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve router: %w", err)
		}
		return nil
	}
}

func buildHandler(cfg config.Config, registry *metricspkg.Registry) (http.Handler, error) {
	fileCache, err := cachepkg.NewFile(cfg.CacheDir, time.Now, cfg.CacheMaxEntryBytes, cfg.CacheMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("initialize cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LedgerPath), 0o700); err != nil {
		return nil, fmt.Errorf("initialize budget directory: %w", err)
	}
	ledger, err := budgetpkg.NewFile(cfg.LedgerPath, cfg.DailyCloudCredits, cfg.MonthlyCloudCredits, time.Now)
	if err != nil {
		return nil, fmt.Errorf("initialize budget: %w", err)
	}
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	return router.NewHandler(router.Config{
		LocalBaseURL: cfg.LocalURL, CloudBaseURL: cfg.CloudURL, CloudAPIKey: cfg.CloudAPIKey, APIKey: cfg.APIKey,
		SearchTTL: cfg.SearchTTL, ScrapeTTL: cfg.ScrapeTTL, HTTPTimeout: cfg.HTTPTimeout, SearchEstimatedCredits: cfg.SearchEstimatedCredits,
		ScrapeEstimatedCredits: cfg.ScrapeEstimatedCredits, MaxRequestBytes: cfg.MaxRequestBytes, MaxResponseBytes: cfg.MaxResponseBytes,
	}, router.Dependencies{HTTPClient: client, Cache: fileCache, Budget: ledger, Metrics: registry, FlightGroup: flightpkg.NewWithLimit[cachepkg.Entry](cfg.MaxInflight)}), nil
}
