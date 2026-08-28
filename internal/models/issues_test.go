package models

import (
	"testing"
	"time"
)

func kinds(issues []SiteIssue) map[string]bool {
	out := map[string]bool{}
	for _, i := range issues {
		out[i.Kind] = true
	}
	return out
}

func TestClassifyAvailability(t *testing.T) {
	cases := map[int]string{500: "server_error", 503: "server_error", 404: "broken", 410: "gone"}
	for status, want := range cases {
		got := kinds(ClassifyURL(URLState{URL: "u", StatusCode: status}, nil))
		if !got[want] {
			t.Errorf("status %d: expected %q, got %v", status, want, got)
		}
	}
}

// A 200 that reads as an error page is the case a status-code-only check
// misses entirely, so it must be caught.
func TestClassifySoftNotFound(t *testing.T) {
	got := kinds(ClassifyURL(URLState{
		URL: "u", StatusCode: 200,
		Page: &PageSignals{Title: "404 - Page Not Found", SoftNotFound: true},
	}, nil))
	if !got["soft_404"] {
		t.Fatalf("expected soft_404, got %v", got)
	}
}

// A healthy page must produce no issues at all — a detector that flags
// everything is useless.
func TestHealthyPageIsClean(t *testing.T) {
	issues := ClassifyURL(URLState{
		URL: "u", StatusCode: 200, ResponseTime: 300,
		Headers: map[string]string{"CF-Cache-Status": "HIT", "Cache-Control": "max-age=3600"},
		Page: &PageSignals{
			Title: "A perfectly reasonable page title", TitleLength: 33,
			MetaDescription: "A description.", MetaDescLength: 14,
			Canonical: "https://example.com/", H1Count: 1, WordCount: 800,
		},
	}, map[string]int{"a perfectly reasonable page title": 1})

	if len(issues) != 0 {
		t.Fatalf("expected no issues, got %+v", issues)
	}
}

func TestClassifyPerformanceAndCaching(t *testing.T) {
	got := kinds(ClassifyURL(URLState{
		URL: "u", StatusCode: 200, ResponseTime: 6000,
		Headers: map[string]string{"CF-Cache-Status": "BYPASS"},
	}, nil))

	if !got["very_slow"] {
		t.Errorf("expected very_slow, got %v", got)
	}
	if !got["cache_bypass"] {
		t.Errorf("expected cache_bypass, got %v", got)
	}
}

func TestDuplicateTitlesNeedTheWholeSet(t *testing.T) {
	state := URLState{
		URL: "u", StatusCode: 200,
		Page: &PageSignals{
			Title: "Shop", TitleLength: 4, MetaDescription: "d",
			Canonical: "c", H1Count: 1, WordCount: 500,
		},
	}
	if kinds(ClassifyURL(state, map[string]int{"shop": 1}))["duplicate_title"] {
		t.Error("a unique title must not be reported as duplicate")
	}
	if !kinds(ClassifyURL(state, map[string]int{"shop": 4}))["duplicate_title"] {
		t.Error("a title used by 4 pages must be reported")
	}
}

// Headers arrive with whatever casing the origin used.
func TestHeaderLookupIsCaseInsensitive(t *testing.T) {
	got := kinds(ClassifyURL(URLState{
		URL: "u", StatusCode: 200,
		Headers: map[string]string{"cf-cache-status": "DYNAMIC"},
	}, nil))
	if !got["cache_dynamic"] {
		t.Fatalf("expected cache_dynamic from lowercase header, got %v", got)
	}
}

func TestIssuesCarryTiming(t *testing.T) {
	first := time.Now().Add(-72 * time.Hour)
	issues := ClassifyURL(URLState{
		URL: "u", StatusCode: 404, FirstSeen: first, Occurrences: 9,
	}, nil)
	if len(issues) == 0 || !issues[0].FirstSeen.Equal(first) || issues[0].Occurrences != 9 {
		t.Fatalf("timing not carried through: %+v", issues)
	}
}
