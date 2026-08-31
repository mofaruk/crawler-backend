package models

import (
	"testing"
	"time"
)

func classify(page *PageSignals, status int) map[string]SiteIssue {
	state := URLState{
		URL:         "https://example.dk/",
		StatusCode:  status,
		ContentType: "text/html; charset=UTF-8",
		Headers:     map[string]string{"Content-Type": "text/html; charset=UTF-8"},
		Page:        page,
		FirstSeen:   time.Now(),
		LastSeen:    time.Now(),
		Occurrences: 1,
	}

	out := map[string]SiteIssue{}
	for _, issue := range ClassifyURL(state, map[string]int{}) {
		out[issue.Kind] = issue
	}

	return out
}

// A page-cache plugin that caches an empty response and marks it as never
// expiring serves a blank page to every visitor and every search engine, while
// returning 200 the whole time. Taken from a real customer site whose homepage
// was 228 bytes of HTML comment and nothing else.
//
// This is the case every status-code monitor misses, so it must not be the one
// this classifier misses too.
func TestBlankPageIsReportedAsCritical(t *testing.T) {
	issues := classify(&PageSignals{}, 200)

	blank, found := issues["blank_page"]
	if !found {
		t.Fatal("a 200 with no title, no heading and no words produced no blank_page issue")
	}

	if blank.Severity != SeverityCritical {
		t.Errorf("blank page severity = %d, want critical (%d)", blank.Severity, SeverityCritical)
	}
}

// thin_content requires WordCount > 0 so it does not fire on assets, which
// have no words either. That guard means it cannot describe an empty page, so
// the two must not both fire and confuse the report.
func TestBlankPageIsNotAlsoThinContent(t *testing.T) {
	issues := classify(&PageSignals{}, 200)

	if _, found := issues["thin_content"]; found {
		t.Error("an empty page was reported as thin content as well as blank")
	}
}

func TestThinContentStillFiresForAShortPage(t *testing.T) {
	issues := classify(&PageSignals{
		Title: "A real page", TitleLength: 11, H1Count: 1, WordCount: 40,
	}, 200)

	if _, found := issues["thin_content"]; !found {
		t.Error("a 40-word page produced no thin_content issue")
	}
	if _, found := issues["blank_page"]; found {
		t.Error("a page with real content was reported as blank")
	}
}

// A page with a heading but no body text is a broken template, not a blank
// page. Reporting it as blank would misdescribe what the owner has to fix.
func TestPageWithHeadingIsNotBlank(t *testing.T) {
	issues := classify(&PageSignals{
		Title: "Contact", TitleLength: 7, H1Count: 1, WordCount: 0,
	}, 200)

	if _, found := issues["blank_page"]; found {
		t.Error("a page with a title and heading was reported as blank")
	}
}

// Only a 200 matters here. A 404 that is empty is a 404 behaving correctly,
// and it is already reported as broken.
func TestEmptyErrorPageIsNotReportedAsBlank(t *testing.T) {
	issues := classify(&PageSignals{}, 404)

	if _, found := issues["blank_page"]; found {
		t.Error("an empty 404 was reported as a blank page rather than as broken")
	}
	if _, found := issues["broken"]; !found {
		t.Error("an empty 404 was not reported as broken")
	}
}
