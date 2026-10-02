package config_test

import (
	"strings"
	"testing"
	"time"

	"web-retrieval/internal/config"
)

func TestParseDefaults(t *testing.T) {
	values := map[string]string{"ROUTER_LOCAL_URL": "http://camofox:3000"}
	got, err := config.Parse(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if got.ListenAddr != ":8080" || got.LocalURL != values["ROUTER_LOCAL_URL"] || got.CloudURL != "https://api.firecrawl.dev" {
		t.Fatalf("addresses = %#v", got)
	}
	if got.CacheDir != "/data/cache" || got.LedgerPath != "/data/budget.json" {
		t.Fatalf("storage = %#v", got)
	}
	if got.SearchTTL != 15*time.Minute || got.ScrapeTTL != 24*time.Hour || got.HTTPTimeout != 60*time.Second || got.ServerReadTimeout != 30*time.Second {
		t.Fatalf("durations = %#v", got)
	}
	if got.MaxRequestBytes != 2<<20 || got.MaxResponseBytes != 16<<20 || got.CacheMaxEntryBytes != 16<<20 || got.CacheMaxBytes != 1<<30 {
		t.Fatalf("limits = %#v", got)
	}
	if got.MaxInflight != 64 {
		t.Fatalf("max inflight = %d, want 64", got.MaxInflight)
	}
	if got.MaxParseBytes != 50<<20 || got.CloudCreditFloor != 50 {
		t.Fatalf("parse bytes = %d credit floor = %d, want 50 MiB and 50", got.MaxParseBytes, got.CloudCreditFloor)
	}
	if got.MCPEnabled {
		t.Fatal("MCP enabled by default, want disabled")
	}
	if got.DailyCloudCredits != 0 || got.MonthlyCloudCredits != 0 || got.CloudBurstCredits != 0 || got.CloudRefillCreditsPerDay != 0 || got.SearchEstimatedCredits != 2 || got.ScrapeEstimatedCredits != 1 {
		t.Fatalf("credits = %#v", got)
	}
}

func TestParseMCPEnabledStrictBoolean(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: "true", want: true},
		{value: "false", want: false},
	} {
		values := map[string]string{"ROUTER_LOCAL_URL": "http://local:3000", "ROUTER_API_KEY": "secret", "MCP_ENABLED": test.value}
		got, err := config.Parse(func(name string) string { return values[name] })
		if err != nil {
			t.Fatalf("MCP_ENABLED=%q: %v", test.value, err)
		}
		if got.MCPEnabled != test.want {
			t.Fatalf("MCP_ENABLED=%q parsed as %t, want %t", test.value, got.MCPEnabled, test.want)
		}
	}
	for _, value := range []string{"1", "TRUE", "False", "yes", " true "} {
		values := map[string]string{"ROUTER_LOCAL_URL": "http://local:3000", "MCP_ENABLED": value}
		if _, err := config.Parse(func(name string) string { return values[name] }); err == nil {
			t.Fatalf("MCP_ENABLED=%q succeeded, want strict boolean error", value)
		}
	}
}

func TestParseRejectsEnabledMCPWithoutAPIKey(t *testing.T) {
	values := map[string]string{"ROUTER_LOCAL_URL": "http://local:3000", "MCP_ENABLED": "true"}
	if _, err := config.Parse(func(name string) string { return values[name] }); err == nil {
		t.Fatal("enabled MCP without ROUTER_API_KEY succeeded, want error")
	}
}

