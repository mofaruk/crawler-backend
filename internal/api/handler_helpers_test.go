package api

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/webkonsulenterne/crawler-backend/internal/models"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ctxWithQuery builds the minimum gin.Context the query-string helpers need,
// without a router or a database behind it.
func ctxWithQuery(t *testing.T, rawQuery string) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?"+rawQuery, nil)
	return c
}

// allowsURL enforces the per-crawl url_type scope at ingestion time. Getting
// it wrong means a "dynamic pages only" crawl silently burns the customer's
// URL quota on images, or a "static assets only" crawl returns nothing.
func TestAllowsURL(t *testing.T) {
	cases := []struct {
		name  string
		scope string
		url   string
		want  bool
	}{
		// An unset scope is the default and must let everything through, or
		// existing crawls would start dropping URLs after an upgrade.
		{"empty scope allows a page", "", "https://example.dk/page", true},
		{"empty scope allows an asset", "", "https://example.dk/a.css", true},
		{"all allows a page", models.CrawlURLTypeAll, "https://example.dk/page", true},
		{"all allows an asset", models.CrawlURLTypeAll, "https://example.dk/a.png", true},
		{"an unrecognised scope fails open rather than dropping everything", "bogus", "https://example.dk/a.png", true},

		{"static accepts a css file", models.CrawlURLTypeStatic, "https://example.dk/a.css", true},
		{"static accepts a jpeg", models.CrawlURLTypeStatic, "https://example.dk/img/a.jpg", true},
		{"static accepts a webfont", models.CrawlURLTypeStatic, "https://example.dk/f.woff2", true},
		{"static accepts a pdf", models.CrawlURLTypeStatic, "https://example.dk/doc.pdf", true},
		{"static rejects a pretty URL", models.CrawlURLTypeStatic, "https://example.dk/produkter/filter", false},
		{"static rejects an html page", models.CrawlURLTypeStatic, "https://example.dk/index.html", false},
		{"static rejects a php page", models.CrawlURLTypeStatic, "https://example.dk/index.php", false},

		{"dynamic accepts a pretty URL", models.CrawlURLTypeDynamic, "https://example.dk/produkter/filter", true},
		{"dynamic accepts an html page", models.CrawlURLTypeDynamic, "https://example.dk/index.html", true},
		{"dynamic rejects a css file", models.CrawlURLTypeDynamic, "https://example.dk/a.css", false},
		{"dynamic rejects an image", models.CrawlURLTypeDynamic, "https://example.dk/a.png", false},

		// Extension classification runs on the path only, so a query string
		// must not turn an asset into a page or the reverse.
		{"static sees through a query string", models.CrawlURLTypeStatic, "https://example.dk/a.css?v=3", true},
		{"dynamic sees through a query string", models.CrawlURLTypeDynamic, "https://example.dk/a.css?v=3", false},
		{"a trailing slash does not create an extension", models.CrawlURLTypeDynamic, "https://example.dk/page/", true},
		{"a dot in a directory name is not an extension", models.CrawlURLTypeDynamic, "https://example.dk/v1.2/page", true},
		{"uppercase extension is recognised", models.CrawlURLTypeStatic, "https://example.dk/A.PNG", true},
		{"an unknown extension counts as a page", models.CrawlURLTypeDynamic, "https://example.dk/a.aspx", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allowsURL(tc.scope, tc.url); got != tc.want {
				t.Fatalf("allowsURL(%q, %q) = %v, want %v", tc.scope, tc.url, got, tc.want)
			}
		})
	}
}

// A URL the parser rejects is classified as non-static, so it survives a
// "dynamic" crawl and is dropped from a "static" one. Documented here because
// the asymmetry is deliberate but surprising.
func TestAllowsURLOnUnparseableInput(t *testing.T) {
	bad := "http://exa\x7fmple.dk/x"
	if allowsURL(models.CrawlURLTypeStatic, bad) {
		t.Error("an unparseable URL must not be treated as a static asset")
	}
	if !allowsURL(models.CrawlURLTypeDynamic, bad) {
		t.Error("an unparseable URL is reported as non-static, so dynamic accepts it")
	}
}

