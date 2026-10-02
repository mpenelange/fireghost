package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	budgetpkg "web-retrieval/internal/budget"
	cachepkg "web-retrieval/internal/cache"
	metricspkg "web-retrieval/internal/metrics"
	flightpkg "web-retrieval/internal/singleflight"
	"web-retrieval/internal/upstream"
)

// Config identifies the local upstream and optional Firecrawl Cloud fallback.
type Config struct {
	LocalBaseURL           string
	CloudBaseURL           string
	CloudAPIKey            string
	APIKey                 string
	SearchTTL              time.Duration
	ScrapeTTL              time.Duration
	HTTPTimeout            time.Duration
	SearchEstimatedCredits int
	ScrapeEstimatedCredits int
	MaxRequestBytes        int64
	MaxResponseBytes       int64
	MCPEnabled             bool
	ServerVersion          string
}

// Dependencies contains injectable runtime dependencies.
type Dependencies struct {
	HTTPClient  *http.Client
	Cache       cachepkg.Cache
	Clock       func() time.Time
	FlightGroup flightpkg.FlightGroup[cachepkg.Entry]
	Budget      budgetpkg.Ledger
	Metrics     *metricspkg.Registry
}

// NewHandler returns the router's HTTP handler.
func NewHandler(config Config, dependencies Dependencies) http.Handler {
	if config.SearchEstimatedCredits == 0 {
		config.SearchEstimatedCredits = 2
	}
	if config.ScrapeEstimatedCredits == 0 {
		config.ScrapeEstimatedCredits = 1
	}
	clock := dependencies.Clock
	if clock == nil {
		clock = time.Now
	}
	local := upstream.New(config.LocalBaseURL, dependencies.HTTPClient)
	cloud := upstream.NewAuthenticated(config.CloudBaseURL, dependencies.HTTPClient, config.CloudAPIKey)
	registry := dependencies.Metrics
	if registry == nil {
		registry = metricspkg.NewRegistry()
	}
	runOperation := newOperationRunner(config, dependencies, clock, registry, local, cloud)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, registry.PrometheusText())
	})
	if config.MCPEnabled {
		mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
			serveMCP(w, r, config, runOperation)
		})
	}
	handleOperation := func(path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			requestBody, err := readRequestBody(r.Body, config.MaxRequestBytes)
			if err != nil {
				if errors.Is(err, errBodyTooLarge) {
					http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			entry, err := runOperation(r.Context(), path, r.Header.Get("Content-Type"), requestBody)
			if err != nil {
				writeExecutionError(w, err)
				return
			}
			writeResponse(w, entry)
		}
	}
	mux.HandleFunc("POST /v2/search", handleOperation("/v2/search"))
	mux.HandleFunc("POST /v2/scrape", handleOperation("/v2/scrape"))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint, measured := metricsEndpoint(r.URL.Path)
		start := time.Now()
		writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		if measured && endpoint == metricspkg.EndpointMetrics {
			registry.IncRequest(endpoint, metricspkg.Status2xx)
			registry.ObserveRequestDuration(endpoint, time.Since(start))
		}
		protected := strings.HasPrefix(r.URL.Path, "/v2/") || (config.MCPEnabled && r.URL.Path == "/mcp")
		if config.APIKey != "" && protected && !validBearer(r.Header.Get("Authorization"), config.APIKey) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
		} else {
			mux.ServeHTTP(writer, r)
		}
		if measured && endpoint != metricspkg.EndpointMetrics {
			registry.IncRequest(endpoint, statusClass(writer.status))
			registry.ObserveRequestDuration(endpoint, time.Since(start))
		}
	})
}

type operationRunner func(context.Context, string, string, []byte) (cachepkg.Entry, error)

