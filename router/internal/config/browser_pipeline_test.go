package config_test

import (
	"strings"
	"testing"

	"web-retrieval/internal/config"
)

func TestBrowserPipelineConfigIsExplicitAndStrict(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    bool
		invalid bool
	}{{"", false, false}, {"false", false, false}, {"true", true, false}, {"TRUE", false, true}, {"1", false, true}} {
		t.Run("value="+test.value, func(t *testing.T) {
			values := map[string]string{"ROUTER_LOCAL_URL": "http://local:3000", "ROUTER_BROWSER_PIPELINE_ENABLED": test.value}
			got, err := config.Parse(func(name string) string { return values[name] })
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "ROUTER_BROWSER_PIPELINE_ENABLED") {
					t.Fatalf("invalid opt-in = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.BrowserPipelineEnabled != test.want {
				t.Fatalf("pipeline enabled = %t, want %t", got.BrowserPipelineEnabled, test.want)
			}
		})
	}
}
