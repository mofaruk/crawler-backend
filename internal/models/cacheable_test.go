package models

import "testing"

// The predicate decides whether the crawler keeps trying to warm a page, so a
// false negative silently stops warming a page that would have cached.
func TestCanEverCache(t *testing.T) {
	cases := []struct {
		name         string
		cacheControl string
		want         bool
	}{
		// The case this was written for: measured on a real site, six
		// consecutive fetches, MISS every time, Age never present.
		{"max-age=0 forbids storage", "public, max-age=0", false},
		{"max-age=0 with must-revalidate", "public, max-age=0, must-revalidate", false},
		{"no-store forbids storage", "no-store", false},
		{"no-cache forbids storage", "no-cache", false},
		{"private is not for shared caches", "private, max-age=600", false},

		{"a real lifetime caches", "public, max-age=600", true},
		{"long lifetime caches", "max-age=31557600", true},

		// s-maxage targets shared caches, so it wins over max-age. This exact
		// pairing is the standard WordPress/Cloudflare setup and must not be
		// reported as forbidden.
		{"s-maxage wins over max-age=0", "s-maxage=86400, max-age=0", true},
		{"s-maxage=0 forbids even with a browser lifetime", "s-maxage=0, max-age=600", false},

		// No policy is ambiguous, not forbidden — CDNs frequently cache anyway,
		// and no_cache_control already reports the ambiguity separately.
		{"absent header is not a refusal", "", true},
		{"unrelated directives do not forbid", "public", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CanEverCache(map[string]string{"Cache-Control": c.cacheControl})
			if got != c.want {
				t.Errorf("CanEverCache(%q) = %v, want %v", c.cacheControl, got, c.want)
			}
		})
	}
}

// Header casing varies by server, and a missed lookup would read as "no policy"
// and quietly re-enable warming on a page that cannot cache.
func TestCanEverCacheIgnoresHeaderCasing(t *testing.T) {
	for _, key := range []string{"cache-control", "Cache-Control", "CACHE-CONTROL"} {
		if CanEverCache(map[string]string{key: "max-age=0"}) {
			t.Errorf("header %q: max-age=0 should forbid caching", key)
		}
	}
}

// The threshold decides when a crawl gives up, so a site saved before the field
// existed must still get the default rather than a limit of zero, which would
// abort every crawl on the first uncacheable page.
func TestUncacheableLimit(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		want       int
	}{
		{"unset falls back to the default", 0, DefaultUncacheablePercentLimit},
		{"negative falls back", -5, DefaultUncacheablePercentLimit},
		{"over 100 falls back", 150, DefaultUncacheablePercentLimit},
		{"a real setting is honoured", 40, 40},
		{"one percent is honoured", 1, 1},
		{"a hundred is honoured", 100, 100},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Site{UncacheablePercentLimit: c.configured}
			if got := s.UncacheableLimit(); got != c.want {
				t.Errorf("UncacheableLimit() with %d = %d, want %d", c.configured, got, c.want)
			}
		})
	}
}

// A page the CDN refuses to store should be reported as its own issue, not left
// looking like a page that merely happened to be cold.
func TestClassifyReportsForbiddenCaching(t *testing.T) {
	state := URLState{
		StatusCode: 200,
		Headers: map[string]string{
			"CF-Cache-Status": "MISS",
			"Cache-Control":   "public, max-age=0",
		},
	}

	var found bool
	for _, issue := range ClassifyURL(state, nil) {
		if issue.Kind == "cache_forbidden" {
			found = true
		}
	}

	if !found {
		t.Error("max-age=0 with a CDN present should raise cache_forbidden")
	}
}

// The standard WordPress/Cloudflare pairing is a correctly cached page and must
// not be reported as forbidden.
func TestClassifyAllowsSharedCacheLifetime(t *testing.T) {
	state := URLState{
		StatusCode: 200,
		Headers: map[string]string{
			"CF-Cache-Status": "HIT",
			"Cache-Control":   "s-maxage=86400, max-age=0",
		},
	}

	for _, issue := range ClassifyURL(state, nil) {
		if issue.Kind == "cache_forbidden" {
			t.Error("s-maxage with max-age=0 is a cached page, not a forbidden one")
		}
	}
}
