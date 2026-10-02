package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	budgetpkg "web-retrieval/internal/budget"
	creditspkg "web-retrieval/internal/credits"
	metricspkg "web-retrieval/internal/metrics"
	"web-retrieval/internal/upstream"
)

// localRoutes are Firecrawl v2 routes CRW implements. They always go to CRW and
// never fall back to Firecrawl Cloud. /v2/search and /v2/scrape are handled
// separately with cache and fallback.
var localRoutes = []string{
	"POST /v2/map",
	"POST /v2/crawl",
	"GET /v2/crawl/active",
	"GET /v2/crawl/{id}",
	"DELETE /v2/crawl/{id}",
	"GET /v2/crawl/{id}/errors",
	"POST /v2/batch/scrape",
	"GET /v2/batch/scrape/{id}",
	"DELETE /v2/batch/scrape/{id}",
	"GET /v2/batch/scrape/{id}/errors",
	"POST /v2/extract",
	"GET /v2/extract/{id}",
	"POST /v2/parse",
	"GET /v2/scrape/{jobId}",
}

// cloudRoutes are Firecrawl v2 routes CRW does not implement, forwarded to
// Firecrawl Cloud with the configured key. Anything absent from both lists is
// a 404. Monitors are deliberately excluded: they schedule billing that the
// credit floor cannot observe. Account settings, feedback, and support routes
// are excluded as well.
var cloudRoutes = []string{
	"GET /v2/agent",
	"POST /v2/agent",
	"GET /v2/agent/{jobId}",
	"DELETE /v2/agent/{jobId}",
	"GET /v2/agent/{jobId}/trace",
	"GET /v2/agent/{jobId}/snapshots/{snapshotId}",
	"GET /v2/interact",
	"POST /v2/interact",
	"POST /v2/interact/{sessionId}/execute",
	"DELETE /v2/interact/{sessionId}",
	"POST /v2/scrape/{jobId}/interact",
	"DELETE /v2/scrape/{jobId}/interact",
	"POST /v2/crawl/params-preview",
	"GET /v2/search/research/papers",
	"GET /v2/search/research/papers/{id}",
	"GET /v2/search/research/papers/{id}/similar",
	"GET /v2/search/developer",
	"POST /v2/search/developer",
	"GET /v2/team/credit-usage",
	"GET /v2/team/credit-usage/historical",
	"GET /v2/team/token-usage",
	"GET /v2/team/token-usage/historical",
	"GET /v2/team/queue-status",
	"GET /v2/team/activity",
}

var (
	routeWildcard   = regexp.MustCompile(`\{(\w+)\}`)
	safePathSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// passthroughEndpoint reports the metrics endpoint for a registered
// pass-through pattern.
func passthroughEndpoint(pattern string) (metricspkg.Endpoint, bool) {
	for _, route := range localRoutes {
		if route == pattern {
			return metricspkg.EndpointLocalPassthrough, true
		}
	}
	for _, route := range cloudRoutes {
		if route == pattern {
			return metricspkg.EndpointCloudPassthrough, true
		}
	}
	return 0, false
}

func registerPassthrough(mux *http.ServeMux, config Config, registry *metricspkg.Registry, local, cloud *upstream.Client, floor *creditspkg.Floor) {
	for _, pattern := range localRoutes {
		limit := config.MaxRequestBytes
		if pattern == "POST /v2/parse" {
			limit = config.MaxParseBytes
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !safeWildcards(pattern, r) {
				writeJSONError(w, http.StatusBadRequest, "invalid path parameter")
				return
			}
			if declaredTooLarge(w, r, limit) {
				return
			}
			registry.IncLocalAttempt(metricspkg.EndpointLocalPassthrough)
			if !forward(w, r, config, local, limit, true) {
				registry.IncUpstreamFailure(metricspkg.EndpointLocalPassthrough, metricspkg.UpstreamLocal)
			}
		})
	}
	for _, pattern := range cloudRoutes {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !safeWildcards(pattern, r) {
				writeJSONError(w, http.StatusBadRequest, "invalid path parameter")
				return
			}
			if declaredTooLarge(w, r, config.MaxRequestBytes) {
				return
			}
			if config.CloudBaseURL == "" || config.CloudAPIKey == "" {
				writeJSONError(w, http.StatusServiceUnavailable, "cloud upstream not configured")
				return
			}
			if billable(r.Method) && floor != nil {
				if err := floor.Check(r.Context()); err != nil {
					if errors.Is(err, budgetpkg.ErrLimitExceeded) {
						registry.IncCloudBudgetDenied(metricspkg.EndpointCloudPassthrough)
						writeJSONError(w, http.StatusServiceUnavailable, "cloud request skipped: credit balance at or below floor")
						return
					}
					writeJSONError(w, http.StatusServiceUnavailable, "cloud request skipped: credit balance unavailable")
					return
				}
			}
			registry.IncCloudAttempt(metricspkg.EndpointCloudPassthrough)
			if !forward(w, r, config, cloud, config.MaxRequestBytes, false) {
				registry.IncUpstreamFailure(metricspkg.EndpointCloudPassthrough, metricspkg.UpstreamCloud)
			}
		})
	}
}

// billable reports whether a cloud request can create billed work. Reads and
// cancellations always pass so paid-for results stay reachable.
func billable(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodDelete
}

// safeWildcards rejects path parameters that could re-route the request once
// the upstream decodes them, such as an encoded slash or dot segment.
func safeWildcards(pattern string, r *http.Request) bool {
	for _, match := range routeWildcard.FindAllStringSubmatch(pattern, -1) {
		if !safePathSegment.MatchString(r.PathValue(match[1])) {
			return false
		}
	}
	return true
}

// forward streams the request to the upstream and the response back. It
// reports whether the upstream produced a response. With preserveHost, the
// client's Host and scheme reach the upstream so CRW's job and pagination URLs
// name this router rather than CRW's private address.
func forward(w http.ResponseWriter, r *http.Request, config Config, client *upstream.Client, limit int64, preserveHost bool) bool {
	request := upstream.Request{Method: r.Method, ContentType: r.Header.Get("Content-Type"), ContentLength: r.ContentLength}
	if r.ContentLength != 0 {
		request.Body = r.Body
		if limit > 0 {
			request.Body = http.MaxBytesReader(w, r.Body, limit)
		}
	}
	if preserveHost {
		request.Host = r.Host
		request.ForwardedProto = forwardedProto(r)
	}
	ctx, cancel := context.WithTimeout(r.Context(), normalizedAttemptTimeout(config.HTTPTimeout))
	defer cancel()
	request.PathAndQuery = r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		request.PathAndQuery += "?" + r.URL.RawQuery
	}
	response, err := client.Send(ctx, request)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return true
		}
		writeJSONError(w, http.StatusBadGateway, "upstream request failed")
		return false
	}
	defer response.Body.Close()
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
	return true
}

// declaredTooLarge rejects a body whose declared length exceeds limit before
// any upstream work or accounting. Undeclared lengths are bounded while
// streaming instead.
func declaredTooLarge(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if limit > 0 && r.ContentLength > limit {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return true
	}
	return false
}

// forwardedProto is the scheme the client used: the proxy's X-Forwarded-Proto
// when present (Traefik terminates TLS), otherwise this connection's.
func forwardedProto(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "http" || proto == "https" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	body, _ := json.Marshal(map[string]any{"success": false, "error": message})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
