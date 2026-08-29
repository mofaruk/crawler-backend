package models

import (
	"strings"
	"testing"
	"time"
)

// issueByKind indexes the classifier's output so a test can assert on the
// detail text and severity of one specific finding, not just its presence.
func issueByKind(issues []SiteIssue, kind string) (SiteIssue, bool) {
	for _, i := range issues {
		if i.Kind == kind {
			return i, true
		}
	}
	return SiteIssue{}, false
}

func healthyPage() *PageSignals {
	return &PageSignals{
		Title:           "A perfectly reasonable page title",
		TitleLength:     33,
		MetaDescription: "A description.",
		MetaDescLength:  14,
		Canonical:       "https://example.dk/",
		H1Count:         1,
		WordCount:       800,
	}
}

// Availability is the one bucket where the classifier must pick exactly one
// answer: a 503 reported as both "server_error" and "broken" would double the
// critical count on every outage and make the issue list unusable.
func TestAvailabilityIsMutuallyExclusive(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantKind   string // "" means no availability issue at all
		wantSever  int
		wantDetail string
	}{
		{"200 OK is not an availability problem", 200, "", 0, ""},
		{"299 stays out of the 4xx branch", 299, "", 0, ""},
		{"301 is handled by the redirect check, not here", 301, "", 0, ""},
		{"399 is still not an error status", 399, "", 0, ""},
		{"400 is the first broken status", 400, "broken", SeverityCritical, "Returns HTTP 400"},
		{"404 is the canonical broken page", 404, "broken", SeverityCritical, "Returns HTTP 404"},
		{"410 outranks the generic 4xx branch", 410, "gone", SeverityWarning, "Returns HTTP 410"},
		{"429 is broken, not gone", 429, "broken", SeverityCritical, "Returns HTTP 429"},
		{"499 is the last broken status", 499, "broken", SeverityCritical, "Returns HTTP 499"},
		{"500 is the first server error", 500, "server_error", SeverityCritical, "Returns HTTP 500"},
		{"503 is a server error", 503, "server_error", SeverityCritical, "Returns HTTP 503"},
		{"599 is still a server error", 599, "server_error", SeverityCritical, "Returns HTTP 599"},
	}

	availability := map[string]bool{
		"server_error": true, "gone": true, "broken": true, "unreachable": true,
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ClassifyURL(URLState{URL: "u", StatusCode: tc.status}, nil)

			var found []string
			for _, i := range issues {
				if availability[i.Kind] {
					found = append(found, i.Kind)
				}
			}

			if tc.wantKind == "" {
				if len(found) != 0 {
					t.Fatalf("status %d: expected no availability issue, got %v", tc.status, found)
				}
				return
			}
			if len(found) != 1 {
				t.Fatalf("status %d: expected exactly one availability issue, got %v", tc.status, found)
			}
			if found[0] != tc.wantKind {
				t.Fatalf("status %d: got %q, want %q", tc.status, found[0], tc.wantKind)
			}
			got, _ := issueByKind(issues, tc.wantKind)
			if got.Severity != tc.wantSever {
				t.Errorf("severity = %d, want %d", got.Severity, tc.wantSever)
			}
			if got.Detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", got.Detail, tc.wantDetail)
			}
			if got.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", got.StatusCode, tc.status)
			}
		})
	}
}

// BUG DOCUMENTATION: status 0 means "the request never completed", which the
// classifier declares critical via the "unreachable" kind. But that case sits
// last in a switch whose `>= 400` arm already matched nothing and whose
// earlier arms cannot match 0 — so it should fire. This test pins the ACTUAL
// behaviour so a future refactor of the switch order is caught.
func TestUnreachableStatusZero(t *testing.T) {
	issues := ClassifyURL(URLState{URL: "u", StatusCode: 0}, nil)
	got, ok := issueByKind(issues, "unreachable")
	if !ok {
		t.Fatalf("status 0 produced no unreachable issue: %+v", issues)
	}
	if got.Severity != SeverityCritical {
		t.Errorf("unreachable severity = %d, want critical", got.Severity)
	}
}

