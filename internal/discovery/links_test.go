package discovery

import (
	"net/url"
	"strings"
	"testing"
)

// IsStaticURL decides whether a URL is fetched for further link extraction and
// which url_type scope it falls in. It is extension-based by design, since the
// Content-Type is unknown until after the fetch.
func TestIsStaticURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"pretty URL is a page", "https://example.dk/produkter/filter", false},
		{"root is a page", "https://example.dk/", false},
		{"no path is a page", "https://example.dk", false},
		{"html extension is a page", "https://example.dk/index.html", false},
		{"php extension is a page", "https://example.dk/index.php", false},
		{"aspx is unknown and treated as a page", "https://example.dk/a.aspx", false},

		{"stylesheet", "https://example.dk/a.css", true},
		{"javascript", "https://example.dk/a.js", true},
		{"es module", "https://example.dk/a.mjs", true},
		{"source map", "https://example.dk/a.js.map", true},
		{"png", "https://example.dk/a.png", true},
		{"jpeg", "https://example.dk/a.jpeg", true},
		{"webp", "https://example.dk/a.webp", true},
		{"svg", "https://example.dk/a.svg", true},
		{"woff2 font", "https://example.dk/a.woff2", true},
		{"mp4", "https://example.dk/a.mp4", true},
		{"pdf", "https://example.dk/a.pdf", true},
		{"zip", "https://example.dk/a.zip", true},
		{"xlsx", "https://example.dk/a.xlsx", true},
		{"json", "https://example.dk/a.json", true},
		{"xml sitemap counts as static", "https://example.dk/sitemap.xml", true},
		{"txt", "https://example.dk/robots.txt", true},
		{"csv", "https://example.dk/a.csv", true},

		// Casing and query strings are where an extension check most often
		// goes wrong on real sites.
		{"uppercase extension", "https://example.dk/A.PNG", true},
		{"mixed-case extension", "https://example.dk/a.Css", true},
		{"query string is not part of the path", "https://example.dk/a.css?v=2", true},
		{"fragment is not part of the path", "https://example.dk/a.css#x", true},
		{"query string on a page", "https://example.dk/search?q=a.css", false},

		// A dot in a directory name must not be read as a file extension.
		{"versioned directory", "https://example.dk/v1.2/page", false},
		{"trailing slash after an extension-like segment", "https://example.dk/a.css/", true},
		{"trailing slash on a directory", "https://example.dk/page/", false},
		{"dotted directory with trailing slash", "https://example.dk/v1.2/", false},

		{"unparseable URL is reported as non-static", "http://exa\x7fmple.dk/a.png", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsStaticURL(tc.url); got != tc.want {
				t.Fatalf("IsStaticURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

// looksLikeHTML is the inverse and gates recursion. Fetching an asset to look
// for links inside it wastes a request per asset on every crawl.
func TestLooksLikeHTML(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"", true},
		{"/", true},
		{"/page", true},
		{"/page/", true},
		{"/a.html", true},
		{"/a.css", false},
		{"/a.CSS", false},
		{"/dir.with.dots/page", true},
		{"/a.unknownext", true},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			u := &url.URL{Scheme: "https", Host: "example.dk", Path: tc.path}
			if got := looksLikeHTML(u); got != tc.want {
				t.Fatalf("looksLikeHTML(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// isHTMLContentType decides whether a fetched body is parsed for links at all.
func TestIsHTMLContentType(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"text/html", true},
		{"text/html; charset=utf-8", true},
		{"TEXT/HTML;charset=ISO-8859-1", true},
		{"  text/html  ", true},
		{"application/xhtml+xml", true},
		{"application/json", false},
		{"text/xml", false},
		{"image/svg+xml", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.ct, func(t *testing.T) {
			if got := isHTMLContentType(tc.ct); got != tc.want {
				t.Fatalf("isHTMLContentType(%q) = %v, want %v", tc.ct, got, tc.want)
			}
		})
	}
}

// emitCandidate is the choke point where non-web schemes are dropped before
// anything is queued. A javascript: or data: URI reaching the fetcher would be
// at best a wasted request and at worst a parsing surprise.
func TestEmitCandidateFiltersAndResolves(t *testing.T) {
	page, err := url.Parse("https://example.dk/shop/produkter/")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		raw  string
		want string // "" = must be dropped
	}{
		{"absolute https URL", "https://other.dk/a", "https://other.dk/a"},
		{"absolute http URL", "http://other.dk/a", "http://other.dk/a"},
		{"root-relative path", "/kontakt", "https://example.dk/kontakt"},
		{"document-relative path", "filter", "https://example.dk/shop/produkter/filter"},
		{"parent-relative path", "../kurv", "https://example.dk/shop/kurv"},
		{"protocol-relative URL inherits the page scheme", "//cdn.dk/a.js", "https://cdn.dk/a.js"},
		{"query-only reference", "?page=2", "https://example.dk/shop/produkter/?page=2"},

		{"empty href is dropped", "", ""},
		{"whitespace-only href is dropped", "   ", ""},
		{"fragment-only link is dropped", "#main", ""},
		{"javascript scheme is dropped", "javascript:void(0)", ""},
		{"mailto is dropped", "mailto:a@example.dk", ""},
		{"tel is dropped", "tel:+4512345678", ""},
		{"data URI is dropped", "data:image/gif;base64,AAA", ""},
		{"ftp is dropped", "ftp://example.dk/f", ""},

		// A colon *after* a slash is part of the path, not a scheme — dropping
		// these would silently lose real links.
		{"colon inside a path segment is not a scheme", "/a/b:c", "https://example.dk/a/b:c"},

		{"surrounding whitespace is trimmed", "  /kontakt  ", "https://example.dk/kontakt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			emitCandidate(page, tc.raw, func(u *url.URL) { got = append(got, u.String()) })

			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("%q should have been dropped, got %v", tc.raw, got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("emitCandidate(%q) = %v, want [%s]", tc.raw, got, tc.want)
			}
		})
	}
}

// srcset is how responsive images are declared; parsing only the first entry
// would leave most of a site's image weight undiscovered.
func TestParseSrcset(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single URL with no descriptor", "a.png", []string{"a.png"}},
		{"width descriptors", "a.png 400w, b.png 800w", []string{"a.png", "b.png"}},
		{"density descriptors", "a.png 1x, b.png 2x", []string{"a.png", "b.png"}},
		{"tab separator before the descriptor", "a.png\t400w", []string{"a.png"}},
		{"extra whitespace and a trailing comma", "  a.png 1x ,  b.png 2x , ", []string{"a.png", "b.png"}},
		{"absolute URLs", "https://cdn.dk/a.png 1x, https://cdn.dk/b.png 2x",
			[]string{"https://cdn.dk/a.png", "https://cdn.dk/b.png"}},
		{"only separators", " , , ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSrcset(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseSrcset(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseSrcset(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// A srcset URL containing a comma (legal in a query string) is a known
// ambiguity in the spec. This pins the ACTUAL behaviour so a change is visible.
func TestParseSrcsetSplitsOnCommasInsideURLs(t *testing.T) {
	got := parseSrcset("https://cdn.dk/a.png?w=1,2 400w")
	if len(got) != 2 {
		t.Fatalf("expected the naive comma split to yield 2 parts, got %v", got)
	}
	if got[0] != "https://cdn.dk/a.png?w=1" {
		t.Fatalf("got[0] = %q", got[0])
	}
}

// extractLinks must find every URL-bearing attribute the crawler reports on.
// A missed attribute is a whole class of asset the customer never sees cache
// data for.
func TestExtractLinksCoversEveryURLBearingAttribute(t *testing.T) {
	page, _ := url.Parse("https://example.dk/")
	doc := `<html><body>
  <a href="/page">p</a>
  <area href="/area">
  <link rel="stylesheet" href="/style.css">
  <script src="/app.js"></script>
  <iframe src="/frame"></iframe>
  <embed src="/embed.swf">
  <img src="/a.png" srcset="/a-400.png 400w, /a-800.png 800w">
  <video src="/v.mp4" poster="/poster.jpg"></video>
  <audio src="/a.mp3"></audio>
  <track src="/subs.vtt">
  <source src="/s.webm" srcset="/s-1.webm 1x">
  <object data="/o.pdf"></object>
  <frame src="/legacy">
</body></html>`

	var got []string
	extractLinks(strings.NewReader(doc), page, func(u *url.URL) { got = append(got, u.Path) })

	seen := map[string]bool{}
	for _, p := range got {
		seen[p] = true
	}

	for _, want := range []string{
		"/page", "/area", "/style.css", "/app.js", "/frame", "/embed.swf",
		"/a.png", "/a-400.png", "/a-800.png", "/v.mp4", "/poster.jpg",
		"/a.mp3", "/subs.vtt", "/s.webm", "/s-1.webm", "/o.pdf", "/legacy",
	} {
		if !seen[want] {
			t.Errorf("missed %q; found %v", want, got)
		}
	}
}

// Attributes that look URL-ish but are not references must be ignored, or the
// queue fills with URLs the site does not actually serve.
func TestExtractLinksIgnoresNonReferenceAttributes(t *testing.T) {
	page, _ := url.Parse("https://example.dk/")
	doc := `<html><body>
  <a name="anchor">no href</a>
  <div data-url="/not-a-link">x</div>
  <form action="/submit"><input type="text" value="/value"></form>
  <a href="javascript:void(0)">js</a>
  <a href="#top">fragment</a>
</body></html>`

	var got []string
	extractLinks(strings.NewReader(doc), page, func(u *url.URL) { got = append(got, u.String()) })

	if len(got) != 0 {
		t.Fatalf("expected no candidates, got %v", got)
	}
}

// A truncated or malformed document must yield what it can rather than
// aborting: connections drop mid-body all the time on real crawls.
func TestExtractLinksOnMalformedHTML(t *testing.T) {
	page, _ := url.Parse("https://example.dk/")
	cases := []string{
		"",
		"not html at all",
		`<a href="/a">one</a><a href="/b`,
		`<img src=`,
		"\x00\x01<a href=\"/a\">x</a>",
	}
	for _, doc := range cases {
		var got []string
		extractLinks(strings.NewReader(doc), page, func(u *url.URL) { got = append(got, u.String()) })
		_ = got // must not panic
	}
}

func TestExtractLinksTruncatedDocumentKeepsEarlierLinks(t *testing.T) {
	page, _ := url.Parse("https://example.dk/")
	var got []string
	extractLinks(strings.NewReader(`<a href="/a">one</a><a href="/b`), page,
		func(u *url.URL) { got = append(got, u.Path) })

	if len(got) != 1 || got[0] != "/a" {
		t.Fatalf("got %v, want [/a] from the part that arrived", got)
	}
}

// Discover rejects a bad seed before starting any goroutines, so a
// misconfigured site fails fast rather than hanging the ingestion path.
func TestDiscoverRejectsInvalidSeeds(t *testing.T) {
	d := New("test-agent")
	for _, bad := range []string{
		"ftp://example.dk/",
		"file:///etc/passwd",
		"example.dk",
		"",
	} {
		t.Run(bad, func(t *testing.T) {
			err := d.Discover(nil, bad, 1, func(string) bool { return true })
			if err == nil {
				t.Fatalf("expected %q to be rejected", bad)
			}
		})
	}
}
