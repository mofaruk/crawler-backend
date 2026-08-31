package models

import (
	"fmt"
	"sort"
	"strings"
)

// RoundSummary reduces one completed crawl round to what alerting compares.
//
// Detection is a pure function of two of these, so the thresholds can be
// tested without a database: what counts as a regression is a product
// decision, and it is the part worth pinning down.
type RoundSummary struct {
	CrawlingID string

	// URLs is how many results the round produced, and the guard for every
	// percentage comparison below.
	URLs int

	// CachePercent is the share of cache-known URLs served from cache, 0-100.
	CachePercent float64

	// MedianResponseMs is median rather than mean: one pathological page
	// should not move a whole site's number.
	MedianResponseMs int64

	// BrokenURLs is every URL that did not return a usable response, so
	// newly-broken and recovered are set differences rather than counts —
	// "3 broke and 3 recovered" must not read as no change.
	BrokenURLs map[string]int

	// IssueKinds is the set of issue kinds present this round, for reporting
	// a kind that has newly appeared.
	IssueKinds map[string]int
}

// DetectAlerts compares a finished round against the one before it.
//
// Alerts fire on *change*, never on state: "3 pages started failing" is worth
// an interruption, "12 pages are still failing" is not, and sending the latter
// every round is how a monitoring product gets filtered into a folder.
//
// Returns nil when prev is nil. A site's first round has nothing to compare
// against, and announcing everything wrong with a site the moment it is added
// is precisely the burst MaxAlertsPerRound exists to prevent.
func DetectAlerts(prev, cur *RoundSummary) []AlertEvent {
	if prev == nil || cur == nil {
		return nil
	}

	var out []AlertEvent

	add := func(kind, title, detail string, severity, count int, examples []string) {
		out = append(out, AlertEvent{
			Kind:     kind,
			Title:    title,
			Detail:   detail,
			Severity: severity,
			Count:    count,
			Examples: examples,
			Resolved: kind == AlertRecovered,
		})
	}

	// --- Availability: set differences, not counts ---

	newlyBroken := keysMissingFrom(cur.BrokenURLs, prev.BrokenURLs)
	if n := len(newlyBroken); n > 0 {
		add(AlertNewlyBroken, plural(n, "page", "pages")+" started failing",
			describeURLs(newlyBroken, cur.BrokenURLs),
			SeverityCritical, n, examplesOf(newlyBroken))
	}

	recovered := keysMissingFrom(prev.BrokenURLs, cur.BrokenURLs)
	if n := len(recovered); n > 0 {
		add(AlertRecovered, plural(n, "page", "pages")+" recovered",
			"Previously failing, now responding normally",
			SeverityInfo, n, examplesOf(recovered))
	}

	// --- Percentage comparisons ---
	//
	// Guarded by URL count on *both* rounds: a site that shrank from 500 URLs
	// to 5 has not got worse, it has been reconfigured, and comparing the two
	// percentages would report a regression that did not happen.
	comparable := prev.URLs >= MinURLsForComparison && cur.URLs >= MinURLsForComparison

	if comparable {
		if drop := prev.CachePercent - cur.CachePercent; drop > CacheDropThresholdPct {
			// Critical once cache coverage has more than halved: that is no
			// longer drift, it is the CDN not doing its job.
			severity := SeverityWarning
			if cur.CachePercent < prev.CachePercent/2 {
				severity = SeverityCritical
			}

			add(AlertCacheRegression, "Cache coverage dropped",
				fmt.Sprintf("Now %.1f%% served from cache, down from %.1f%%",
					cur.CachePercent, prev.CachePercent),
				severity, cur.URLs, nil)
		}

		if prev.MedianResponseMs > 0 &&
			float64(cur.MedianResponseMs) > float64(prev.MedianResponseMs)*SlowdownFactor {
			add(AlertSlower, "Site got slower",
				fmt.Sprintf("Median response %dms, up from %dms",
					cur.MedianResponseMs, prev.MedianResponseMs),
				SeverityWarning, cur.URLs, nil)
		}
	}

	// --- Newly appearing issue kinds ---

	for _, kind := range sortedNewKinds(prev.IssueKinds, cur.IssueKinds) {
		n := cur.IssueKinds[kind]
		add(AlertNewIssues, "New problem found: "+humanKind(kind),
			fmt.Sprintf("Affects %s", plural(n, "page", "pages")),
			SeverityWarning, n, nil)
	}

	return capAlerts(out)
}

// capAlerts keeps only the worst MaxAlertsPerRound alerts.
//
// However bad a round is, one site cannot fill an inbox from it. Sorting by
// severity first means the cap drops the least important findings rather than
// whichever happened to be detected last.
func capAlerts(alerts []AlertEvent) []AlertEvent {
	if len(alerts) <= MaxAlertsPerRound {
		return alerts
	}

	sort.SliceStable(alerts, func(i, j int) bool {
		if alerts[i].Severity != alerts[j].Severity {
			return alerts[i].Severity > alerts[j].Severity
		}
		return alerts[i].Count > alerts[j].Count
	})

	return alerts[:MaxAlertsPerRound]
}

// keysMissingFrom returns the keys of a that b does not have, sorted so the
// output is stable between runs.
func keysMissingFrom(a, b map[string]int) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// sortedNewKinds returns issue kinds present in cur but absent from prev.
func sortedNewKinds(prev, cur map[string]int) []string {
	var out []string
	for k, n := range cur {
		if n == 0 {
			continue
		}
		if _, existed := prev[k]; !existed {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// maxAlertExamples is how many URLs an alert carries.
//
// A footer image that broke on four hundred pages is one alert with a count
// and a few examples, not four hundred alerts — and an email listing four
// hundred URLs is one nobody reads.
const maxAlertExamples = 5

func examplesOf(urls []string) []string {
	if len(urls) <= maxAlertExamples {
		return urls
	}
	return urls[:maxAlertExamples]
}

// describeURLs summarises what the failures were, so the alert says "returns
// HTTP 500" rather than only how many pages are affected.
func describeURLs(urls []string, statuses map[string]int) string {
	counts := map[int]int{}
	for _, u := range urls {
		counts[statuses[u]]++
	}

	codes := make([]int, 0, len(counts))
	for code := range counts {
		codes = append(codes, code)
	}
	sort.Ints(codes)

	parts := make([]string, 0, len(codes))
	for _, code := range codes {
		if code == 0 {
			parts = append(parts, fmt.Sprintf("%d unreachable", counts[code]))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d returning HTTP %d", counts[code], code))
	}

	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// humanKind turns an issue kind into the words used in the alert. Falls back
// to the raw kind so a newly added issue type still reads sensibly here
// without having to be registered in two places.
func humanKind(kind string) string {
	switch kind {
	case "server_error":
		return "server errors"
	case "broken":
		return "broken pages"
	case "soft_404":
		return "pages that look like errors but return OK"
	case "cache_bypass":
		return "pages the CDN never caches"
	case "cache_dynamic":
		return "pages the CDN treats as uncacheable"
	case "cache_stale":
		return "pages serving stale content"
	case "no_cache_control":
		return "pages with no caching policy"
	case "very_slow":
		return "very slow pages"
	case "slow":
		return "slow pages"
	case "mixed_content":
		return "pages loading insecure resources"
	case "noindex":
		return "pages hidden from search engines"
	case "missing_title":
		return "pages with no title"
	case "duplicate_title":
		return "duplicate page titles"
	default:
		return strings.ReplaceAll(kind, "_", " ")
	}
}