// A soft 404 is only meaningful on a 200. On a real 404 the status already
// tells the truth, and reporting both would double-count the same page.
func TestSoftNotFoundOnlyAppliesToOK(t *testing.T) {
	cases := []struct {
		name   string
		status int
		soft   bool
		want   bool
	}{
		{"200 with soft-404 signals is reported", 200, true, true},
		{"200 without soft-404 signals is clean", 200, false, false},
		{"a real 404 is not also a soft 404", 404, true, false},
		{"a 500 error page is not a soft 404", 500, true, false},
		{"301 carrying the flag is not reported", 301, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ClassifyURL(URLState{
				URL: "u", StatusCode: tc.status,
				Page: &PageSignals{Title: "404 Not Found", SoftNotFound: tc.soft},
			}, nil)
			_, got := issueByKind(issues, "soft_404")
			if got != tc.want {
				t.Fatalf("soft_404 present = %v, want %v (issues %+v)", got, tc.want, issues)
			}
		})
	}
}

// Nil Page must never panic: assets (images, CSS) are crawled too and carry
// no parsed HTML, so the whole page-quality block has to be skipped.
func TestNilPageSkipsPageQualityChecks(t *testing.T) {
	issues := ClassifyURL(URLState{
		URL: "https://example.dk/logo.png", StatusCode: 200, ResponseTime: 50,
	}, map[string]int{"": 5})

	for _, i := range issues {
		switch i.Kind {
		case "missing_title", "missing_meta_description", "missing_canonical",
			"missing_h1", "soft_404", "duplicate_title":
			t.Fatalf("asset with no HTML produced page-quality issue %q", i.Kind)
		}
	}
}

// Page-quality checks are gated on a 200. A broken page missing its title is
// noise: fix the 500 first, and the title question becomes moot.
func TestPageQualityRequiresStatus200(t *testing.T) {
	blank := &PageSignals{} // everything missing
	for _, status := range []int{301, 404, 410, 500} {
		issues := ClassifyURL(URLState{URL: "u", StatusCode: status, Page: blank}, nil)
		if _, ok := issueByKind(issues, "missing_title"); ok {
			t.Errorf("status %d: page-quality checks must not run on non-200", status)
		}
	}
}

// Title length drives three mutually exclusive issues with hard cut-offs.
// Exactly-at-threshold values are where an off-by-one silently mislabels
// thousands of pages, so each boundary is pinned.
func TestTitleLengthBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		title string
		len   int
		want  string // "" = no title issue
	}{
		{"empty title is missing, not short", "", 0, "missing_title"},
		{"1 char is short", "x", 1, "short_title"},
		{"9 chars is still short", "nine char", 9, "short_title"},
		{"exactly 10 chars is acceptable", "ten charss", 10, ""},
		{"11 chars is acceptable", "eleven char", 11, ""},
		{"exactly 70 chars is acceptable", strings.Repeat("a", 70), 70, ""},
		{"71 chars is too long", strings.Repeat("a", 71), 71, "long_title"},
	}

	titleKinds := map[string]bool{"missing_title": true, "short_title": true, "long_title": true}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := healthyPage()
			p.Title = tc.title
			p.TitleLength = tc.len
			issues := ClassifyURL(URLState{URL: "u", StatusCode: 200, Page: p}, nil)

			var found []string
			for _, i := range issues {
				if titleKinds[i.Kind] {
					found = append(found, i.Kind)
				}
			}
			if tc.want == "" {
				if len(found) != 0 {
					t.Fatalf("title %q (len %d): expected no title issue, got %v", tc.title, tc.len, found)
				}
				return
			}
			if len(found) != 1 || found[0] != tc.want {
				t.Fatalf("title len %d: got %v, want [%s]", tc.len, found, tc.want)
			}
		})
	}
}

// TitleLength is what the thresholds compare, not len(Title). If the two ever
// disagree (a rune-counting bug upstream) the classifier follows the field —
// worth pinning so the coupling is explicit.
func TestTitleThresholdsUseTitleLengthField(t *testing.T) {
	p := healthyPage()
	p.Title = "short"  // 5 characters
	p.TitleLength = 40 // but the stored length says otherwise
	issues := ClassifyURL(URLState{URL: "u", StatusCode: 200, Page: p}, nil)
	if _, ok := issueByKind(issues, "short_title"); ok {
		t.Fatal("classifier must trust TitleLength, not len(Title)")
	}
}

