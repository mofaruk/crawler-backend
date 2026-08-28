package source

import (
	"net/http"
	"strings"
	"testing"
)

// isValidURL is the single gate between a source document and the crawl queue.
// Anything it lets through is fetched server-side, so non-web schemes must be
// rejected here rather than relied on being caught downstream.
func TestIsValidURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"https URL", "https://example.dk/page", true},
		{"http URL", "http://example.dk/page", true},
		{"uppercase scheme is normalised by url.Parse", "HTTPS://example.dk/", true},
		{"empty string has no scheme", "", false},
		{"bare domain has no scheme", "example.dk/page", false},
		{"protocol-relative has no scheme", "//example.dk/page", false},
		{"root-relative path", "/page", false},
		{"ftp is not a web scheme", "ftp://example.dk/f", false},
		{"file scheme is rejected", "file:///etc/passwd", false},
		{"javascript scheme is rejected", "javascript:alert(1)", false},
		{"data URI is rejected", "data:text/html,<h1>x</h1>", false},
		{"mailto is rejected", "mailto:a@b.dk", false},
		{"gopher is rejected", "gopher://example.dk/", false},
		{"unparseable control characters", "http://exa\x7fmple.dk/", false},
		{"https with port and query", "https://example.dk:8443/a?b=c#d", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isValidURL(tc.raw); got != tc.want {
				t.Fatalf("isValidURL(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// parseCSV drives the "csv" source type. Its counters are what Diagnosis()
// later turns into the customer-facing explanation, so both the URLs and the
// stats have to be right.
func TestParseCSV(t *testing.T) {
	p := NewURLParser()

	cases := []struct {
		name        string
		body        string
		limit       int
		wantURLs    []string
		wantScanned int
		wantHeader  int
		wantEmpty   int
		wantInvalid int
		wantLimit   bool
	}{
		{
			name:        "plain one-column list",
			body:        "https://example.dk/a\nhttps://example.dk/b\n",
			wantURLs:    []string{"https://example.dk/a", "https://example.dk/b"},
			wantScanned: 2,
		},
		{
			name:        "lowercase url header row is skipped",
			body:        "url\nhttps://example.dk/a\n",
			wantURLs:    []string{"https://example.dk/a"},
			wantScanned: 2,
			wantHeader:  1,
		},
		{
			name:        "uppercase URL header row is skipped",
			body:        "URL\nhttps://example.dk/a\n",
			wantURLs:    []string{"https://example.dk/a"},
			wantScanned: 2,
			wantHeader:  1,
		},
		{
			name:        "a Url header with other casing is treated as data, not a header",
			body:        "Url\nhttps://example.dk/a\n",
			wantURLs:    []string{"https://example.dk/a"},
			wantScanned: 2,
			wantInvalid: 1,
		},
		{
			name:        "only the first column is read",
			body:        "https://example.dk/a,ignored,https://example.dk/never\n",
			wantURLs:    []string{"https://example.dk/a"},
			wantScanned: 1,
		},
		{
			name:        "surrounding whitespace is trimmed",
			body:        "  https://example.dk/a  \n",
			wantURLs:    []string{"https://example.dk/a"},
			wantScanned: 1,
		},
		{
			// The row must keep the same field count, or encoding/csv rejects
			// it as malformed before the empty-value branch is ever reached.
			name:        "blank first column counts as an empty row",
			body:        "https://example.dk/a,note\n,note\nhttps://example.dk/b,note\n",
			wantURLs:    []string{"https://example.dk/a", "https://example.dk/b"},
			wantScanned: 3,
			wantEmpty:   1,
		},
		{
			name:        "non-URL values are rejected, not crawled",
			body:        "not a url\nftp://example.dk/x\nhttps://example.dk/a\n",
			wantURLs:    []string{"https://example.dk/a"},
			wantScanned: 3,
			wantInvalid: 2,
		},
		{
			name:        "limit stops reading early",
			body:        "https://example.dk/a\nhttps://example.dk/b\nhttps://example.dk/c\n",
			limit:       2,
			wantURLs:    []string{"https://example.dk/a", "https://example.dk/b"},
			wantScanned: 2,
			wantLimit:   true,
		},
		{
			name:        "limit of zero means unlimited",
			body:        "https://example.dk/a\nhttps://example.dk/b\n",
			limit:       0,
			wantURLs:    []string{"https://example.dk/a", "https://example.dk/b"},
			wantScanned: 2,
		},
		{
			name:        "quoted fields containing commas",
			body:        "\"https://example.dk/a?x=1,2\",note\n",
			wantURLs:    []string{"https://example.dk/a?x=1,2"},
			wantScanned: 1,
		},
		{
			name: "empty document yields nothing",
			body: "",
		},
		{
			name:        "CRLF line endings",
			body:        "https://example.dk/a\r\nhttps://example.dk/b\r\n",
			wantURLs:    []string{"https://example.dk/a", "https://example.dk/b"},
			wantScanned: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stats := &ParseStats{SourceType: "csv"}
			got, err := p.parseCSV(strings.NewReader(tc.body), tc.limit, stats)
			if err != nil {
				t.Fatalf("parseCSV returned error: %v", err)
			}
			if len(got) != len(tc.wantURLs) {
				t.Fatalf("URLs = %v, want %v", got, tc.wantURLs)
			}
			for i := range got {
				if got[i] != tc.wantURLs[i] {
					t.Fatalf("URLs = %v, want %v", got, tc.wantURLs)
				}
			}
			if stats.CSVRowsScanned != tc.wantScanned {
				t.Errorf("CSVRowsScanned = %d, want %d", stats.CSVRowsScanned, tc.wantScanned)
			}
			if stats.CSVHeaderRows != tc.wantHeader {
				t.Errorf("CSVHeaderRows = %d, want %d", stats.CSVHeaderRows, tc.wantHeader)
			}
			if stats.CSVEmptyRows != tc.wantEmpty {
				t.Errorf("CSVEmptyRows = %d, want %d", stats.CSVEmptyRows, tc.wantEmpty)
			}
			if stats.URLsRejectedInvalid != tc.wantInvalid {
				t.Errorf("URLsRejectedInvalid = %d, want %d", stats.URLsRejectedInvalid, tc.wantInvalid)
			}
			if stats.URLsAccepted != len(tc.wantURLs) {
				t.Errorf("URLsAccepted = %d, want %d", stats.URLsAccepted, len(tc.wantURLs))
			}
			if stats.LimitReached != tc.wantLimit {
				t.Errorf("LimitReached = %v, want %v", stats.LimitReached, tc.wantLimit)
			}
		})
	}
}

// A CSV with a varying column count is the single most common real-world
// malformation (a stray comma inside an unquoted field). It must be counted
// and skipped, not abort the whole import.
func TestParseCSVCountsMalformedRowsAndKeepsGoing(t *testing.T) {
	p := NewURLParser()
	stats := &ParseStats{SourceType: "csv"}

	// Row 2 has more fields than row 1, which encoding/csv rejects.
	body := "https://example.dk/a,one\nhttps://example.dk/b,one,two\nhttps://example.dk/c,three\n"
	got, err := p.parseCSV(strings.NewReader(body), 0, stats)
	if err != nil {
		t.Fatalf("parseCSV must not fail on a malformed row: %v", err)
	}
	if stats.CSVMalformed == 0 {
		t.Fatalf("expected a malformed row to be counted, stats %+v", stats)
	}
	if len(got) != 2 || got[0] != "https://example.dk/a" || got[1] != "https://example.dk/c" {
		t.Fatalf("good rows on either side of the malformed one must survive, got %v", got)
	}
}

// parseSitemapReader backs sitemap *discovery*: it only needs to know a
// document is a real sitemap with entries, and deliberately does not walk
// child sitemaps — it returns their URLs as results instead.
func TestParseSitemapReader(t *testing.T) {
	p := NewURLParser()

	cases := []struct {
		name       string
		body       string
		limit      int
		wantURLs   []string
		wantFormat string
		wantChild  int
	}{
		{
			name: "urlset yields page URLs",
			body: `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.dk/a</loc></url>
  <url><loc>https://example.dk/b</loc></url>
</urlset>`,
			wantURLs:   []string{"https://example.dk/a", "https://example.dk/b"},
			wantFormat: "urlset",
		},
		{
			name: "sitemapindex yields child sitemap URLs without following them",
			body: `<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://example.dk/sitemap-1.xml</loc></sitemap>
  <sitemap><loc>https://example.dk/sitemap-2.xml</loc></sitemap>
</sitemapindex>`,
			wantURLs:   []string{"https://example.dk/sitemap-1.xml", "https://example.dk/sitemap-2.xml"},
			wantFormat: "sitemapindex",
			wantChild:  2,
		},
		{
			name: "whitespace around loc is trimmed",
			body: `<urlset><url><loc>
   https://example.dk/a
  </loc></url></urlset>`,
			wantURLs:   []string{"https://example.dk/a"},
			wantFormat: "urlset",
		},
		{
			name: "non-http locs are dropped",
			body: `<urlset>
  <url><loc>ftp://example.dk/a</loc></url>
  <url><loc></loc></url>
  <url><loc>https://example.dk/b</loc></url>
</urlset>`,
			wantURLs:   []string{"https://example.dk/b"},
			wantFormat: "urlset",
		},
		{
			name:       "limit truncates the result",
			body:       `<urlset><url><loc>https://example.dk/a</loc></url><url><loc>https://example.dk/b</loc></url><url><loc>https://example.dk/c</loc></url></urlset>`,
			limit:      2,
			wantURLs:   []string{"https://example.dk/a", "https://example.dk/b"},
			wantFormat: "urlset",
		},
		{
			name:       "HTML served instead of a sitemap yields nothing and no format",
			body:       `<!doctype html><html><body><h1>Not a sitemap</h1></body></html>`,
			wantFormat: "",
		},
		{
			name:       "empty document",
			body:       "",
			wantFormat: "",
		},
		{
			name:       "urlset with no entries is still recognised as a sitemap",
			body:       `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"></urlset>`,
			wantFormat: "urlset",
		},
		{
			name: "malformed tail still returns what was parsed",
			body: `<urlset><url><loc>https://example.dk/a</loc></url><url><loc>https://exa`,
			wantURLs:   []string{"https://example.dk/a"},
			wantFormat: "urlset",
		},
		{
			name: "a legacy charset declaration is transcoded rather than aborting",
			body: `<?xml version="1.0" encoding="ISO-8859-1"?>
<urlset><url><loc>https://example.dk/a</loc></url></urlset>`,
			wantURLs:   []string{"https://example.dk/a"},
			wantFormat: "urlset",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stats := &ParseStats{SourceType: "xml"}
			got, err := p.parseSitemapReader(strings.NewReader(tc.body), tc.limit, stats)
			if err != nil {
				t.Fatalf("parseSitemapReader returned error: %v", err)
			}
			if len(got) != len(tc.wantURLs) {
				t.Fatalf("URLs = %v, want %v", got, tc.wantURLs)
			}
			for i := range got {
				if got[i] != tc.wantURLs[i] {
					t.Fatalf("URLs = %v, want %v", got, tc.wantURLs)
				}
			}
			if stats.XMLFormat != tc.wantFormat {
				t.Errorf("XMLFormat = %q, want %q", stats.XMLFormat, tc.wantFormat)
			}
			if stats.XMLChildSitemaps != tc.wantChild {
				t.Errorf("XMLChildSitemaps = %d, want %d", stats.XMLChildSitemaps, tc.wantChild)
			}
		})
	}
}

// ParseURLs must reject an unknown source type before doing any network work,
// and still hand back stats so the caller can log what it was asked for.
func TestParseURLsRejectsUnknownSourceType(t *testing.T) {
	p := NewURLParser()
	for _, st := range []string{"json", "", "sitemap", "XMLX"} {
		urls, stats, err := p.ParseURLs(nil, "https://example.dk/x", st, "", 0)
		if err == nil {
			t.Fatalf("source type %q: expected an error", st)
		}
		if urls != nil {
			t.Errorf("source type %q: expected no URLs, got %v", st, urls)
		}
		if stats == nil {
			t.Fatalf("source type %q: stats must never be nil", st)
		}
		if stats.SourceType != strings.ToLower(st) {
			t.Errorf("SourceType = %q, want %q", stats.SourceType, strings.ToLower(st))
		}
	}
}

// The source type is lower-cased on the way in, so a site configured with
// "CSV" or "XML" is not silently treated as unsupported.
func TestParseURLsNormalisesSourceTypeCasing(t *testing.T) {
	p := NewURLParser()
	_, stats, _ := p.ParseURLs(nil, "https://example.dk/x", "XML", "", 0)
	if stats.SourceType != "xml" {
		t.Fatalf("SourceType = %q, want %q", stats.SourceType, "xml")
	}
}

// Diagnosis() is the text a customer reads when their import produced nothing.
// A wrong branch here means a support ticket, so each precedence rule is
// pinned explicitly.
func TestParseStatsDiagnosis(t *testing.T) {
	cases := []struct {
		name     string
		stats    *ParseStats
		contains string
	}{
		{
			name:     "nil stats degrade gracefully",
			stats:    nil,
			contains: "no parse statistics available",
		},
		{
			name:     "a non-200 status outranks every other explanation",
			stats:    &ParseStats{SourceType: "xml", HTTPStatus: http.StatusNotFound, ContentBytes: 500, XMLFormat: "urlset"},
			contains: "source returned HTTP 404",
		},
		{
			name:     "a 500 is reported by its code",
			stats:    &ParseStats{SourceType: "csv", HTTPStatus: 500},
			contains: "source returned HTTP 500",
		},
		{
			name:     "an empty body is reported before any format analysis",
			stats:    &ParseStats{SourceType: "csv", HTTPStatus: 200, ContentBytes: 0},
			contains: "source returned 0 bytes",
		},
		{
			name:     "bytes but no CSV rows",
			stats:    &ParseStats{SourceType: "csv", HTTPStatus: 200, ContentBytes: 120},
			contains: "produced 0 parsable rows",
		},
		{
			name: "CSV rows present but no valid URLs names the column",
			stats: &ParseStats{
				SourceType: "csv", HTTPStatus: 200, ContentBytes: 400,
				CSVRowsScanned: 10, URLsRejectedInvalid: 10,
			},
			contains: "none were valid http(s) URLs",
		},
		{
			name: "every CSV row malformed is called out specifically",
			stats: &ParseStats{
				SourceType: "csv", HTTPStatus: 200, ContentBytes: 400,
				CSVRowsScanned: 6, CSVMalformed: 6,
			},
			contains: "all 6 rows malformed",
		},
		{
			name: "CSV rows that were all headers or empty fall through to the generic message",
			stats: &ParseStats{
				SourceType: "csv", HTTPStatus: 200, ContentBytes: 400,
				CSVRowsScanned: 4, CSVEmptyRows: 3, CSVHeaderRows: 1,
			},
			contains: "column 1 yielded no usable URLs",
		},
		{
			name: "XML that is not a sitemap at all",
			stats: &ParseStats{
				SourceType: "xml", HTTPStatus: 200, ContentBytes: 900, XMLFormat: "unknown",
			},
			contains: "found no <urlset> or <sitemapindex> elements",
		},
		{
			name: "an unset XML format is treated the same as unknown",
			stats: &ParseStats{
				SourceType: "xml", HTTPStatus: 200, ContentBytes: 900,
			},
			contains: "found no <urlset> or <sitemapindex> elements",
		},
		{
			name: "a decoder error is surfaced verbatim so the user can see the syntax problem",
			stats: &ParseStats{
				SourceType: "xml", HTTPStatus: 200, ContentBytes: 900,
				XMLFormat: "unknown", XMLParseError: "XML syntax error on line 3",
			},
			contains: "XML syntax error on line 3",
		},
		{
			name: "an empty sitemapindex",
			stats: &ParseStats{
				SourceType: "xml", HTTPStatus: 200, ContentBytes: 300,
				XMLFormat: "sitemapindex", XMLChildSitemaps: 0,
			},
			contains: "contained no <sitemap><loc> entries",
		},
		{
			name: "children that all failed to fetch",
			stats: &ParseStats{
				SourceType: "xml", HTTPStatus: 200, ContentBytes: 300,
				XMLFormat: "sitemapindex", XMLChildSitemaps: 4,
				XMLDocumentsFailed: 4, XMLDepthReached: 1,
			},
			contains: "referenced 4 child sitemap(s)",
		},
		{
			name: "a urlset with zero loc entries",
			stats: &ParseStats{
				SourceType: "xml", HTTPStatus: 200, ContentBytes: 300,
				XMLFormat: "urlset", XMLLocEntries: 0,
			},
			contains: "contained 0 <url><loc> entries",
		},
		{
			name: "locs present but all invalid",
			stats: &ParseStats{
				SourceType: "xml", HTTPStatus: 200, ContentBytes: 300,
				XMLFormat: "urlset", XMLLocEntries: 5, URLsRejectedInvalid: 5,
			},
			contains: "none accepted",
		},
		{
			name: "an unsupported source type says so",
			stats: &ParseStats{
				SourceType: "json", HTTPStatus: 200, ContentBytes: 300,
			},
			contains: `unsupported source type "json"`,
		},
		{
			name: "source type casing does not break the switch",
			stats: &ParseStats{
				SourceType: "CSV", HTTPStatus: 200, ContentBytes: 400, CSVRowsScanned: 0,
			},
			contains: "produced 0 parsable rows",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.stats.Diagnosis()
			if !strings.Contains(got, tc.contains) {
				t.Fatalf("Diagnosis() = %q, want it to contain %q", got, tc.contains)
			}
		})
	}
}

