package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cachepkg "web-retrieval/internal/cache"
	metricspkg "web-retrieval/internal/metrics"
	"web-retrieval/internal/upstream"
)

const browserPipelinePath = "/v2/browser/scrape"

// The browser pipeline has a dedicated local HTTP contract. Generic cloud
// scrapes cannot satisfy it, and partial browser captures are never cached.
func executeBrowserPipeline(ctx context.Context, config Config, metrics *metricspkg.Registry, local *upstream.Client, contentType string, requestBody []byte) (cachepkg.Entry, error) {
	metrics.IncLocalAttempt(metricspkg.EndpointBrowserScrape)
	timeout := min(normalizedAttemptTimeout(config.HTTPTimeout), 60*time.Second)
	response, err := postWithAttemptTimeout(ctx, timeout, local, browserPipelinePath, contentType, bytes.NewReader(requestBody))
	if err != nil {
		metrics.IncUpstreamFailure(metricspkg.EndpointBrowserScrape, metricspkg.UpstreamLocal)
		return cachepkg.Entry{}, fmt.Errorf("local browser pipeline request failed")
	}
	entry, err := readEntry(response, config.MaxResponseBytes)
	if err != nil {
		metrics.IncUpstreamFailure(metricspkg.EndpointBrowserScrape, metricspkg.UpstreamLocal)
		return cachepkg.Entry{}, fmt.Errorf("local browser pipeline response failed")
	}
	if !successfulBrowserPipelineOutcome(entry.Status, entry.Body) {
		metrics.IncUpstreamFailure(metricspkg.EndpointBrowserScrape, metricspkg.UpstreamLocal)
	}
	return entry, nil
}

func successfulBrowserPipelineOutcome(status int, body []byte) bool {
	if !cacheableResponse(status, body) {
		return false
	}
	var result struct {
		Data struct {
			Markdown string `json:"markdown"`
			Metadata struct {
				Pipeline string `json:"pipeline"`
			} `json:"metadata"`
		} `json:"data"`
	}
	return json.Unmarshal(body, &result) == nil && strings.TrimSpace(result.Data.Markdown) != "" && result.Data.Metadata.Pipeline == "browser-v1"
}

func browserPipelineInputSchema() map[string]any {
	positiveInteger := func(maximum int) map[string]any {
		return map[string]any{"type": "integer", "minimum": 1, "maximum": maximum}
	}
	return map[string]any{
		"type": "object", "required": []string{"url"}, "additionalProperties": false,
		"properties": map[string]any{
			"url":       map[string]any{"type": "string", "format": "uri"},
			"profile":   map[string]any{"type": "string", "enum": []string{"article", "redditThread"}, "default": "article"},
			"timeout":   positiveInteger(60000),
			"maxRounds": positiveInteger(100),
			"maxItems":  positiveInteger(1000),
			"maxBytes":  positiveInteger(262144),
		},
	}
}