// Duplicate detection is case-insensitive because "Shop" and "SHOP" are the
// same title to a search engine, and CMSs are inconsistent about casing.
func TestDuplicateTitleLookupIsCaseInsensitive(t *testing.T) {
	cases := []struct {
		name   string
		title  string
		counts map[string]int
		want   bool
	}{
		{"unique title is not duplicate", "Shop", map[string]int{"shop": 1}, false},
		{"count of 2 is duplicate", "Shop", map[string]int{"shop": 2}, true},
		{"uppercase title matches lowercased key", "SHOP", map[string]int{"shop": 3}, true},
		{"mixed case title matches", "ShOp", map[string]int{"shop": 3}, true},
		{"missing key means unique", "Shop", map[string]int{"other": 9}, false},
		{"nil map means unique", "Shop", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := healthyPage()
			p.Title = tc.title
			p.TitleLength = len(tc.title) + 20 // keep out of the short/long branches
			issues := ClassifyURL(URLState{URL: "u", StatusCode: 200, Page: p}, tc.counts)
			_, got := issueByKind(issues, "duplicate_title")
			if got != tc.want {
				t.Fatalf("duplicate_title = %v, want %v", got, tc.want)
			}
		})
	}
}

// An empty title must not be counted as a duplicate of every other untitled
// page — that would bury the actionable missing_title finding under noise.
func TestEmptyTitleIsNeverDuplicate(t *testing.T) {
	p := healthyPage()
	p.Title = ""
	p.TitleLength = 0
	issues := ClassifyURL(URLState{URL: "u", StatusCode: 200, Page: p}, map[string]int{"": 400})
	if _, ok := issueByKind(issues, "duplicate_title"); ok {
		t.Fatal("empty titles must not be reported as duplicates of each other")
	}
	if _, ok := issueByKind(issues, "missing_title"); !ok {
		t.Fatal("empty title should still be reported as missing")
	}
}

// Response-time thresholds separate "annoying" from "abandoned". Both are
// >= comparisons, so the exact millisecond matters.
func TestResponseTimeBoundaries(t *testing.T) {
	table := []struct {
		name string
		ms   int64
		want string
	}{
		{"0ms is not slow", 0, ""},
		{"1999ms is just under the slow threshold", 1999, ""},
		{"exactly 2000ms is slow", 2000, "slow"},
		{"4999ms is slow but not very slow", 4999, "slow"},
		{"exactly 5000ms is very slow", 5000, "very_slow"},
		{"12000ms is very slow", 12000, "very_slow"},
	}

	perfKinds := map[string]bool{"slow": true, "very_slow": true}

	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			issues := ClassifyURL(URLState{URL: "u", StatusCode: 200, ResponseTime: tc.ms}, nil)
			var found []string
			for _, i := range issues {
				if perfKinds[i.Kind] {
					found = append(found, i.Kind)
				}
			}
			if tc.want == "" {
				if len(found) != 0 {
					t.Fatalf("%dms: expected no perf issue, got %v", tc.ms, found)
				}
				return
			}
			if len(found) != 1 || found[0] != tc.want {
				t.Fatalf("%dms: got %v, want [%s]", tc.ms, found, tc.want)
			}
		})
	}
}

// The "Took N.Ns" detail is what the customer reads. A millisecond value
// rendered as raw milliseconds would be meaningless in the UI.
func TestSlowIssueDetailIsFormattedInSeconds(t *testing.T) {
	issues := ClassifyURL(URLState{URL: "u", StatusCode: 200, ResponseTime: 3450}, nil)
	got, ok := issueByKind(issues, "slow")
	if !ok {
		t.Fatal("expected a slow issue")
	}
	if got.Detail != "Took 3.5s" {
		t.Fatalf("detail = %q, want %q", got.Detail, "Took 3.5s")
	}
}

