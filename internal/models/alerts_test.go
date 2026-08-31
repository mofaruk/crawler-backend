package models

import (
	"fmt"
	"strings"
	"testing"
)

// round builds a summary with enough URLs to clear MinURLsForComparison, so a
// test only has to state the thing it is actually about.
func round(id string, cache float64, medianMs int64) *RoundSummary {
	return &RoundSummary{
		CrawlingID:       id,
		URLs:             100,
		CachePercent:     cache,
		MedianResponseMs: medianMs,
		BrokenURLs:       map[string]int{},
		IssueKinds:       map[string]int{},
	}
}

func kindsOf(alerts []AlertEvent) map[string]AlertEvent {
	out := map[string]AlertEvent{}
	for _, a := range alerts {
		out[a.Kind] = a
	}
	return out
}

// A site's first round has nothing to compare against. Announcing everything
// wrong with a site the moment it is added is the burst MaxAlertsPerRound
// exists to prevent, so this is a hard rule rather than a threshold.
func TestNoAlertsOnFirstRound(t *testing.T) {
	cur := round("c1", 20, 900)
	cur.BrokenURLs = map[string]int{"https://x.dk/a": 500}
	cur.IssueKinds = map[string]int{"server_error": 1}

	if got := DetectAlerts(nil, cur); got != nil {
		t.Fatalf("first round produced %d alert(s), want none: %+v", len(got), got)
	}
}

// The cache threshold is the product's core judgement: too low and every
// ordinary fluctuation emails the customer, too high and a real regression is
// missed. Both sides of the boundary are pinned.
func TestCacheRegressionThreshold(t *testing.T) {
	cases := []struct {
		name       string
		prev, cur  float64
		wantAlert  bool
	}{
		{"just under the threshold is silent", 90, 76, false},
		{"exactly at the threshold is silent", 90, 75, false},
		{"just over the threshold alerts", 90, 74, true},
		{"a rise never alerts", 60, 95, false},
		{"unchanged never alerts", 80, 80, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kindsOf(DetectAlerts(round("p", tc.prev, 500), round("c", tc.cur, 500)))
			_, fired := got[AlertCacheRegression]

			if fired != tc.wantAlert {
				t.Fatalf("%.0f%% -> %.0f%% fired=%v, want %v (threshold %.0f)",
					tc.prev, tc.cur, fired, tc.wantAlert, CacheDropThresholdPct)
			}
		})
	}
}

// Halving coverage is not drift, so it escalates. A warning that says the same
// thing for a 16-point drop and a collapse to zero is not much of a warning.
func TestCacheCollapseIsCritical(t *testing.T) {
	drifted := kindsOf(DetectAlerts(round("p", 90, 500), round("c", 70, 500)))
	if got := drifted[AlertCacheRegression].Severity; got != SeverityWarning {
		t.Errorf("90%%→70%% severity = %d, want warning (%d)", got, SeverityWarning)
	}

	collapsed := kindsOf(DetectAlerts(round("p", 90, 500), round("c", 10, 500)))
	if got := collapsed[AlertCacheRegression].Severity; got != SeverityCritical {
		t.Errorf("90%%→10%% severity = %d, want critical (%d)", got, SeverityCritical)
	}
}

func TestSlowdownThreshold(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur int64
		wantAlert bool
	}{
		{"1.4x slower is silent", 1000, 1400, false},
		{"exactly 1.5x is silent", 1000, 1500, false},
		{"1.6x slower alerts", 1000, 1600, true},
		{"getting faster never alerts", 1000, 200, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kindsOf(DetectAlerts(round("p", 80, tc.prev), round("c", 80, tc.cur)))
			_, fired := got[AlertSlower]

			if fired != tc.wantAlert {
				t.Fatalf("%dms -> %dms fired=%v, want %v (factor %.1f)",
					tc.prev, tc.cur, fired, tc.wantAlert, SlowdownFactor)
			}
		})
	}
}

// Percentages over a handful of URLs are noise. A five-URL site where one page
// misses swings coverage by 20 points without anything having gone wrong.
func TestSmallSitesSkipPercentageComparisons(t *testing.T) {
	prev := round("p", 90, 500)
	cur := round("c", 10, 5000)
	prev.URLs = MinURLsForComparison - 1
	cur.URLs = MinURLsForComparison - 1

	got := kindsOf(DetectAlerts(prev, cur))

	if _, fired := got[AlertCacheRegression]; fired {
		t.Error("cache regression fired below MinURLsForComparison")
	}
	if _, fired := got[AlertSlower]; fired {
		t.Error("slowdown fired below MinURLsForComparison")
	}
}

// A site that shrank from 500 URLs to 5 was reconfigured, not degraded.
// Guarding only the current round would report a regression that did not
// happen.
func TestShrunkSiteSkipsPercentageComparisons(t *testing.T) {
	prev := round("p", 90, 500)
	cur := round("c", 10, 500)
	cur.URLs = 5

	if _, fired := kindsOf(DetectAlerts(prev, cur))[AlertCacheRegression]; fired {
		t.Error("cache regression fired when the site shrank below the comparison floor")
	}
}

// Availability is a set difference, not a count: three pages breaking while
// three others recover is two events, not silence.
func TestBrokenAndRecoveredAreSetDifferences(t *testing.T) {
	prev := round("p", 80, 500)
	prev.BrokenURLs = map[string]int{"https://x.dk/old": 500}

	cur := round("c", 80, 500)
	cur.BrokenURLs = map[string]int{"https://x.dk/new": 404}

	got := kindsOf(DetectAlerts(prev, cur))

	broken, ok := got[AlertNewlyBroken]
	if !ok {
		t.Fatal("no newly_broken alert when a different page started failing")
	}
	if broken.Count != 1 || broken.Examples[0] != "https://x.dk/new" {
		t.Errorf("newly_broken = count %d %v, want the new URL only", broken.Count, broken.Examples)
	}

	recovered, ok := got[AlertRecovered]
	if !ok {
		t.Fatal("no recovered alert when the previously failing page came back")
	}
	if !recovered.Resolved {
		t.Error("recovered alert is not marked Resolved")
	}
}

