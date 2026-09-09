package models

import (
	"testing"
	"time"
)

// Every kind the classifier can emit must be categorised deliberately.
//
// IssueCategory defaults to crawler for unmapped kinds, which keeps a new check
// visible rather than hiding it — but a *silent* default is how an SEO check
// ends up in the list a customer is paying attention to. This fails when a kind
// is added without deciding where it belongs.
func TestEveryEmittedKindIsCategorised(t *testing.T) {
	// A URL state chosen to trip as many checks as one row can, so the set of
	// kinds under test is not just the handful an ordinary page produces.
	states := []URLState{
		{URL: "https://e.dk/a", StatusCode: 200, Headers: map[string]string{"CF-Cache-Status": "BYPASS", "Cache-Control": ""},
			ResponseTime: 9000, Page: &PageSignals{NoIndex: true, H1Count: 0, WordCount: 5, ImagesMissingAlt: 3, InsecureRefs: 2}},
		{URL: "https://e.dk/b", StatusCode: 404},
		{URL: "https://e.dk/c", StatusCode: 410},
		{URL: "https://e.dk/d", StatusCode: 500},
		{URL: "https://e.dk/e", StatusCode: 301, RedirectedTo: "https://e.dk/f"},
		{URL: "https://e.dk/g", StatusCode: 0},
		{URL: "https://e.dk/h", StatusCode: 200, Headers: map[string]string{"CF-Cache-Status": "HIT", "Age": "99999", "Cache-Control": "max-age=60"}},
		{URL: "https://e.dk/i", StatusCode: 200, Headers: map[string]string{"Cache-Control": "no-store", "CF-Cache-Status": "DYNAMIC"}},
		{URL: "https://e.dk/j", StatusCode: 200, Page: &PageSignals{Title: "dup", TitleLength: 3, MetaDescription: "", H1Count: 1, WordCount: 900}},
		{URL: "https://e.dk/k", StatusCode: 200, Page: &PageSignals{Title: "dup", TitleLength: 3, H1Count: 1, WordCount: 900}},
		{URL: "https://e.dk/l", StatusCode: 200, Page: &PageSignals{
			Title:       "a title long enough to be flagged as overlong by the sixty character rule",
			TitleLength: 74, H1Count: 1, WordCount: 900,
		}},
	}

	titleCounts := map[string]int{"dup": 2}

	seen := map[string]bool{}
	for _, s := range states {
		s.FirstSeen = time.Now().Add(-time.Hour)
		s.LastSeen = time.Now()
		for _, issue := range ClassifyURL(s, titleCounts) {
			seen[issue.Kind] = true

			if issue.Category != CategoryCrawler && issue.Category != CategorySEO {
				t.Errorf("kind %q has category %q, want %q or %q",
					issue.Kind, issue.Category, CategoryCrawler, CategorySEO)
			}

			if _, mapped := issueCategories[issue.Kind]; !mapped {
				t.Errorf("kind %q is not in issueCategories — decide whether it is a crawler or an SEO issue", issue.Kind)
			}
		}
	}

	if len(seen) < 10 {
		t.Fatalf("only %d kinds exercised (%v); this test is meant to cover most of them", len(seen), seen)
	}
}

// The split is only useful if the two lists mean what they say: a customer
// looking at crawler issues is looking for what stops a page being warmed.
func TestCachingAndAvailabilityAreCrawlerIssues(t *testing.T) {
	for _, kind := range []string{
		"broken", "gone", "server_error", "unreachable", "redirect", "blank_page",
		"cache_bypass", "cache_dynamic", "cache_expired", "cache_forbidden",
		"cache_stale", "no_cache_control", "slow", "very_slow",
	} {
		if got := IssueCategory(kind); got != CategoryCrawler {
			t.Errorf("IssueCategory(%q) = %q, want %q", kind, got, CategoryCrawler)
		}
	}

	for _, kind := range []string{
		"duplicate_title", "images_missing_alt", "long_title", "missing_canonical",
		"missing_meta_description", "missing_title", "mixed_content", "noindex",
		"short_title", "thin_content",
	} {
		if got := IssueCategory(kind); got != CategorySEO {
			t.Errorf("IssueCategory(%q) = %q, want %q", kind, got, CategorySEO)
		}
	}
}