// parsePagination guards the database: an unbounded limit lets one request
// pull an entire crawl's results into memory.
func TestParsePagination(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantSkip  int64
		wantLimit int64
	}{
		{"no parameters uses the defaults", "", 0, 20},
		{"explicit values pass through", "skip=40&limit=50", 40, 50},
		{"limit of 100 is the maximum allowed", "limit=100", 0, 100},
		{"limit above the cap falls back to the default", "limit=101", 0, 20},
		{"a huge limit falls back to the default", "limit=100000", 0, 20},
		{"zero limit falls back to the default", "limit=0", 0, 20},
		{"negative limit falls back to the default", "limit=-5", 0, 20},
		{"negative skip is clamped to zero", "skip=-10", 0, 20},
		{"non-numeric limit falls back to the default", "limit=abc", 0, 20},
		{"non-numeric skip is treated as zero", "skip=abc", 0, 20},
		{"skip is not capped", "skip=99999&limit=10", 99999, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip, limit := parsePagination(ctxWithQuery(t, tc.query))
			if skip != tc.wantSkip || limit != tc.wantLimit {
				t.Fatalf("parsePagination(%q) = (%d, %d), want (%d, %d)",
					tc.query, skip, limit, tc.wantSkip, tc.wantLimit)
			}
		})
	}
}

// parseDay accepts two formats because the UI sends a plain date from a date
// picker and a full timestamp from a chart brush.
func TestParseDay(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
		want    time.Time
	}{
		{"plain calendar date", "2026-08-29", false, time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)},
		{"RFC3339 timestamp", "2026-08-29T13:45:00Z", false, time.Date(2026, 8, 29, 13, 45, 0, 0, time.UTC)},
		{"empty string", "", true, time.Time{}},
		{"American format is not accepted", "08/29/2026", true, time.Time{}},
		{"a date with a time but no zone", "2026-08-29 13:45", true, time.Time{}},
		{"nonsense", "yesterday", true, time.Time{}},
		{"impossible date", "2026-13-45", true, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDay(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseDay(%q) = %v, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDay(%q) returned %v", tc.raw, err)
			}
			if !got.UTC().Equal(tc.want) {
				t.Fatalf("parseDay(%q) = %v, want %v", tc.raw, got.UTC(), tc.want)
			}
		})
	}
}

// An explicit from/to window must win over days, and the end date must be
// inclusive — a customer asking for from=to=today expects today's data, not
// an empty result.
func TestResolveWindowExplicitDates(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantSince time.Time
		wantUntil time.Time
		wantDays  int
	}{
		{
			name:      "from and to cover whole days inclusively",
			query:     "from=2026-08-01&to=2026-08-07",
			wantSince: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC),
			wantDays:  7,
		},
		{
			name:      "a single day window returns that whole day",
			query:     "from=2026-08-29&to=2026-08-29",
			wantSince: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
			wantDays:  1,
		},
		{
			name:      "reversed dates are swapped rather than returning nothing",
			query:     "from=2026-08-20&to=2026-08-10",
			wantSince: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
			wantDays:  9,
		},
		{
			name:      "explicit dates beat an also-supplied days parameter",
			query:     "days=90&from=2026-08-01&to=2026-08-02",
			wantSince: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
			wantDays:  2,
		},
		{
			name:      "RFC3339 bounds are accepted",
			query:     "from=" + url.QueryEscape("2026-08-01T00:00:00Z") + "&to=" + url.QueryEscape("2026-08-02T00:00:00Z"),
			wantSince: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
			wantDays:  2,
		},
		{
			name:      "surrounding whitespace is trimmed",
			query:     "from=" + url.QueryEscape(" 2026-08-01 ") + "&to=" + url.QueryEscape(" 2026-08-02 "),
			wantSince: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
			wantDays:  2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			since, until, days := resolveWindow(ctxWithQuery(t, tc.query), 30, 365)
			if !since.UTC().Equal(tc.wantSince) {
				t.Errorf("since = %v, want %v", since.UTC(), tc.wantSince)
			}
			if !until.UTC().Equal(tc.wantUntil) {
				t.Errorf("until = %v, want %v", until.UTC(), tc.wantUntil)
			}
			if days != tc.wantDays {
				t.Errorf("days = %d, want %d", days, tc.wantDays)
			}
		})
	}
}