// Cache status is the product's core subject. Only three of Cloudflare's
// values are problems; HIT/MISS/REVALIDATED are normal operation and must not
// be reported, or every crawl would show thousands of false issues.
func TestCacheStatusClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		want      string // "" = no cache issue
		wantSever int
	}{
		{"HIT is the goal, not an issue", "HIT", "", 0},
		{"MISS is normal on first request", "MISS", "", 0},
		{"REVALIDATED is normal", "REVALIDATED", "", 0},
		{"UPDATING is normal", "UPDATING", "", 0},
		{"BYPASS means caching never happens", "BYPASS", "cache_bypass", SeverityWarning},
		{"DYNAMIC means the CDN refuses to cache", "DYNAMIC", "cache_dynamic", SeverityWarning},
		{"EXPIRED is only informational", "EXPIRED", "cache_expired", SeverityInfo},
		{"lowercase bypass is normalised", "bypass", "cache_bypass", SeverityWarning},
		{"mixed-case Dynamic is normalised", "Dynamic", "cache_dynamic", SeverityWarning},
		{"unknown value is not guessed at", "WEIRD", "", 0},
	}

	cacheKinds := map[string]bool{"cache_bypass": true, "cache_dynamic": true, "cache_expired": true}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ClassifyURL(URLState{
				URL: "u", StatusCode: 200,
				Headers: map[string]string{"CF-Cache-Status": tc.status, "Cache-Control": "max-age=60"},
			}, nil)
			var found []SiteIssue
			for _, i := range issues {
				if cacheKinds[i.Kind] {
					found = append(found, i)
				}
			}
			if tc.want == "" {
				if len(found) != 0 {
					t.Fatalf("%q: expected no cache issue, got %+v", tc.status, found)
				}
				return
			}
			if len(found) != 1 || found[0].Kind != tc.want {
				t.Fatalf("%q: got %+v, want [%s]", tc.status, found, tc.want)
			}
			if found[0].Severity != tc.wantSever {
				t.Errorf("severity = %d, want %d", found[0].Severity, tc.wantSever)
			}
		})
	}
}

// no_cache_control is deliberately narrow: it only fires behind a CDN (a
// cf-cache-status header is present) and on a 200. Reporting it for every
// asset off any origin would drown the real finding.
func TestNoCacheControlPreconditions(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		want    bool
	}{
		{
			"CDN present and no Cache-Control is the real finding",
			200, map[string]string{"CF-Cache-Status": "HIT"}, true,
		},
		{
			"Cache-Control present means the origin has a policy",
			200, map[string]string{"CF-Cache-Status": "HIT", "Cache-Control": "max-age=60"}, false,
		},
		{
			"no CDN header means we cannot say the CDN is guessing",
			200, map[string]string{}, false,
		},
		{
			"nil headers must not panic and must not report",
			200, nil, false,
		},
		{
			"non-200 responses are excluded",
			404, map[string]string{"CF-Cache-Status": "HIT"}, false,
		},
		{
			"lowercase cache-control still counts as a policy",
			200, map[string]string{"cf-cache-status": "HIT", "cache-control": "no-store"}, false,
		},
		{
			"empty Cache-Control value counts as absent",
			200, map[string]string{"CF-Cache-Status": "HIT", "Cache-Control": ""}, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ClassifyURL(URLState{URL: "u", StatusCode: tc.status, Headers: tc.headers}, nil)
			_, got := issueByKind(issues, "no_cache_control")
			if got != tc.want {
				t.Fatalf("no_cache_control = %v, want %v (issues %+v)", got, tc.want, issues)
			}
		})
	}
}

// Redirects are reported at info level regardless of status code, because the
// crawler follows them: the URL in the customer's sitemap is not the URL
// served, which is worth knowing but not broken.
func TestRedirectIsReportedWheneverTargetDiffers(t *testing.T) {
	cases := []struct {
		name         string
		redirectedTo string
		want         bool
	}{
		{"no redirect recorded", "", false},
		{"redirect target recorded", "https://example.dk/new", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ClassifyURL(URLState{
				URL: "https://example.dk/old", StatusCode: 200, RedirectedTo: tc.redirectedTo,
			}, nil)
			got, ok := issueByKind(issues, "redirect")
			if ok != tc.want {
				t.Fatalf("redirect = %v, want %v", ok, tc.want)
			}
			if ok {
				if got.Severity != SeverityInfo {
					t.Errorf("severity = %d, want info", got.Severity)
				}
				if got.Detail != "Now serves "+tc.redirectedTo {
					t.Errorf("detail = %q", got.Detail)
				}
			}
		})
	}
}