func newOperationRunner(config Config, dependencies Dependencies, clock func() time.Time, registry *metricspkg.Registry, local, cloud *upstream.Client) operationRunner {
	return func(ctx context.Context, path, contentType string, requestBody []byte) (cachepkg.Entry, error) {
		endpoint := metricspkg.EndpointSearch
		ttl := config.SearchTTL
		cacheableOutcome := func(entry cachepkg.Entry) bool { return cacheableSearchResponse(entry.Status, entry.Body) }
		executeUpstream := func(executionCtx context.Context) (cachepkg.Entry, error) {
			return executeSearch(executionCtx, config, dependencies.Budget, registry, local, cloud, contentType, requestBody)
		}
		if path == "/v2/scrape" {
			endpoint = metricspkg.EndpointScrape
			ttl = config.ScrapeTTL
			cacheableOutcome = func(entry cachepkg.Entry) bool { return successfulScrapeOutcome(entry.Status, requestBody, entry.Body) }
			executeUpstream = func(executionCtx context.Context) (cachepkg.Entry, error) {
				return executeScrape(executionCtx, config, dependencies.Budget, registry, local, cloud, contentType, requestBody)
			}
		}

		cacheKey, cacheableRequest := responseCacheKey(path, requestBody)
		if cacheableRequest && dependencies.Cache != nil && ttl > 0 {
			entry, found, cacheErr := dependencies.Cache.Get(ctx, cacheKey)
			if cacheErr == nil && found && clock().Before(entry.ExpiresAt) {
				registry.IncCacheHit(endpoint)
				return entry, nil
			}
		}
		execute := func(executionCtx context.Context) (cachepkg.Entry, error) {
			entry, executeErr := executeUpstream(executionCtx)
			if executeErr == nil && cacheableRequest && dependencies.Cache != nil && ttl > 0 && cacheableOutcome(entry) {
				entry.ExpiresAt = clock().Add(ttl)
				_ = dependencies.Cache.Set(context.WithoutCancel(executionCtx), cacheKey, cachepkg.Entry{
					Status: entry.Status, ContentType: entry.ContentType, Body: bytes.Clone(entry.Body), Warning: entry.Warning, ExpiresAt: entry.ExpiresAt,
				})
			}
			return entry, executeErr
		}
		if cacheableRequest && dependencies.FlightGroup != nil {
			return dependencies.FlightGroup.Do(ctx, cacheKey, func() (cachepkg.Entry, error) {
				executionCtx, cancel := sharedExecutionContext(config.HTTPTimeout)
				defer cancel()
				return execute(executionCtx)
			})
		}
		return execute(ctx)
	}
}

func sharedExecutionContext(configuredTimeout time.Duration) (context.Context, context.CancelFunc) {
	attemptTimeout := normalizedAttemptTimeout(configuredTimeout)
	totalTimeout := attemptTimeout * 2
	if attemptTimeout > time.Duration(1<<63-1)/2 {
		totalTimeout = time.Duration(1<<63 - 1)
	}
	return context.WithTimeout(context.Background(), totalTimeout)
}

func normalizedAttemptTimeout(configuredTimeout time.Duration) time.Duration {
	if configuredTimeout <= 0 {
		return 60 * time.Second
	}
	return configuredTimeout
}