func TestParseMonthlyResetDay(t *testing.T) {
	tests := []struct {
		name, value string
		want        int
		wantError   bool
	}{
		{name: "default", want: 1},
		{name: "configured", value: "3", want: 3},
		{name: "below range", value: "0", wantError: true},
		{name: "above range", value: "29", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{
				"ROUTER_LOCAL_URL":         "http://local:3000",
				"ROUTER_MONTHLY_RESET_DAY": test.value,
			}
			got, err := config.Parse(func(name string) string { return values[name] })
			if test.wantError {
				if err == nil {
					t.Fatal("expected validation error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.MonthlyResetDay != test.want {
				t.Fatalf("monthly reset day = %d, want %d", got.MonthlyResetDay, test.want)
			}
		})
	}
}

func TestParseRejectsHalfConfiguredCloudBurstPolicy(t *testing.T) {
	for _, variable := range []string{"ROUTER_CLOUD_BURST_CREDITS", "ROUTER_CLOUD_REFILL_CREDITS_PER_DAY"} {
		values := map[string]string{
			"ROUTER_LOCAL_URL": "http://local:3000",
			variable:           "5",
		}
		if _, err := config.Parse(func(name string) string { return values[name] }); err == nil {
			t.Fatalf("configuration with only %s succeeded, want error", variable)
		}
	}
}

func TestParseRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name, variable, value string
		extras                map[string]string
	}{
		{"local required", "ROUTER_LOCAL_URL", "", nil},
		{"local absolute URL", "ROUTER_LOCAL_URL", "localhost:3000", nil},
		{"cloud absolute URL", "ROUTER_CLOUD_URL", "://bad", nil},
		{"duration", "ROUTER_HTTP_TIMEOUT", "later", nil},
		{"positive timeout", "ROUTER_HTTP_TIMEOUT", "0s", nil},
		{"server read timeout duration", "ROUTER_SERVER_READ_TIMEOUT", "later", nil},
		{"positive server read timeout", "ROUTER_SERVER_READ_TIMEOUT", "0s", nil},
		{"nonnegative ttl", "ROUTER_SEARCH_TTL", "-1s", nil},
		{"positive bytes", "ROUTER_MAX_REQUEST_BYTES", "0", nil},
		{"positive cache total", "ROUTER_CACHE_MAX_BYTES", "0", nil},
		{"positive parse bytes", "ROUTER_MAX_PARSE_BYTES", "0", nil},
		{"nonnegative credit floor", "ROUTER_CLOUD_CREDIT_FLOOR", "-1", nil},
		{"integer credit floor", "ROUTER_CLOUD_CREDIT_FLOOR", "many", nil},
		{"positive max inflight", "ROUTER_MAX_INFLIGHT", "0", nil},
		{"nonnegative credits", "ROUTER_DAILY_CLOUD_CREDITS", "-1", nil},
		{"nonnegative burst", "ROUTER_CLOUD_BURST_CREDITS", "-1", nil},
		{"nonnegative refill", "ROUTER_CLOUD_REFILL_CREDITS_PER_DAY", "-1", nil},
		{"positive estimate", "ROUTER_SEARCH_ESTIMATED_CREDITS", "0", nil},
		{"cloud key needs hard limit", "FIRECRAWL_CLOUD_API_KEY", "secret", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{"ROUTER_LOCAL_URL": "http://local:3000", test.variable: test.value}
			for key, value := range test.extras {
				values[key] = value
			}
			_, err := config.Parse(func(name string) string { return values[name] })
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestParseErrorsRedactSecrets(t *testing.T) {
	const cloudSecret = "cloud-secret-must-not-leak"
	const ingressSecret = "ingress-secret-must-not-leak"
	values := map[string]string{
		"ROUTER_LOCAL_URL": "not-a-url", "FIRECRAWL_CLOUD_API_KEY": cloudSecret, "ROUTER_API_KEY": ingressSecret,
	}
	_, err := config.Parse(func(name string) string { return values[name] })
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), cloudSecret) || strings.Contains(err.Error(), ingressSecret) {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestParseOverrides(t *testing.T) {
	values := map[string]string{
		"ROUTER_LISTEN_ADDR": "127.0.0.1:9090", "ROUTER_LOCAL_URL": "http://local:1",
		"ROUTER_CLOUD_URL": "https://cloud.example", "FIRECRAWL_CLOUD_API_KEY": "cloud-secret", "ROUTER_API_KEY": "inbound-secret",
		"ROUTER_CACHE_DIR": "/tmp/cache", "ROUTER_LEDGER_PATH": "/tmp/ledger.json",
		"ROUTER_SEARCH_TTL": "30s", "ROUTER_SCRAPE_TTL": "2h", "ROUTER_HTTP_TIMEOUT": "7s", "ROUTER_SERVER_READ_TIMEOUT": "11s",
		"ROUTER_MAX_REQUEST_BYTES": "100", "ROUTER_MAX_RESPONSE_BYTES": "200", "ROUTER_CACHE_MAX_ENTRY_BYTES": "150", "ROUTER_CACHE_MAX_BYTES": "300",
		"ROUTER_MAX_INFLIGHT":        "7",
		"ROUTER_DAILY_CLOUD_CREDITS": "10", "ROUTER_MONTHLY_CLOUD_CREDITS": "100",
		"ROUTER_CLOUD_BURST_CREDITS": "8", "ROUTER_CLOUD_REFILL_CREDITS_PER_DAY": "6",
		"ROUTER_SEARCH_ESTIMATED_CREDITS": "3", "ROUTER_SCRAPE_ESTIMATED_CREDITS": "4",
	}
	got, err := config.Parse(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if got.ListenAddr != values["ROUTER_LISTEN_ADDR"] || got.CloudAPIKey != "cloud-secret" || got.APIKey != "inbound-secret" {
		t.Fatalf("strings = %#v", got)
	}
	if got.SearchTTL != 30*time.Second || got.ScrapeTTL != 2*time.Hour || got.HTTPTimeout != 7*time.Second || got.ServerReadTimeout != 11*time.Second {
		t.Fatalf("durations = %#v", got)
	}
	if got.MaxRequestBytes != 100 || got.MaxResponseBytes != 200 || got.CacheMaxEntryBytes != 150 || got.CacheMaxBytes != 300 {
		t.Fatalf("bytes = %#v", got)
	}
	if got.MaxInflight != 7 {
		t.Fatalf("max inflight = %d, want 7", got.MaxInflight)
	}
	if got.DailyCloudCredits != 10 || got.MonthlyCloudCredits != 100 || got.CloudBurstCredits != 8 || got.CloudRefillCreditsPerDay != 6 || got.SearchEstimatedCredits != 3 || got.ScrapeEstimatedCredits != 4 {
		t.Fatalf("credits = %#v", got)
	}
}