// H1 count has two failure modes on either side of exactly one.
func TestH1CountBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		count int
		want  string
	}{
		{"zero h1 is a missing main heading", 0, "missing_h1"},
		{"exactly one h1 is correct", 1, ""},
		{"two h1 elements is a structure problem", 2, "multiple_h1"},
		{"nine h1 elements is still multiple_h1", 9, "multiple_h1"},
	}
	h1Kinds := map[string]bool{"missing_h1": true, "multiple_h1": true}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := healthyPage()
			p.H1Count = tc.count
			issues := ClassifyURL(URLState{URL: "u", StatusCode: 200, Page: p}, nil)
			var found []string
			for _, i := range issues {
				if h1Kinds[i.Kind] {
					found = append(found, i.Kind)
				}
			}
			if tc.want == "" {
				if len(found) != 0 {
					t.Fatalf("h1=%d: expected none, got %v", tc.count, found)
				}
				return
			}
			if len(found) != 1 || found[0] != tc.want {
				t.Fatalf("h1=%d: got %v, want [%s]", tc.count, found, tc.want)
			}
		})
	}
}

// A word count of 0 means the parser found no body text at all — which for a
// non-HTML or JS-rendered page is not evidence of thin content. Only a page
// with *some* words but too few is flagged.
func TestThinContentBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		words int
		want  bool
	}{
		{"zero words means unparsed, not thin", 0, false},
		{"1 word is thin", 1, true},
		{"99 words is thin", 99, true},
		{"exactly 100 words is acceptable", 100, false},
		{"101 words is acceptable", 101, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := healthyPage()
			p.WordCount = tc.words
			issues := ClassifyURL(URLState{URL: "u", StatusCode: 200, Page: p}, nil)
			_, got := issueByKind(issues, "thin_content")
			if got != tc.want {
				t.Fatalf("words=%d: thin_content = %v, want %v", tc.words, got, tc.want)
			}
		})
	}
}

// Mixed content is critical because browsers block the resources outright:
// the visitor sees a visibly broken page, not a subtle SEO penalty.
func TestMixedContentAndAltTextCounts(t *testing.T) {
	cases := []struct {
		name          string
		insecure      int
		missingAlt    int
		wantMixed     bool
		wantAlt       bool
		wantMixedText string
		wantAltText   string
	}{
		{"clean page reports neither", 0, 0, false, false, "", ""},
		{"one insecure ref is critical", 1, 0, true, false,
			"1 http:// resources on an https page; browsers block these", ""},
		{"images without alt are info-level", 0, 12, false, true, "", "12 images"},
		{"both can be reported for one URL", 3, 4, true, true,
			"3 http:// resources on an https page; browsers block these", "4 images"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := healthyPage()
			p.InsecureRefs = tc.insecure
			p.ImagesMissingAlt = tc.missingAlt
			issues := ClassifyURL(URLState{URL: "u", StatusCode: 200, Page: p}, nil)

			mixed, gotMixed := issueByKind(issues, "mixed_content")
			if gotMixed != tc.wantMixed {
				t.Fatalf("mixed_content = %v, want %v", gotMixed, tc.wantMixed)
			}
			if gotMixed {
				if mixed.Severity != SeverityCritical {
					t.Errorf("mixed_content severity = %d, want critical", mixed.Severity)
				}
				if mixed.Detail != tc.wantMixedText {
					t.Errorf("detail = %q, want %q", mixed.Detail, tc.wantMixedText)
				}
			}

			alt, gotAlt := issueByKind(issues, "images_missing_alt")
			if gotAlt != tc.wantAlt {
				t.Fatalf("images_missing_alt = %v, want %v", gotAlt, tc.wantAlt)
			}
			if gotAlt {
				if alt.Severity != SeverityInfo {
					t.Errorf("alt severity = %d, want info", alt.Severity)
				}
				if alt.Detail != tc.wantAltText {
					t.Errorf("detail = %q, want %q", alt.Detail, tc.wantAltText)
				}
			}
		})
	}
}