func postWithAttemptTimeout(ctx context.Context, configuredTimeout time.Duration, client *upstream.Client, path, contentType string, body io.Reader) (*http.Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, normalizedAttemptTimeout(configuredTimeout))
	response, err := client.Post(attemptCtx, path, contentType, body)
	if err != nil {
		cancel()
		return nil, err
	}
	response.Body = &cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body *cancelOnClose) Close() error {
	err := body.ReadCloser.Close()
	body.cancel()
	return err
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func metricsEndpoint(path string) (metricspkg.Endpoint, bool) {
	switch path {
	case "/v2/search":
		return metricspkg.EndpointSearch, true
	case "/v2/scrape":
		return metricspkg.EndpointScrape, true
	case "/health":
		return metricspkg.EndpointHealth, true
	case "/metrics":
		return metricspkg.EndpointMetrics, true
	case "/mcp":
		return metricspkg.EndpointMCP, true
	default:
		return 0, false
	}
}

func statusClass(status int) metricspkg.StatusClass {
	switch status / 100 {
	case 2:
		return metricspkg.Status2xx
	case 3:
		return metricspkg.Status3xx
	case 4:
		return metricspkg.Status4xx
	default:
		return metricspkg.Status5xx
	}
}

var errBodyTooLarge = errors.New("body exceeds configured limit")

var (
	errLocalUpstreamResponseFailed       = errors.New("local upstream response failed")
	errLocalResponseBudgetDenied         = errors.New("local upstream response failed; cloud fallback skipped: credit budget exceeded")
	errLocalResponseBudgetAccounting     = errors.New("local upstream response failed; cloud fallback skipped: budget accounting failure")
	errCloudFallbackBudgetAccounting     = errors.New("cloud fallback skipped: budget accounting failure")
	warningCloudBudgetExceeded           = "cloud fallback skipped: credit budget exceeded"
	warningCloudBudgetAccountingDisabled = "cloud fallback skipped because budget accounting was unavailable"
)

func readRequestBody(body io.Reader, maximum int64) ([]byte, error) {
	if maximum <= 0 {
		return io.ReadAll(body)
	}
	contents, err := io.ReadAll(io.LimitReader(body, maximum+1))
	if err == nil && int64(len(contents)) > maximum {
		return nil, errBodyTooLarge
	}
	return contents, err
}

func validBearer(authorization, expected string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		return false
	}
	provided := strings.TrimPrefix(authorization, prefix)
	return len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func writeExecutionError(w http.ResponseWriter, err error) {
	writeResponse(w, executionErrorEntry(err))
}

func executionErrorEntry(err error) cachepkg.Entry {
	if errors.Is(err, budgetpkg.ErrLimitExceeded) {
		return cachepkg.Entry{Status: http.StatusServiceUnavailable, ContentType: "application/json", Body: []byte("{\"error\":\"cloud fallback skipped: credit budget exceeded\"}\n")}
	}
	if err.Error() == "local and cloud upstream requests failed" ||
		errors.Is(err, errLocalUpstreamResponseFailed) || errors.Is(err, errLocalResponseBudgetDenied) ||
		errors.Is(err, errLocalResponseBudgetAccounting) || errors.Is(err, errCloudFallbackBudgetAccounting) {
		body, _ := json.Marshal(map[string]string{"error": err.Error()})
		return cachepkg.Entry{Status: http.StatusBadGateway, ContentType: "application/json", Body: append(body, '\n')}
	}
	return cachepkg.Entry{Status: http.StatusBadGateway, ContentType: "text/plain; charset=utf-8", Body: []byte(err.Error() + "\n")}
}

func executeScrape(ctx context.Context, config Config, ledger budgetpkg.Ledger, metrics *metricspkg.Registry, local, cloud *upstream.Client, contentType string, requestBody []byte) (cachepkg.Entry, error) {
	metrics.IncLocalAttempt(metricspkg.EndpointScrape)
	response, err := postWithAttemptTimeout(ctx, config.HTTPTimeout, local, "/v2/scrape", contentType, bytes.NewReader(requestBody))
	if err != nil {
		metrics.IncUpstreamFailure(metricspkg.EndpointScrape, metricspkg.UpstreamLocal)
		if config.CloudBaseURL != "" && config.CloudAPIKey != "" {
			if reserveErr := reserveCloud(ctx, ledger, config.ScrapeEstimatedCredits); reserveErr != nil {
				return cachepkg.Entry{}, reservationErrorWithoutLocal(metrics, metricspkg.EndpointScrape, reserveErr)
			}
			metrics.IncCloudAttempt(metricspkg.EndpointScrape)
			cloudResponse, cloudErr := postWithAttemptTimeout(ctx, config.HTTPTimeout, cloud, "/v2/scrape", contentType, bytes.NewReader(requestBody))
			if cloudErr == nil {
				cloudEntry, cloudReadErr := readEntry(cloudResponse, config.MaxResponseBytes)
				if cloudReadErr == nil && successfulScrapeOutcome(cloudEntry.Status, requestBody, cloudEntry.Body) {
					return cloudEntry, nil
				}
			}
			metrics.IncUpstreamFailure(metricspkg.EndpointScrape, metricspkg.UpstreamCloud)
			return cachepkg.Entry{}, fmt.Errorf("local and cloud upstream requests failed")
		}
		return cachepkg.Entry{}, fmt.Errorf("local upstream request failed")
	}
	localEntry, readErr := readEntry(response, config.MaxResponseBytes)
	if readErr != nil {
		metrics.IncUpstreamFailure(metricspkg.EndpointScrape, metricspkg.UpstreamLocal)
		var reserveErr error
		if config.CloudBaseURL != "" && config.CloudAPIKey != "" {
			reserveErr = reserveCloud(ctx, ledger, config.ScrapeEstimatedCredits)
			if reserveErr == nil {
				metrics.IncCloudAttempt(metricspkg.EndpointScrape)
				cloudResponse, cloudErr := postWithAttemptTimeout(ctx, config.HTTPTimeout, cloud, "/v2/scrape", contentType, bytes.NewReader(requestBody))
				if cloudErr == nil {
					cloudEntry, cloudReadErr := readEntry(cloudResponse, config.MaxResponseBytes)
					if cloudReadErr == nil && successfulScrapeOutcome(cloudEntry.Status, requestBody, cloudEntry.Body) {
						return cloudEntry, nil
					}
				}
				metrics.IncUpstreamFailure(metricspkg.EndpointScrape, metricspkg.UpstreamCloud)
				return cachepkg.Entry{}, fmt.Errorf("local and cloud upstream requests failed")
			}
			if errors.Is(reserveErr, budgetpkg.ErrLimitExceeded) {
				metrics.IncCloudBudgetDenied(metricspkg.EndpointScrape)
			}
		}
		return cachepkg.Entry{}, localResponseReadFailure(reserveErr)
	}
	if config.CloudBaseURL != "" && config.CloudAPIKey != "" && shouldFallbackScrape(localEntry.Status, requestBody, localEntry.Body) {
		metrics.IncUpstreamFailure(metricspkg.EndpointScrape, metricspkg.UpstreamLocal)
		if reserveErr := reserveCloud(ctx, ledger, config.ScrapeEstimatedCredits); reserveErr != nil {
			return withReservationWarning(metrics, metricspkg.EndpointScrape, localEntry, reserveErr), nil
		}
		metrics.IncCloudAttempt(metricspkg.EndpointScrape)
		cloudResponse, cloudErr := postWithAttemptTimeout(ctx, config.HTTPTimeout, cloud, "/v2/scrape", contentType, bytes.NewReader(requestBody))
		if cloudErr == nil {
			cloudEntry, cloudReadErr := readEntry(cloudResponse, config.MaxResponseBytes)
			if cloudReadErr == nil && successfulScrapeOutcome(cloudEntry.Status, requestBody, cloudEntry.Body) {
				return cloudEntry, nil
			}
		}
		metrics.IncUpstreamFailure(metricspkg.EndpointScrape, metricspkg.UpstreamCloud)
	}
	return localEntry, nil
}

func executeSearch(ctx context.Context, config Config, ledger budgetpkg.Ledger, metrics *metricspkg.Registry, local, cloud *upstream.Client, contentType string, requestBody []byte) (cachepkg.Entry, error) {
	metrics.IncLocalAttempt(metricspkg.EndpointSearch)
	response, err := postWithAttemptTimeout(ctx, config.HTTPTimeout, local, "/v2/search", contentType, bytes.NewReader(requestBody))
	if err != nil {
		metrics.IncUpstreamFailure(metricspkg.EndpointSearch, metricspkg.UpstreamLocal)
		if config.CloudBaseURL != "" && config.CloudAPIKey != "" {
			if reserveErr := reserveCloud(ctx, ledger, config.SearchEstimatedCredits); reserveErr != nil {
				return cachepkg.Entry{}, reservationErrorWithoutLocal(metrics, metricspkg.EndpointSearch, reserveErr)
			}
			metrics.IncCloudAttempt(metricspkg.EndpointSearch)
			cloudResponse, cloudErr := postWithAttemptTimeout(ctx, config.HTTPTimeout, cloud, "/v2/search", contentType, bytes.NewReader(requestBody))
			if cloudErr == nil {
				cloudEntry, cloudReadErr := readEntry(cloudResponse, config.MaxResponseBytes)
				if cloudReadErr == nil && cloudSearchSucceeded(cloudEntry.Status, cloudEntry.Body) {
					return cloudEntry, nil
				}
			}
			metrics.IncUpstreamFailure(metricspkg.EndpointSearch, metricspkg.UpstreamCloud)
			return cachepkg.Entry{}, fmt.Errorf("local and cloud upstream requests failed")
		}
		return cachepkg.Entry{}, fmt.Errorf("local upstream request failed")
	}
	localEntry, readErr := readEntry(response, config.MaxResponseBytes)
	if readErr != nil {
		metrics.IncUpstreamFailure(metricspkg.EndpointSearch, metricspkg.UpstreamLocal)
		var reserveErr error
		if config.CloudBaseURL != "" && config.CloudAPIKey != "" {
			reserveErr = reserveCloud(ctx, ledger, config.SearchEstimatedCredits)
			if reserveErr == nil {
				metrics.IncCloudAttempt(metricspkg.EndpointSearch)
				cloudResponse, cloudErr := postWithAttemptTimeout(ctx, config.HTTPTimeout, cloud, "/v2/search", contentType, bytes.NewReader(requestBody))
				if cloudErr == nil {
					cloudEntry, cloudReadErr := readEntry(cloudResponse, config.MaxResponseBytes)
					if cloudReadErr == nil && cloudSearchSucceeded(cloudEntry.Status, cloudEntry.Body) {
						return cloudEntry, nil
					}
				}
				metrics.IncUpstreamFailure(metricspkg.EndpointSearch, metricspkg.UpstreamCloud)
				return cachepkg.Entry{}, fmt.Errorf("local and cloud upstream requests failed")
			}
			if errors.Is(reserveErr, budgetpkg.ErrLimitExceeded) {
				metrics.IncCloudBudgetDenied(metricspkg.EndpointSearch)
			}
		}
		return cachepkg.Entry{}, localResponseReadFailure(reserveErr)
	}
	if config.CloudBaseURL != "" && config.CloudAPIKey != "" && shouldFallbackSearch(localEntry.Status, localEntry.Body) {
		metrics.IncUpstreamFailure(metricspkg.EndpointSearch, metricspkg.UpstreamLocal)
		if reserveErr := reserveCloud(ctx, ledger, config.SearchEstimatedCredits); reserveErr != nil {
			return withReservationWarning(metrics, metricspkg.EndpointSearch, localEntry, reserveErr), nil
		}
		metrics.IncCloudAttempt(metricspkg.EndpointSearch)
		cloudResponse, cloudErr := postWithAttemptTimeout(ctx, config.HTTPTimeout, cloud, "/v2/search", contentType, bytes.NewReader(requestBody))
		if cloudErr == nil {
			cloudEntry, cloudReadErr := readEntry(cloudResponse, config.MaxResponseBytes)
			if cloudReadErr == nil && cloudSearchSucceeded(cloudEntry.Status, cloudEntry.Body) {
				return cloudEntry, nil
			}
		}
		metrics.IncUpstreamFailure(metricspkg.EndpointSearch, metricspkg.UpstreamCloud)
	}
	return localEntry, nil
}

func reserveCloud(ctx context.Context, ledger budgetpkg.Ledger, credits int) error {
	if ledger == nil {
		return nil
	}
	return ledger.Reserve(ctx, credits)
}

func reservationErrorWithoutLocal(metrics *metricspkg.Registry, endpoint metricspkg.Endpoint, err error) error {
	if errors.Is(err, budgetpkg.ErrLimitExceeded) {
		metrics.IncCloudBudgetDenied(endpoint)
		return err
	}
	return fmt.Errorf("%w: %v", errCloudFallbackBudgetAccounting, err)
}

func localResponseReadFailure(reserveErr error) error {
	if errors.Is(reserveErr, budgetpkg.ErrLimitExceeded) {
		return errLocalResponseBudgetDenied
	}
	if reserveErr != nil {
		return errLocalResponseBudgetAccounting
	}
	return errLocalUpstreamResponseFailed
}

func withReservationWarning(metrics *metricspkg.Registry, endpoint metricspkg.Endpoint, entry cachepkg.Entry, err error) cachepkg.Entry {
	warning := warningCloudBudgetAccountingDisabled
	if errors.Is(err, budgetpkg.ErrLimitExceeded) {
		metrics.IncCloudBudgetDenied(endpoint)
		warning = warningCloudBudgetExceeded
	}
	return withWarning(entry, warning)
}

func withWarning(entry cachepkg.Entry, warning string) cachepkg.Entry {
	var object map[string]json.RawMessage
	if json.Unmarshal(entry.Body, &object) != nil || object == nil {
		entry.Warning = warning
		return entry
	}
	message := warning
	var existing string
	if json.Unmarshal(object["warning"], &existing) == nil && existing != "" {
		message = existing + "; " + warning
	}
	object["warning"], _ = json.Marshal(message)
	if body, err := json.Marshal(object); err == nil {
		entry.Body = body
	}
	return entry
}

func readEntry(response *http.Response, maximum int64) (cachepkg.Entry, error) {
	defer response.Body.Close()
	body, err := readRequestBody(response.Body, maximum)
	return cachepkg.Entry{Status: response.StatusCode, ContentType: response.Header.Get("Content-Type"), Body: body}, err
}

func responseCacheKey(path string, body []byte) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "", false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return "", false
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(append([]byte(path+"\n"), canonical...))
	return fmt.Sprintf("%x", sum), true
}

