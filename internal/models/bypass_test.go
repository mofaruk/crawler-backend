package models

import "testing"

// Jens's rule: one bypass is natural — a cold URL, a purge, a request that
// happened to carry a cookie. It is the repeat that says the CDN will never
// store this page.
func TestBypassReportedOnlyWhenRepeated(t *testing.T) {
	cases := []struct {
		name     string
		bypasses int
		want     bool
	}{
		{"a single bypass is natural and not reported", 1, false},
		{"never bypassed is obviously not reported", 0, false},
		{"twice is no longer natural", 2, true},
		{"and more so at ten", 10, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ClassifyURL(URLState{
				URL:         "https://e.dk/a",
				StatusCode:  200,
				Headers:     map[string]string{"CF-Cache-Status": "BYPASS"},
				Occurrences: 12,
				Bypasses:    tc.bypasses,
			}, nil)

			_, got := issueByKind(issues, "cache_bypass")
			if got != tc.want {
				t.Fatalf("with %d bypasses: reported = %v, want %v", tc.bypasses, got, tc.want)
			}
		})
	}
}

// Occurrences counts crawls, not bypasses. A URL crawled many times that
// bypassed once must not be reported: reading the wrong counter is exactly the
// mistake this rule is meant to avoid.
func TestManyCrawlsWithOneBypassIsNotReported(t *testing.T) {
	issues := ClassifyURL(URLState{
		URL:         "https://e.dk/a",
		StatusCode:  200,
		Headers:     map[string]string{"CF-Cache-Status": "BYPASS"},
		Occurrences: 50,
		Bypasses:    1,
	}, nil)

	if _, got := issueByKind(issues, "cache_bypass"); got {
		t.Fatal("a URL crawled 50 times that bypassed once must not be reported")
	}
}