// noindex is critical: the page is invisible in search, which for a commercial
// site is indistinguishable from the page not existing.
func TestNoindexAndCanonicalAndMetaDescription(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*PageSignals)
		wantKind string
		wantSev  int
	}{
		{"noindex hides the page from search", func(p *PageSignals) { p.NoIndex = true }, "noindex", SeverityCritical},
		{"missing canonical risks duplicate competition", func(p *PageSignals) { p.Canonical = "" }, "missing_canonical", SeverityInfo},
		{"missing meta description lets Google invent one", func(p *PageSignals) { p.MetaDescription = "" }, "missing_meta_description", SeverityInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := healthyPage()
			tc.mutate(p)
			issues := ClassifyURL(URLState{URL: "u", StatusCode: 200, Page: p}, nil)
			got, ok := issueByKind(issues, tc.wantKind)
			if !ok {
				t.Fatalf("expected %q, got %+v", tc.wantKind, issues)
			}
			if got.Severity != tc.wantSev {
				t.Errorf("severity = %d, want %d", got.Severity, tc.wantSev)
			}
		})
	}
}

// One URL routinely has several problems at once. The classifier must return
// them all rather than stopping at the first — the UI groups by URL and would
// otherwise show a page as "only slow" when it is also uncached and noindexed.
func TestOneURLCanProduceManyIssues(t *testing.T) {
	first := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)

	issues := ClassifyURL(URLState{
		URL: "https://example.dk/bad", StatusCode: 200, ResponseTime: 7000,
		RedirectedTo: "https://example.dk/other",
		Headers:      map[string]string{"CF-Cache-Status": "BYPASS"},
		FirstSeen:    first, LastSeen: last, Occurrences: 3,
		Page: &PageSignals{
			Title: "", TitleLength: 0, MetaDescription: "", Canonical: "",
			NoIndex: true, H1Count: 0, WordCount: 12,
			InsecureRefs: 2, ImagesMissingAlt: 5,
		},
	}, nil)

	want := []string{
		"redirect", "very_slow", "cache_bypass", "no_cache_control",
		"missing_title", "missing_meta_description", "noindex",
		"missing_canonical", "missing_h1", "thin_content",
		"mixed_content", "images_missing_alt",
	}
	got := map[string]bool{}
	for _, i := range issues {
		got[i.Kind] = true
		// Every issue must carry the URL and timing, since the UI renders each
		// one standalone.
		if i.URL != "https://example.dk/bad" {
			t.Errorf("issue %q lost the URL", i.Kind)
		}
		if !i.FirstSeen.Equal(first) || !i.LastSeen.Equal(last) || i.Occurrences != 3 {
			t.Errorf("issue %q lost its timing metadata: %+v", i.Kind, i)
		}
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("expected issue %q, got %v", k, got)
		}
	}
	if len(issues) != len(want) {
		t.Errorf("got %d issues (%v), want exactly %d", len(issues), got, len(want))
	}
}

// A completely empty state (never crawled, no data) is the degenerate input
// the aggregation can produce. It must not panic and must not invent issues
// beyond the unreachable finding.
func TestZeroValueStateIsSafe(t *testing.T) {
	issues := ClassifyURL(URLState{}, nil)
	for _, i := range issues {
		if i.Kind != "unreachable" {
			t.Errorf("zero state produced unexpected issue %q", i.Kind)
		}
	}
}

// Severity constants must stay ordered, since the UI sorts on them and the
// dashboard's "critical only" filter is a numeric comparison.
func TestSeverityOrdering(t *testing.T) {
	if !(SeverityInfo < SeverityWarning && SeverityWarning < SeverityCritical) {
		t.Fatalf("severity order broken: info=%d warning=%d critical=%d",
			SeverityInfo, SeverityWarning, SeverityCritical)
	}
}

// headerLookup is exercised through the classifier elsewhere; here it is
// pinned directly because header casing varies per origin and an exact-match
// fallback would silently disable every cache check.
func TestHeaderLookup(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		lookup  string
		want    string
	}{
		{"nil map returns empty", nil, "CF-Cache-Status", ""},
		{"exact match wins", map[string]string{"CF-Cache-Status": "HIT"}, "CF-Cache-Status", "HIT"},
		{"lowercase key found", map[string]string{"cf-cache-status": "MISS"}, "CF-Cache-Status", "MISS"},
		{"uppercase key found", map[string]string{"CF-CACHE-STATUS": "HIT"}, "cf-cache-status", "HIT"},
		{"absent header returns empty", map[string]string{"Age": "5"}, "CF-Cache-Status", ""},
		{"empty value returned as empty", map[string]string{"Age": ""}, "Age", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := headerLookup(tc.headers, tc.lookup); got != tc.want {
				t.Fatalf("headerLookup = %q, want %q", got, tc.want)
			}
		})
	}
}