// A `from` with no `to` means "that day onwards". The end is anchored to the
// start of tomorrow rather than the current instant, so the window covers whole
// days and a saved link does not silently widen every time it is opened.
func TestResolveWindowFromWithoutTo(t *testing.T) {
	c := ctxWithQuery(t, "from=2026-08-01")
	since, until, days := resolveWindow(c, 30, 365)

	want := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if !since.UTC().Equal(want) {
		t.Fatalf("since = %v, want %v", since.UTC(), want)
	}
	if !until.After(since) {
		t.Fatalf("until %v must be after since %v", until, since)
	}
	// Whole days: the end is midnight, not the time the request arrived.
	if h, m, sec := until.UTC().Clock(); h != 0 || m != 0 || sec != 0 {
		t.Errorf("until = %v, want the start of a day", until.UTC())
	}
	if days != int(until.Sub(since).Hours()/24) {
		t.Fatalf("days = %d is not the whole-day span of the resolved bounds", days)
	}
}

// An unparseable `from` must not silently produce a zero-time window covering
// the year 1; it has to fall through to the days-based branch.
func TestResolveWindowIgnoresUnparseableFrom(t *testing.T) {
	before := time.Now().UTC()
	since, until, days := resolveWindow(ctxWithQuery(t, "from=not-a-date&days=7"), 30, 365)

	if days != 7 {
		t.Fatalf("days = %d, want 7 (fell through to the days branch)", days)
	}
	if since.Year() < 2000 {
		t.Fatalf("since = %v — an unparseable from must not yield the zero time", since)
	}
	if until.Before(before) {
		t.Fatalf("until = %v, want approximately now", until)
	}
	// The window must span exactly the requested number of days.
	if got := until.Sub(since).Hours(); got < 167 || got > 169 {
		t.Fatalf("window spans %.1f hours, want ~168", got)
	}
}

// The days form is what the dashboard uses by default. The cap exists so one
// request cannot aggregate an unbounded slice of history.
func TestResolveWindowDaysForm(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		defaults int
		max      int
		wantDays int
	}{
		{"no parameters uses the default", "", 30, 365, 30},
		{"an explicit day count is honoured", "days=7", 30, 365, 7},
		{"one day is allowed", "days=1", 30, 365, 1},
		{"a count above the cap is clamped", "days=9999", 30, 365, 365},
		{"exactly the cap is allowed", "days=365", 30, 365, 365},
		{"zero is rejected and the default applies", "days=0", 30, 365, 30},
		{"a negative count is rejected", "days=-5", 30, 365, 30},
		{"a non-numeric count is rejected", "days=lots", 30, 365, 30},
		{"a default above the cap is itself clamped", "", 900, 365, 365},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			since, until, days := resolveWindow(ctxWithQuery(t, tc.query), tc.defaults, tc.max)
			if days != tc.wantDays {
				t.Fatalf("days = %d, want %d", days, tc.wantDays)
			}
			// The bounds must actually match the day count, or the query and
			// the label the UI shows disagree.
			spanDays := until.Sub(since).Hours() / 24
			if spanDays < float64(tc.wantDays)-0.05 || spanDays > float64(tc.wantDays)+0.05 {
				t.Fatalf("window spans %.2f days, want %d", spanDays, tc.wantDays)
			}
		})
	}
}

// buildResultsFilter turns the URL-list query string into a Mongo filter.
// Every branch here is reachable from an unauthenticated-ish query parameter,
// so user input must be escaped rather than interpolated into a regex.
func TestBuildResultsFilterEmptyQuery(t *testing.T) {
	got := buildResultsFilter(ctxWithQuery(t, ""))
	if len(got) != 0 {
		t.Fatalf("an empty query must produce an empty filter, got %v", got)
	}
}

func TestBuildResultsFilterStatusCode(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  interface{} // nil = key absent
	}{
		{"numeric status code", "status_code=404", 404},
		{"zero is a legitimate value for a failed fetch", "status_code=0", 0},
		{"absent parameter adds no constraint", "", nil},
		{"empty parameter adds no constraint", "status_code=", nil},
		{"non-numeric input is ignored rather than erroring", "status_code=4xx", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildResultsFilter(ctxWithQuery(t, tc.query))
			v, ok := got["status_code"]
			if tc.want == nil {
				if ok {
					t.Fatalf("status_code should be absent, got %v", v)
				}
				return
			}
			if !ok || v != tc.want {
				t.Fatalf("status_code = %v (present %v), want %v", v, ok, tc.want)
			}
		})
	}
}