func cacheableResponse(status int, body []byte) bool {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return false
	}
	var result struct {
		Success *bool `json:"success"`
	}
	return json.Unmarshal(body, &result) == nil && result.Success != nil && *result.Success
}

func cacheableSearchResponse(status int, body []byte) bool {
	if !cacheableResponse(status, body) {
		return false
	}
	var result struct {
		Data *struct {
			Web *[]json.RawMessage `json:"web"`
		} `json:"data"`
	}
	return json.Unmarshal(body, &result) == nil && result.Data != nil && result.Data.Web != nil && len(*result.Data.Web) > 0
}

func cloudScrapeSucceeded(status int, body []byte) bool {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return false
	}
	var result struct {
		Success *bool `json:"success"`
	}
	return json.Unmarshal(body, &result) == nil && result.Success != nil && *result.Success
}

func successfulScrapeOutcome(status int, requestBody, responseBody []byte) bool {
	if !cloudScrapeSucceeded(status, responseBody) {
		return false
	}
	if !markdownRequested(requestBody) {
		return true
	}
	var result struct {
		Data *struct {
			Markdown *string `json:"markdown"`
		} `json:"data"`
	}
	return json.Unmarshal(responseBody, &result) == nil && result.Data != nil && result.Data.Markdown != nil && strings.TrimSpace(*result.Data.Markdown) != ""
}

