package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c != Default() {
		t.Fatalf("got %+v, want defaults %+v", c, Default())
	}
	if got := c.BaseHost(); got != "localhost" {
		t.Fatalf("BaseHost = %q, want localhost", got)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"LINKLY_ADDR":                 "127.0.0.1:9000",
		"LINKLY_DB":                   "/data/x.db",
		"LINKLY_BASE_URL":             "https://Sho.rt/",
		"LINKLY_ADMIN_TOKEN":          "s3cret",
		"LINKLY_TRUST_PROXY":          "true",
		"LINKLY_RATE_LIMIT":           "2.5",
		"LINKLY_RATE_BURST":           "3",
		"LINKLY_RATE_IDLE_TTL":        "1m",
		"LINKLY_CLICK_BUFFER":         "10",
		"LINKLY_CLICK_BATCH":          "5",
		"LINKLY_CLICK_FLUSH_INTERVAL": "250ms",
		"LINKLY_SHUTDOWN_TIMEOUT":     "3s",
		"LINKLY_LOG_LEVEL":            "debug",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		Addr: "127.0.0.1:9000", DBPath: "/data/x.db", BaseURL: "https://Sho.rt",
		AdminToken: "s3cret", TrustProxy: true, RateLimit: 2.5, RateBurst: 3,
		RateIdleTTL: time.Minute, ClickBuffer: 10, ClickBatch: 5,
		ClickFlushInterval: 250 * time.Millisecond, ShutdownTimeout: 3 * time.Second,
		LogLevel: slog.LevelDebug,
	}
	if c != want {
		t.Fatalf("got  %+v\nwant %+v", c, want)
	}
	if got := c.BaseHost(); got != "sho.rt" {
		t.Fatalf("BaseHost = %q, want sho.rt", got)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string // substrings that must all appear in the error
	}{
		{"bad bool", map[string]string{"LINKLY_TRUST_PROXY": "yes please"}, []string{"LINKLY_TRUST_PROXY"}},
		{"bad duration", map[string]string{"LINKLY_CLICK_FLUSH_INTERVAL": "soon"}, []string{"LINKLY_CLICK_FLUSH_INTERVAL"}},
		{"bad scheme", map[string]string{"LINKLY_BASE_URL": "ftp://x"}, []string{"http or https"}},
		{"base url with path", map[string]string{"LINKLY_BASE_URL": "https://x.io/s"}, []string{"origin"}},
		{"negative rate", map[string]string{"LINKLY_RATE_LIMIT": "-1"}, []string{"LINKLY_RATE_LIMIT"}},
		{"NaN rate", map[string]string{"LINKLY_RATE_LIMIT": "NaN"}, []string{"LINKLY_RATE_LIMIT", "finite"}},
		{"Inf rate", map[string]string{"LINKLY_RATE_LIMIT": "Inf"}, []string{"LINKLY_RATE_LIMIT", "finite"}},
		{"+Inf rate", map[string]string{"LINKLY_RATE_LIMIT": "+infinity"}, []string{"LINKLY_RATE_LIMIT", "finite"}},
		{"base url with userinfo", map[string]string{"LINKLY_BASE_URL": "https://admin:pw@sho.rt"}, []string{"LINKLY_BASE_URL", "credentials"}},
		{"base url with user", map[string]string{"LINKLY_BASE_URL": "https://admin@sho.rt"}, []string{"LINKLY_BASE_URL", "credentials"}},
		{"base url trailing dot", map[string]string{"LINKLY_BASE_URL": "https://sho.rt."}, []string{"LINKLY_BASE_URL", "trailing dot"}},
		{"base url trailing dot with port", map[string]string{"LINKLY_BASE_URL": "https://sho.rt.:8443"}, []string{"LINKLY_BASE_URL", "trailing dot"}},
		{"base url non-ASCII host", map[string]string{"LINKLY_BASE_URL": "https://exämple.com"}, []string{"LINKLY_BASE_URL", "ASCII"}},
		{"zero burst", map[string]string{"LINKLY_RATE_BURST": "0"}, []string{"LINKLY_RATE_BURST"}},
		{"bad level", map[string]string{"LINKLY_LOG_LEVEL": "loud"}, []string{"LINKLY_LOG_LEVEL"}},
		{
			"reports all errors",
			map[string]string{"LINKLY_CLICK_BUFFER": "0", "LINKLY_CLICK_BATCH": "x"},
			[]string{"LINKLY_CLICK_BUFFER", "LINKLY_CLICK_BATCH"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, s := range tt.want {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not mention %q", err, s)
				}
			}
		})
	}
}