// The free-text URL search is fed straight into a $regex. Without quoting,
// a customer searching for "a.b" would match "axb", and a search for "(" would
// make Mongo reject the query outright.
func TestBuildResultsFilterQuotesTheURLSearch(t *testing.T) {
	cases := []struct {
		name  string
		q     string
		want  string
		absent bool
	}{
		{name: "plain text", q: "produkter", want: "produkter"},
		{name: "a dot is escaped so it is not a wildcard", q: "a.b", want: `a\.b`},
		{name: "regex metacharacters are neutralised", q: "a+b*c?", want: `a\+b\*c\?`},
		{name: "an unbalanced paren cannot break the query", q: "(", want: `\(`},
		{name: "whitespace is trimmed", q: "  shop  ", want: "shop"},
		{name: "a whitespace-only search adds no constraint", q: "   ", absent: true},
		{name: "an absent search adds no constraint", q: "", absent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildResultsFilter(ctxWithQuery(t, "q="+url.QueryEscape(tc.q)))
			raw, ok := got["url"]
			if tc.absent {
				if ok {
					t.Fatalf("url filter should be absent, got %v", raw)
				}
				return
			}
			m, isM := raw.(bson.M)
			if !ok || !isM {
				t.Fatalf("url filter = %v, want a bson.M", raw)
			}
			if m["$regex"] != tc.want {
				t.Fatalf("$regex = %v, want %q", m["$regex"], tc.want)
			}
			if m["$options"] != "i" {
				t.Errorf("URL search must be case-insensitive, got options %v", m["$options"])
			}
		})
	}
}

// Header filtering must be case-insensitive on both the name and the value,
// because origins disagree on casing and the customer types whatever they saw.
func TestBuildResultsFilterHeaderMatching(t *testing.T) {
	t.Run("header alone matches on the name only", func(t *testing.T) {
		got := buildResultsFilter(ctxWithQuery(t, "header=cf-cache-status"))
		expr, ok := got["$expr"]
		if !ok {
			t.Fatalf("expected an $expr filter, got %v", got)
		}
		conds := headerConds(t, expr)
		if len(conds) != 1 {
			t.Fatalf("expected 1 condition (name only), got %d: %v", len(conds), conds)
		}
		assertRegexMatch(t, conds[0], "$$pair.k", `^cf-cache-status$`)
	})

	t.Run("header and value match both sides", func(t *testing.T) {
		got := buildResultsFilter(ctxWithQuery(t, "header=CF-Cache-Status&value=HIT"))
		conds := headerConds(t, got["$expr"])
		if len(conds) != 2 {
			t.Fatalf("expected 2 conditions, got %d: %v", len(conds), conds)
		}
		assertRegexMatch(t, conds[0], "$$pair.k", `^CF-Cache-Status$`)
		assertRegexMatch(t, conds[1], "$$pair.v", `^HIT$`)
	})

	t.Run("a value without a header is ignored", func(t *testing.T) {
		got := buildResultsFilter(ctxWithQuery(t, "value=HIT"))
		if _, ok := got["$expr"]; ok {
			t.Fatalf("value alone must not build a header filter, got %v", got)
		}
	})

	t.Run("a whitespace-only header is ignored", func(t *testing.T) {
		got := buildResultsFilter(ctxWithQuery(t, "header="+url.QueryEscape("   ")))
		if _, ok := got["$expr"]; ok {
			t.Fatalf("blank header must not build a filter, got %v", got)
		}
	})

	t.Run("a whitespace-only value falls back to name-only matching", func(t *testing.T) {
		got := buildResultsFilter(ctxWithQuery(t, "header=Age&value="+url.QueryEscape("  ")))
		conds := headerConds(t, got["$expr"])
		if len(conds) != 1 {
			t.Fatalf("expected name-only matching, got %d conditions", len(conds))
		}
	})

	// A header value is user input reaching a regex engine; ".*" must match
	// the literal string, not everything.
	t.Run("regex metacharacters in the value are escaped", func(t *testing.T) {
		got := buildResultsFilter(ctxWithQuery(t, "header=Age&value="+url.QueryEscape(".*")))
		conds := headerConds(t, got["$expr"])
		assertRegexMatch(t, conds[1], "$$pair.v", `^\.\*$`)
	})
}

