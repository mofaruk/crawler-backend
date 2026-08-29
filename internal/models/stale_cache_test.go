package models

import "testing"

// s-maxage is the directive aimed at shared caches, which is what a CDN is, so
// it must win over max-age. The common WordPress/Cloudflare pairing of a short
// browser lifetime with a long CDN one ("s-maxage=31557600, max-age=600") is a
// correctly cached page; judging it against max-age alone would report every
// such page as stale.
func TestCacheFreshnessPrefersSMaxAge(t *testing.T) {
	cases := []struct {
		name       string
		headers    map[string]string
		wantAge    int
		wantMaxAge int
		wantOK     bool
	}{
		{
			name:       "s-maxage wins over max-age",
			headers:    map[string]string{"Age": "3944", "Cache-Control": "s-maxage=31557600, max-age=600"},
			wantAge:    3944,
			wantMaxAge: 31557600,
			wantOK:     true,
		},
		{
			name:       "max-age is used when there is no s-maxage",
			headers:    map[string]string{"Age": "900", "Cache-Control": "public, max-age=600"},
			wantAge:    900,
			wantMaxAge: 600,
			wantOK:     true,
		},
		{
			name:       "order within the header does not matter",
			headers:    map[string]string{"Age": "10", "Cache-Control": "max-age=600, s-maxage=86400"},
			wantAge:    10,
			wantMaxAge: 86400,
			wantOK:     true,
		},
		{
			name:       "header names are matched case-insensitively",
			headers:    map[string]string{"age": "120", "cache-control": "MAX-AGE=60"},
			wantAge:    120,
			wantMaxAge: 60,
			wantOK:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			age, maxAge, ok := cacheFreshness(tc.headers)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if age != tc.wantAge {
				t.Errorf("age = %d, want %d", age, tc.wantAge)
			}
			if maxAge != tc.wantMaxAge {
				t.Errorf("maxAge = %d, want %d", maxAge, tc.wantMaxAge)
			}
		})
	}
}

// Freshness cannot be judged without both numbers, and a copy the origin
// forbids caching is already covered by cache_bypass — reporting it as stale
// as well would be noise.
func TestCacheFreshnessUndeterminable(t *testing.T) {
	cases := map[string]map[string]string{
		"no Age header":         {"Cache-Control": "max-age=600"},
		"no Cache-Control":      {"Age": "100"},
		"neither":               {},
		"no-store":              {"Age": "100", "Cache-Control": "no-store, max-age=600"},
		"no-cache":              {"Age": "100", "Cache-Control": "no-cache"},
		"no lifetime directive": {"Age": "100", "Cache-Control": "public, must-revalidate"},
		"unparseable Age":       {"Age": "soon", "Cache-Control": "max-age=600"},
		"negative Age":          {"Age": "-5", "Cache-Control": "max-age=600"},
		"unparseable max-age":   {"Age": "100", "Cache-Control": "max-age=forever"},
	}

	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, ok := cacheFreshness(headers); ok {
				t.Errorf("cacheFreshness(%v) reported ok, want not determinable", headers)
			}
		})
	}
}

// "max-age" must not be read out of "s-maxage": they are different directives
// and the substring match would return the wrong lifetime.
func TestCacheDirectiveDoesNotMatchInsideAnotherName(t *testing.T) {
	if v, found := cacheDirectiveSeconds("s-maxage=31557600", "max-age"); found {
		t.Errorf("max-age matched inside s-maxage, returning %d", v)
	}
	if v, found := cacheDirectiveSeconds("s-maxage=86400", "s-maxage"); !found || v != 86400 {
		t.Errorf("s-maxage = %d (found=%v), want 86400", v, found)
	}
}

// The whole point of the check: a page that is cached but past its policy.
func TestClassifyReportsStaleCache(t *testing.T) {
	state := URLState{
		URL:        "https://example.dk/",
		StatusCode: 200,
		Headers: map[string]string{
			"CF-Cache-Status": "HIT",
			"Age":             "7200",                // 2 hours old
			"Cache-Control":   "public, max-age=600", // 10 minutes allowed
		},
	}

	if !hasIssue(ClassifyURL(state, nil), "cache_stale") {
		t.Error("a copy 2 hours old under a 10-minute policy was not reported as stale")
	}
}

// A correctly cached page must stay silent, or the finding is worthless.
func TestClassifyDoesNotReportFreshCache(t *testing.T) {
	fresh := []struct {
		name    string
		headers map[string]string
	}{
		{
			name:    "well within its lifetime",
			headers: map[string]string{"CF-Cache-Status": "HIT", "Age": "60", "Cache-Control": "public, max-age=600"},
		},
		{
			// The real QLiving case that prompted this feature.
			name:    "long s-maxage with a short max-age",
			headers: map[string]string{"CF-Cache-Status": "HIT", "Age": "3944", "Cache-Control": "s-maxage=31557600, max-age=600"},
		},
		{
			name:    "just past the lifetime, inside the grace margin",
			headers: map[string]string{"CF-Cache-Status": "HIT", "Age": "630", "Cache-Control": "public, max-age=600"},
		},
	}

	for _, tc := range fresh {
		t.Run(tc.name, func(t *testing.T) {
			state := URLState{URL: "https://example.dk/", StatusCode: 200, Headers: tc.headers}
			if hasIssue(ClassifyURL(state, nil), "cache_stale") {
				t.Errorf("a correctly cached page was reported stale: %v", tc.headers)
			}
		})
	}
}

// Severity has to separate "lagging slightly" from "long abandoned", or every
// stale page looks equally urgent.
func TestStaleCacheSeverityScalesWithOverrun(t *testing.T) {
	cases := []struct {
		name string
		age  string
		want int
	}{
		{name: "moderately over is a warning", age: "1200", want: SeverityWarning}, // 2x
		{name: "far over is critical", age: "600000", want: SeverityCritical},      // 1000x
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := URLState{
				URL:        "https://example.dk/",
				StatusCode: 200,
				Headers: map[string]string{
					"CF-Cache-Status": "HIT",
					"Age":             tc.age,
					"Cache-Control":   "public, max-age=600",
				},
			}

			for _, issue := range ClassifyURL(state, nil) {
				if issue.Kind == "cache_stale" {
					if issue.Severity != tc.want {
						t.Errorf("severity = %d, want %d", issue.Severity, tc.want)
					}
					return
				}
			}
			t.Fatal("no cache_stale issue was reported")
		})
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[int]string{
		30:     "30s",
		600:    "10m",
		7200:   "2.0h",
		259200: "3.0 days",
	}

	for seconds, want := range cases {
		if got := humanDuration(seconds); got != want {
			t.Errorf("humanDuration(%d) = %q, want %q", seconds, got, want)
		}
	}
}

func hasIssue(issues []SiteIssue, kind string) bool {
	for _, i := range issues {
		if i.Kind == kind {
			return true
		}
	}
	return false
}