func cloudSearchSucceeded(status int, body []byte) bool {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return false
	}
	var result struct {
		Success *bool `json:"success"`
		Data    *struct {
			Web *[]json.RawMessage `json:"web"`
		} `json:"data"`
	}
	return json.Unmarshal(body, &result) == nil &&
		(result.Success == nil || *result.Success) &&
		result.Data != nil && result.Data.Web != nil && len(*result.Data.Web) > 0
}

func shouldFallbackScrape(status int, requestBody, responseBody []byte) bool {
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		var result struct {
			Success *bool  `json:"success"`
			Warning string `json:"warning"`
			Error   string `json:"error"`
			Data    *struct {
				Markdown *string `json:"markdown"`
			} `json:"data"`
		}
		if json.Unmarshal(responseBody, &result) != nil {
			return false
		}
		if result.Success != nil && !*result.Success {
			return hasRetryableScrapeIndicator(result.Warning + " " + result.Error)
		}
		return markdownRequested(requestBody) && (result.Data == nil || result.Data.Markdown == nil || strings.TrimSpace(*result.Data.Markdown) == "")
	}
	switch status {
	case http.StatusUnauthorized, http.StatusNotFound, http.StatusGone:
		return false
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if hasDeterministicScrapeIndicator(string(responseBody)) {
			return false
		}
		return hasRetryableScrapeIndicator(string(responseBody))
	}
	message := strings.ToLower(string(responseBody))
	return !strings.Contains(message, "robots") && !strings.Contains(message, "invalid url")
}