// headerConds digs the $and condition list out of the header filter so a test
// can assert on it without repeating the nesting each time.
func headerConds(t *testing.T, expr interface{}) bson.A {
	t.Helper()
	top, ok := expr.(bson.M)
	if !ok {
		t.Fatalf("$expr = %v, want bson.M", expr)
	}
	anyEl, ok := top["$anyElementTrue"].(bson.A)
	if !ok || len(anyEl) != 1 {
		t.Fatalf("$anyElementTrue = %v", top["$anyElementTrue"])
	}
	mapM, ok := anyEl[0].(bson.M)
	if !ok {
		t.Fatalf("expected a bson.M inside $anyElementTrue, got %v", anyEl[0])
	}
	inner, ok := mapM["$map"].(bson.M)
	if !ok {
		t.Fatalf("$map = %v", mapM["$map"])
	}
	in, ok := inner["in"].(bson.M)
	if !ok {
		t.Fatalf("in = %v", inner["in"])
	}
	conds, ok := in["$and"].(bson.A)
	if !ok {
		t.Fatalf("$and = %v", in["$and"])
	}
	return conds
}

func assertRegexMatch(t *testing.T, cond interface{}, wantInput, wantRegex string) {
	t.Helper()
	m, ok := cond.(bson.M)
	if !ok {
		t.Fatalf("condition = %v, want bson.M", cond)
	}
	rm, ok := m["$regexMatch"].(bson.M)
	if !ok {
		t.Fatalf("$regexMatch = %v", m["$regexMatch"])
	}
	if rm["input"] != wantInput {
		t.Errorf("input = %v, want %q", rm["input"], wantInput)
	}
	if rm["regex"] != wantRegex {
		t.Errorf("regex = %v, want %q", rm["regex"], wantRegex)
	}
	if rm["options"] != "i" {
		t.Errorf("options = %v, want %q", rm["options"], "i")
	}
}

// The url_type filter on the *list* view is content-type based (post-fetch),
// unlike the extension-based ingestion filter. Confusing the two would make
// the two numbers in the UI disagree.
func TestBuildResultsFilterURLTypeUsesContentType(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		present bool
		mustHit string
	}{
		{"static matches asset content types", "url_type=static", true, "text/css"},
		{"dynamic matches document content types", "url_type=dynamic", true, "text/html"},
		{"uppercase input is normalised", "url_type=STATIC", true, "text/css"},
		{"mixed case input is normalised", "url_type=Dynamic", true, "text/html"},
		{"all adds no content-type constraint", "url_type=all", false, ""},
		{"an unknown value adds no constraint", "url_type=weird", false, ""},
		{"an absent parameter adds no constraint", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildResultsFilter(ctxWithQuery(t, tc.query))
			raw, ok := got["content_type"]
			if !tc.present {
				if ok {
					t.Fatalf("content_type should be absent, got %v", raw)
				}
				return
			}
			m, isM := raw.(bson.M)
			if !ok || !isM {
				t.Fatalf("content_type = %v, want bson.M", raw)
			}
			rx, _ := m["$regex"].(string)
			if rx == "" || !strings.Contains(rx, tc.mustHit) {
				t.Fatalf("$regex = %q, expected it to mention %q", rx, tc.mustHit)
			}
			if m["$options"] != "i" {
				t.Errorf("content-type matching must be case-insensitive")
			}
		})
	}
}

// The filter clauses are independent and must compose: the UI lets a customer
// narrow by status, header and search text at once.
func TestBuildResultsFilterCombinesClauses(t *testing.T) {
	got := buildResultsFilter(ctxWithQuery(t,
		"status_code=200&header=CF-Cache-Status&value=MISS&q=shop&url_type=dynamic"))

	for _, key := range []string{"status_code", "$expr", "url", "content_type"} {
		if _, ok := got[key]; !ok {
			t.Errorf("filter is missing %q: %v", key, got)
		}
	}
	if len(got) != 4 {
		t.Fatalf("expected exactly 4 clauses, got %d: %v", len(got), got)
	}
}