// A page broken in both rounds is state, not change. Reporting it every round
// is exactly how alerting gets filtered into a folder.
func TestStillBrokenDoesNotAlert(t *testing.T) {
	prev := round("p", 80, 500)
	prev.BrokenURLs = map[string]int{"https://x.dk/a": 500}

	cur := round("c", 80, 500)
	cur.BrokenURLs = map[string]int{"https://x.dk/a": 500}

	if got := DetectAlerts(prev, cur); len(got) != 0 {
		t.Fatalf("unchanged breakage produced %d alert(s): %+v", len(got), got)
	}
}

func TestNewIssueKindAlertsButExistingOneDoesNot(t *testing.T) {
	prev := round("p", 80, 500)
	prev.IssueKinds = map[string]int{"slow": 3}

	cur := round("c", 80, 500)
	cur.IssueKinds = map[string]int{"slow": 40, "mixed_content": 2}

	alerts := DetectAlerts(prev, cur)

	var newIssue *AlertEvent
	for i := range alerts {
		if alerts[i].Kind == AlertNewIssues {
			newIssue = &alerts[i]
		}
	}

	if newIssue == nil {
		t.Fatal("a newly appearing issue kind produced no alert")
	}
	if !strings.Contains(newIssue.Title, "insecure") {
		t.Errorf("alert title %q does not describe mixed_content", newIssue.Title)
	}
	if strings.Contains(newIssue.Title, "slow") {
		t.Error("an issue kind present in both rounds was reported as new")
	}
}

// However bad a round is, one site cannot fill an inbox from it — and the cap
// must drop the least important findings, not whichever came last.
func TestAlertsAreCappedWorstFirst(t *testing.T) {
	prev := round("p", 95, 500)
	prev.BrokenURLs = map[string]int{"https://x.dk/recovers": 500}

	cur := round("c", 10, 5000)
	cur.BrokenURLs = map[string]int{}
	for i := 0; i < 3; i++ {
		cur.BrokenURLs[fmt.Sprintf("https://x.dk/broken-%d", i)] = 500
	}
	cur.IssueKinds = map[string]int{
		"mixed_content": 1, "noindex": 2, "thin_content": 3,
		"missing_title": 4, "slow": 5, "duplicate_title": 6,
	}

	alerts := DetectAlerts(prev, cur)

	if len(alerts) > MaxAlertsPerRound {
		t.Fatalf("produced %d alerts, want at most %d", len(alerts), MaxAlertsPerRound)
	}

	// The critical newly-broken alert must survive a cap that drops info-level
	// findings; keeping "3 thin pages" over "3 pages started failing" would
	// make the cap actively harmful.
	if _, ok := kindsOf(alerts)[AlertNewlyBroken]; !ok {
		t.Error("the critical newly_broken alert was dropped by the cap")
	}

	for i := 1; i < len(alerts); i++ {
		if alerts[i-1].Severity < alerts[i].Severity {
			t.Errorf("alerts not ordered worst-first: %d before %d",
				alerts[i-1].Severity, alerts[i].Severity)
		}
	}
}

// The detail line is what the customer reads first, so it has to say what
// actually happened rather than only how many pages.
func TestNewlyBrokenDetailNamesTheStatusCodes(t *testing.T) {
	prev := round("p", 80, 500)

	cur := round("c", 80, 500)
	cur.BrokenURLs = map[string]int{
		"https://x.dk/a": 500,
		"https://x.dk/b": 500,
		"https://x.dk/c": 404,
		"https://x.dk/d": 0,
	}

	detail := kindsOf(DetectAlerts(prev, cur))[AlertNewlyBroken].Detail

	for _, want := range []string{"2 returning HTTP 500", "1 returning HTTP 404", "1 unreachable"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

// An alert covering hundreds of URLs carries a count and a few examples. A
// footer image that broke on four hundred pages is one alert; an email listing
// four hundred URLs is one nobody reads.
func TestExamplesAreCappedButCountIsNot(t *testing.T) {
	prev := round("p", 80, 500)

	cur := round("c", 80, 500)
	cur.BrokenURLs = map[string]int{}
	for i := 0; i < 400; i++ {
		cur.BrokenURLs[fmt.Sprintf("https://x.dk/p-%03d", i)] = 500
	}

	alert := kindsOf(DetectAlerts(prev, cur))[AlertNewlyBroken]

	if alert.Count != 400 {
		t.Errorf("count = %d, want the full 400", alert.Count)
	}
	if len(alert.Examples) > maxAlertExamples {
		t.Errorf("carried %d examples, want at most %d", len(alert.Examples), maxAlertExamples)
	}
}

// A round with nothing to say must produce nothing at all, or every crawl of a
// healthy site becomes an email.
func TestStableSiteProducesNoAlerts(t *testing.T) {
	prev := round("p", 88, 400)
	prev.IssueKinds = map[string]int{"slow": 2}

	cur := round("c", 89, 410)
	cur.IssueKinds = map[string]int{"slow": 2}

	if got := DetectAlerts(prev, cur); len(got) != 0 {
		t.Fatalf("a stable site produced %d alert(s): %+v", len(got), got)
	}
}