func hasDeterministicScrapeIndicator(message string) bool {
	message = strings.ToLower(message)
	for _, indicator := range []string{"malformed", "unsupported", "invalid url", "robots"} {
		if strings.Contains(message, indicator) {
			return true
		}
	}
	return false
}

func hasRetryableScrapeIndicator(message string) bool {
	message = strings.ToLower(message)
	for _, indicator := range []string{
		"anti-bot", "antibot", "blocked", "cloudflare", "captcha", "challenge", "403", "forbidden",
		"rate limit", "rate-limit", "rate_limit", "timeout", "timed out",
		"renderer exhausted", "renderer-exhausted", "renderer_exhausted",
		"invalid extraction", "invalid-extraction", "invalid_extraction",
		"connection reset", "connection-reset", "connection_reset",
	} {
		if strings.Contains(message, indicator) {
			return true
		}
	}
	return false
}

func markdownRequested(body []byte) bool {
	var request struct {
		Formats json.RawMessage `json:"formats"`
	}
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	if request.Formats == nil {
		return true
	}
	var formats []json.RawMessage
	if json.Unmarshal(request.Formats, &formats) != nil {
		return false
	}
	if len(formats) == 0 {
		return true
	}
	for _, format := range formats {
		var name string
		if json.Unmarshal(format, &name) == nil && name == "markdown" {
			return true
		}
		var object struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(format, &object) == nil && object.Type == "markdown" {
			return true
		}
	}
	return false
}

func shouldFallbackSearch(status int, body []byte) bool {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return true
	}
	var result struct {
		Success *bool `json:"success"`
		Data    *struct {
			Web *[]json.RawMessage `json:"web"`
		} `json:"data"`
		Warning string `json:"warning"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &result) != nil {
		return false
	}
	if result.Success != nil && !*result.Success {
		return true
	}
	if result.Data == nil || result.Data.Web == nil {
		return result.Success != nil && *result.Success
	}
	return len(*result.Data.Web) == 0
}

func writeResponse(w http.ResponseWriter, entry cachepkg.Entry) {
	w.Header().Set("Content-Type", entry.ContentType)
	if entry.Warning != "" {
		w.Header().Set("X-Web-Retrieval-Warning", entry.Warning)
	}
	w.WriteHeader(entry.Status)
	_, _ = w.Write(entry.Body)
}
