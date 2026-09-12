// Package config parses and validates router runtime configuration.
package config

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Config contains validated runtime settings.
type Config struct {
	ListenAddr, LocalURL, CloudURL, CloudAPIKey, APIKey  string
	CacheDir, LedgerPath                                 string
	SearchTTL, ScrapeTTL, HTTPTimeout, ServerReadTimeout time.Duration
	MaxRequestBytes, MaxResponseBytes                    int64
	CacheMaxBytes                                        int64
	CacheMaxEntryBytes                                   int
	MaxInflight                                          int
	DailyCloudCredits, MonthlyCloudCredits               int
	CloudBurstCredits, CloudRefillCreditsPerDay          int
	MonthlyResetDay                                      int
	SearchEstimatedCredits, ScrapeEstimatedCredits       int
	MCPEnabled                                           bool
}

// Parse reads configuration through getenv, making parsing deterministic in tests.
func Parse(getenv func(string) string) (Config, error) {
	c := Config{
		ListenAddr: ":8080", LocalURL: getenv("ROUTER_LOCAL_URL"), CloudURL: "https://api.firecrawl.dev",
		CacheDir: "/data/cache", LedgerPath: "/data/budget.json",
		SearchTTL: 15 * time.Minute, ScrapeTTL: 24 * time.Hour, HTTPTimeout: 60 * time.Second, ServerReadTimeout: 30 * time.Second,
		MaxRequestBytes: 2 << 20, MaxResponseBytes: 16 << 20, CacheMaxBytes: 1 << 30, CacheMaxEntryBytes: 16 << 20,
		MaxInflight:            64,
		MonthlyResetDay:        1,
		SearchEstimatedCredits: 2, ScrapeEstimatedCredits: 1,
	}
	if raw := getenv("MCP_ENABLED"); raw != "" {
		switch raw {
		case "true":
			c.MCPEnabled = true
		case "false":
			c.MCPEnabled = false
		default:
			return Config{}, fmt.Errorf("invalid MCP_ENABLED: must be true or false")
		}
	}
	stringValues := []struct {
		name   string
		target *string
	}{
		{"ROUTER_LISTEN_ADDR", &c.ListenAddr}, {"ROUTER_CLOUD_URL", &c.CloudURL},
		{"FIRECRAWL_CLOUD_API_KEY", &c.CloudAPIKey}, {"ROUTER_API_KEY", &c.APIKey},
		{"ROUTER_CACHE_DIR", &c.CacheDir}, {"ROUTER_LEDGER_PATH", &c.LedgerPath},
	}
	for _, value := range stringValues {
		if raw := getenv(value.name); raw != "" {
			*value.target = raw
		}
	}
	durations := []struct {
		name   string
		target *time.Duration
	}{
		{"ROUTER_SEARCH_TTL", &c.SearchTTL}, {"ROUTER_SCRAPE_TTL", &c.ScrapeTTL}, {"ROUTER_HTTP_TIMEOUT", &c.HTTPTimeout},
		{"ROUTER_SERVER_READ_TIMEOUT", &c.ServerReadTimeout},
	}
	for _, value := range durations {
		if raw := getenv(value.name); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				return Config{}, fmt.Errorf("invalid %s", value.name)
			}
			*value.target = parsed
		}
	}
	int64s := []struct {
		name   string
		target *int64
	}{
		{"ROUTER_MAX_REQUEST_BYTES", &c.MaxRequestBytes}, {"ROUTER_MAX_RESPONSE_BYTES", &c.MaxResponseBytes},
		{"ROUTER_CACHE_MAX_BYTES", &c.CacheMaxBytes},
	}
	for _, value := range int64s {
		if raw := getenv(value.name); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return Config{}, fmt.Errorf("invalid %s", value.name)
			}
			*value.target = parsed
		}
	}
	ints := []struct {
		name   string
		target *int
	}{
		{"ROUTER_CACHE_MAX_ENTRY_BYTES", &c.CacheMaxEntryBytes}, {"ROUTER_MAX_INFLIGHT", &c.MaxInflight},
		{"ROUTER_DAILY_CLOUD_CREDITS", &c.DailyCloudCredits},
		{"ROUTER_MONTHLY_CLOUD_CREDITS", &c.MonthlyCloudCredits}, {"ROUTER_MONTHLY_RESET_DAY", &c.MonthlyResetDay},
		{"ROUTER_CLOUD_BURST_CREDITS", &c.CloudBurstCredits},
		{"ROUTER_CLOUD_REFILL_CREDITS_PER_DAY", &c.CloudRefillCreditsPerDay},
		{"ROUTER_SEARCH_ESTIMATED_CREDITS", &c.SearchEstimatedCredits},
		{"ROUTER_SCRAPE_ESTIMATED_CREDITS", &c.ScrapeEstimatedCredits},
	}
	for _, value := range ints {
		if raw := getenv(value.name); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				return Config{}, fmt.Errorf("invalid %s", value.name)
			}
			*value.target = parsed
		}
	}
	if err := validate(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func validate(c Config) error {
	if c.LocalURL == "" {
		return fmt.Errorf("ROUTER_LOCAL_URL is required")
	}
	for name, raw := range map[string]string{"ROUTER_LOCAL_URL": c.LocalURL, "ROUTER_CLOUD_URL": c.CloudURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("invalid %s", name)
		}
	}
	if c.SearchTTL < 0 {
		return fmt.Errorf("ROUTER_SEARCH_TTL must not be negative")
	}
	if c.ScrapeTTL < 0 {
		return fmt.Errorf("ROUTER_SCRAPE_TTL must not be negative")
	}
	if c.HTTPTimeout <= 0 {
		return fmt.Errorf("ROUTER_HTTP_TIMEOUT must be positive")
	}
	if c.ServerReadTimeout <= 0 {
		return fmt.Errorf("ROUTER_SERVER_READ_TIMEOUT must be positive")
	}
	if c.MaxRequestBytes <= 0 {
		return fmt.Errorf("ROUTER_MAX_REQUEST_BYTES must be positive")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("ROUTER_MAX_RESPONSE_BYTES must be positive")
	}
	if c.CacheMaxEntryBytes <= 0 {
		return fmt.Errorf("ROUTER_CACHE_MAX_ENTRY_BYTES must be positive")
	}
	if c.CacheMaxBytes <= 0 {
		return fmt.Errorf("ROUTER_CACHE_MAX_BYTES must be positive")
	}
	if c.MaxInflight <= 0 {
		return fmt.Errorf("ROUTER_MAX_INFLIGHT must be positive")
	}
	if c.DailyCloudCredits < 0 {
		return fmt.Errorf("ROUTER_DAILY_CLOUD_CREDITS must not be negative")
	}
	if c.MonthlyCloudCredits < 0 {
		return fmt.Errorf("ROUTER_MONTHLY_CLOUD_CREDITS must not be negative")
	}
	if c.CloudBurstCredits < 0 {
		return fmt.Errorf("ROUTER_CLOUD_BURST_CREDITS must not be negative")
	}
	if c.CloudRefillCreditsPerDay < 0 {
		return fmt.Errorf("ROUTER_CLOUD_REFILL_CREDITS_PER_DAY must not be negative")
	}
	if (c.CloudBurstCredits == 0) != (c.CloudRefillCreditsPerDay == 0) {
		return fmt.Errorf("ROUTER_CLOUD_BURST_CREDITS and ROUTER_CLOUD_REFILL_CREDITS_PER_DAY must both be zero or both be positive")
	}
	if c.MonthlyResetDay < 1 || c.MonthlyResetDay > 28 {
		return fmt.Errorf("ROUTER_MONTHLY_RESET_DAY must be between 1 and 28")
	}
	if c.SearchEstimatedCredits <= 0 {
		return fmt.Errorf("ROUTER_SEARCH_ESTIMATED_CREDITS must be positive")
	}
	if c.ScrapeEstimatedCredits <= 0 {
		return fmt.Errorf("ROUTER_SCRAPE_ESTIMATED_CREDITS must be positive")
	}
	if c.CloudAPIKey != "" && c.DailyCloudCredits == 0 && c.MonthlyCloudCredits == 0 {
		return fmt.Errorf("cloud API key requires a positive daily or monthly cloud credit limit")
	}
	return nil
}