// A successful parse never calls Diagnosis(), but if a caller does it must not
// panic or claim HTTP 200 is a failure — the string is only advisory.
func TestDiagnosisOnASuccessfulParseIsHarmless(t *testing.T) {
	s := &ParseStats{
		SourceType: "xml", HTTPStatus: 200, ContentBytes: 1200,
		XMLFormat: "urlset", XMLLocEntries: 40, URLsAccepted: 40,
	}
	if got := s.Diagnosis(); got == "" {
		t.Fatal("Diagnosis() must always return something printable")
	}
}

// countingReader is what makes ContentBytes trustworthy, and ContentBytes is
// the first branch of Diagnosis() — a miscount would report "0 bytes" for a
// source that actually returned data.
func TestCountingReaderTalliesEveryByte(t *testing.T) {
	body := strings.Repeat("https://example.dk/a\n", 100)
	cr := &countingReader{r: strings.NewReader(body)}

	buf := make([]byte, 7) // deliberately not a divisor of the length
	for {
		n, err := cr.Read(buf)
		_ = n
		if err != nil {
			break
		}
	}
	if cr.n != int64(len(body)) {
		t.Fatalf("counted %d bytes, want %d", cr.n, len(body))
	}
}

func TestCountingReaderOnEmptySource(t *testing.T) {
	cr := &countingReader{r: strings.NewReader("")}
	buf := make([]byte, 8)
	if _, err := cr.Read(buf); err == nil {
		t.Fatal("expected EOF from an empty reader")
	}
	if cr.n != 0 {
		t.Fatalf("counted %d bytes from an empty source", cr.n)
	}
}
