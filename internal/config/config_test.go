package config

import (
	"strings"
	"testing"
	"time"
)

// Every env helper falls back on both "unset" and "set but unparseable". A
// silent zero value here would mean a zero timeout or zero worker count in
// production, which fails in ways that look like a network problem.
func TestEnvStr(t *testing.T) {
	cases := []struct {
		name     string
		set      bool
		value    string
		fallback string
		want     string
	}{
		{"unset uses the fallback", false, "", "default", "default"},
		{"empty value is treated as unset", true, "", "default", "default"},
		{"set value wins", true, "custom", "default", "custom"},
		{"whitespace is a value, not emptiness", true, "  ", "default", "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const key = "CRAWLER_TEST_STR"
			if tc.set {
				t.Setenv(key, tc.value)
			}
			if got := envStr(key, tc.fallback); got != tc.want {
				t.Fatalf("envStr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnvInt(t *testing.T) {
	cases := []struct {
		name     string
		set      bool
		value    string
		fallback int
		want     int
	}{
		{"unset uses the fallback", false, "", 100, 100},
		{"a valid integer wins", true, "250", 100, 250},
		{"zero is a legitimate configured value", true, "0", 100, 0},
		{"a negative value is passed through", true, "-1", 100, -1},
		{"a non-numeric value falls back rather than becoming zero", true, "many", 100, 100},
		{"a float falls back", true, "1.5", 100, 100},
		{"an empty value is treated as unset", true, "", 100, 100},
		{"surrounding whitespace is not trimmed and falls back", true, " 250 ", 100, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const key = "CRAWLER_TEST_INT"
			if tc.set {
				t.Setenv(key, tc.value)
			}
			if got := envInt(key, tc.fallback); got != tc.want {
				t.Fatalf("envInt = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEnvDuration(t *testing.T) {
	cases := []struct {
		name     string
		set      bool
		value    string
		fallback time.Duration
		want     time.Duration
	}{
		{"unset uses the fallback", false, "", 30 * time.Second, 30 * time.Second},
		{"seconds", true, "45s", 30 * time.Second, 45 * time.Second},
		{"milliseconds", true, "250ms", 30 * time.Second, 250 * time.Millisecond},
		{"minutes", true, "5m", 30 * time.Second, 5 * time.Minute},
		{"compound duration", true, "1m30s", 30 * time.Second, 90 * time.Second},
		{"zero is a legitimate configured value", true, "0s", 30 * time.Second, 0},
		{"a bare number has no unit and falls back", true, "30", 30 * time.Second, 30 * time.Second},
		{"nonsense falls back", true, "soon", 30 * time.Second, 30 * time.Second},
		{"an empty value is treated as unset", true, "", 30 * time.Second, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const key = "CRAWLER_TEST_DUR"
			if tc.set {
				t.Setenv(key, tc.value)
			}
			if got := envDuration(key, tc.fallback); got != tc.want {
				t.Fatalf("envDuration = %v, want %v", got, tc.want)
			}
		})
	}
}

// splitAndTrim parses ALLOWED_ORIGINS. A stray blank entry becoming an empty
// allowed origin would either widen CORS or break it, depending on the
// comparison, so blanks must be dropped rather than kept.
func TestSplitAndTrim(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty string yields nil", "", nil},
		{"whitespace only yields nil", "   ", nil},
		{"single value", "https://a.dk", []string{"https://a.dk"}},
		{"several values", "https://a.dk,https://b.dk", []string{"https://a.dk", "https://b.dk"}},
		{"surrounding whitespace trimmed", " https://a.dk , https://b.dk ", []string{"https://a.dk", "https://b.dk"}},
		{"blank entries dropped", "https://a.dk,,https://b.dk", []string{"https://a.dk", "https://b.dk"}},
		{"trailing comma dropped", "https://a.dk,", []string{"https://a.dk"}},
		{"only commas yields empty", ",,,", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitAndTrim(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitAndTrim(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("splitAndTrim(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// The two protection switches must default to their SAFE positions. A default
// that disabled auth or the SSRF guard would be a production incident on any
// deployment that forgot to set them.
func TestLoadDefaultsToTheSafeSide(t *testing.T) {
	cfg := Load()

	if cfg.AllowPrivateTargets {
		t.Error("ALLOW_PRIVATE_TARGETS must default to false — otherwise the API is an SSRF primitive")
	}
	if cfg.AllowedOrigins != nil {
		t.Errorf("ALLOWED_ORIGINS must default to nil, got %v", cfg.AllowedOrigins)
	}
	if cfg.CrawlerMaxRetries <= 0 {
		t.Errorf("CrawlerMaxRetries = %d, want a positive default", cfg.CrawlerMaxRetries)
	}
	if cfg.CrawlerTimeout <= 0 {
		t.Errorf("CrawlerTimeout = %v, want a positive default", cfg.CrawlerTimeout)
	}
	if cfg.WorkerConcurrency <= 0 {
		t.Errorf("WorkerConcurrency = %d, want a positive default", cfg.WorkerConcurrency)
	}
}

// ALLOW_PRIVATE_TARGETS disables the SSRF guard, so how it parses matters.
// Deploy files write booleans several ways; each accepted spelling must mean
// what it looks like, and anything unrecognised must keep the safe default
// rather than turning protection off by accident.
func TestAllowPrivateTargetsParsing(t *testing.T) {
	cases := map[string]bool{
		"true":    true,
		"TRUE":    true,
		"True":    true,
		"  true ": true,
		"1":       true,
		"yes":     true,
		"on":      true,

		"false": false,
		"FALSE": false,
		"0":     false,
		"no":    false,
		"off":   false,

		// Unrecognised values keep the default, which is off.
		"":        false,
		"maybe":   false,
		"enabled": false,
	}

	for value, want := range cases {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ALLOW_PRIVATE_TARGETS", value)
			if got := Load().AllowPrivateTargets; got != want {
				t.Fatalf("ALLOW_PRIVATE_TARGETS=%q gave %v, want %v", value, got, want)
			}
		})
	}
}

// The default User-Agent impersonates a browser on purpose: CDNs serve
// different cache behaviour to unrecognised bots, which would make the
// reported cache headers describe the bot's experience, not a visitor's.
func TestDefaultUserAgentLooksLikeABrowser(t *testing.T) {
	cfg := Load()
	if cfg.DefaultUserAgent == "" {
		t.Fatal("DefaultUserAgent must not be empty")
	}
	for _, want := range []string{"Mozilla/5.0", "Chrome/"} {
		if !strings.Contains(cfg.DefaultUserAgent, want) {
			t.Errorf("default UA %q should contain %q", cfg.DefaultUserAgent, want)
		}
	}
}

func TestLoadHonoursOverrides(t *testing.T) {
	t.Setenv("SERVICE_ROLE", "worker")
	t.Setenv("WORKER_CONCURRENCY", "7")
	t.Setenv("CRAWLER_TIMEOUT", "12s")
	t.Setenv("ALLOWED_ORIGINS", "https://a.dk, https://b.dk")

	cfg := Load()
	if cfg.ServiceRole != "worker" {
		t.Errorf("ServiceRole = %q", cfg.ServiceRole)
	}
	if cfg.WorkerConcurrency != 7 {
		t.Errorf("WorkerConcurrency = %d", cfg.WorkerConcurrency)
	}
	if cfg.CrawlerTimeout != 12*time.Second {
		t.Errorf("CrawlerTimeout = %v", cfg.CrawlerTimeout)
	}
	if len(cfg.AllowedOrigins) != 2 {
		t.Errorf("AllowedOrigins = %v", cfg.AllowedOrigins)
	}
}

// LogLevel is lower-cased on load, since zerolog only accepts lowercase names
// and a rejected level silently changes how much the service logs.
func TestLogLevelIsNormalised(t *testing.T) {
	t.Setenv("LOG_LEVEL", "DEBUG")
	if got := Load().LogLevel; got != "debug" {
		t.Fatalf("LogLevel = %q, want %q", got, "debug")
	}
}
