package crawler

import (
	"strings"
	"testing"
)

// A complete, well-formed page is the baseline: every signal must be picked up
// in one pass, because the crawler only ever reads the body once.
func TestExtractPageSignalsFullDocument(t *testing.T) {
	doc := `<!doctype html>
<html>
<head>
  <title>  Billige   filtre  til   ventilation </title>
  <meta name="description" content="  Vi sælger filtre.  ">
  <meta name="robots" content="index, follow">
  <link rel="canonical" href="https://example.dk/filtre">
</head>
<body>
  <h1>Filtre</h1>
  <p>One two three four five.</p>
  <img src="https://example.dk/a.png" alt="A filter">
  <img src="https://example.dk/b.png">
</body>
</html>`

	got := ExtractPageSignals(strings.NewReader(doc), true)

	// Whitespace inside a title is collapsed: the raw value would otherwise
	// render with newlines in the dashboard and break duplicate matching.
	if got.Title != "Billige filtre til ventilation" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.TitleLength != len([]rune("Billige filtre til ventilation")) {
		t.Errorf("TitleLength = %d", got.TitleLength)
	}
	if got.MetaDescription != "Vi sælger filtre." {
		t.Errorf("MetaDescription = %q", got.MetaDescription)
	}
	// Length is counted in runes, not bytes: "æ" is two bytes but one
	// character, and a byte count would misreport every Danish title.
	if got.MetaDescLength != len([]rune("Vi sælger filtre.")) {
		t.Errorf("MetaDescLength = %d, want %d", got.MetaDescLength, len([]rune("Vi sælger filtre.")))
	}
	if got.Canonical != "https://example.dk/filtre" {
		t.Errorf("Canonical = %q", got.Canonical)
	}
	if got.NoIndex {
		t.Error("robots index,follow must not set NoIndex")
	}
	if got.H1Count != 1 {
		t.Errorf("H1Count = %d, want 1", got.H1Count)
	}
	if got.ImagesMissingAlt != 1 {
		t.Errorf("ImagesMissingAlt = %d, want 1", got.ImagesMissingAlt)
	}
	if got.InsecureRefs != 0 {
		t.Errorf("InsecureRefs = %d, want 0", got.InsecureRefs)
	}
	if got.SoftNotFound {
		t.Error("a normal title must not be flagged as a soft 404")
	}
	if got.WordCount == 0 {
		t.Error("body text should have produced a word count")
	}
}

// Title extraction has to survive real-world markup: empty tags, missing
// tags, entities, and nesting.
func TestExtractTitle(t *testing.T) {
	cases := []struct {
		name      string
		html      string
		wantTitle string
		wantLen   int
	}{
		{"no title element", `<html><head></head><body>x</body></html>`, "", 0},
		{"empty title element", `<html><head><title></title></head></html>`, "", 0},
		{"whitespace-only title stays empty", "<html><head><title>   \n\t </title></head></html>", "", 0},
		{"simple title", `<title>Shop</title>`, "Shop", 4},
		{"internal whitespace collapsed", "<title>A\n  B\tC</title>", "A B C", 5},
		{"leading and trailing space trimmed", `<title>   Padded   </title>`, "Padded", 6},
		{"entities are decoded by the tokenizer", `<title>Filtre &amp; ventiler</title>`, "Filtre & ventiler", 17},
		{"non-ASCII counted in runes not bytes", `<title>Ærø</title>`, "Ærø", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractPageSignals(strings.NewReader(tc.html), false)
			if got.Title != tc.wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, tc.wantTitle)
			}
			if got.TitleLength != tc.wantLen {
				t.Errorf("TitleLength = %d, want %d", got.TitleLength, tc.wantLen)
			}
		})
	}
}

// The noindex check reads a substring of the robots content because real sites
// write it in every combination and casing.
func TestNoIndexDetection(t *testing.T) {
	cases := []struct {
		name string
		html string
		want bool
	}{
		{"no robots meta", `<head><title>t</title></head>`, false},
		{"index,follow is not noindex", `<meta name="robots" content="index, follow">`, false},
		{"bare noindex", `<meta name="robots" content="noindex">`, true},
		{"noindex,nofollow", `<meta name="robots" content="noindex, nofollow">`, true},
		{"uppercase NOINDEX", `<meta name="robots" content="NOINDEX">`, true},
		{"mixed-case attribute name", `<meta NAME="Robots" content="NoIndex">`, true},
		{"noindex on a different meta name is ignored", `<meta name="googlebot" content="noindex">`, false},
		{"empty content", `<meta name="robots" content="">`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractPageSignals(strings.NewReader(tc.html), false).NoIndex; got != tc.want {
				t.Fatalf("NoIndex = %v, want %v", got, tc.want)
			}
		})
	}
}

// A canonical URL is only meaningful from rel="canonical"; other link
// relations (stylesheets, icons, preloads) are on nearly every page and would
// otherwise overwrite it.
func TestCanonicalExtraction(t *testing.T) {
	cases := []struct {
		name string
		html string
		want string
	}{
		{"no link tags", `<head><title>t</title></head>`, ""},
		{"rel canonical", `<link rel="canonical" href="https://example.dk/a">`, "https://example.dk/a"},
		{"rel canonical uppercase", `<link rel="CANONICAL" href="https://example.dk/a">`, "https://example.dk/a"},
		{"stylesheet is not canonical", `<link rel="stylesheet" href="/s.css">`, ""},
		{"icon is not canonical", `<link rel="icon" href="/f.ico">`, ""},
		{"href whitespace trimmed", `<link rel="canonical" href="  /a  ">`, "/a"},
		{"canonical without href yields empty", `<link rel="canonical">`, ""},
		{"a later canonical overwrites an earlier one",
			`<link rel="canonical" href="/first"><link rel="canonical" href="/second">`, "/second"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractPageSignals(strings.NewReader(tc.html), false).Canonical; got != tc.want {
				t.Fatalf("Canonical = %q, want %q", got, tc.want)
			}
		})
	}
}

// The description is read from name="description" only. A page can carry a
// dozen og:/twitter: metas and none of them is what search results show.
func TestMetaDescriptionExtraction(t *testing.T) {
	cases := []struct {
		name    string
		html    string
		want    string
		wantLen int
	}{
		{"absent", `<head><title>t</title></head>`, "", 0},
		{"present", `<meta name="description" content="Hello">`, "Hello", 5},
		{"case-insensitive name", `<meta name="DESCRIPTION" content="Hello">`, "Hello", 5},
		{"trimmed", `<meta name="description" content="   Hello   ">`, "Hello", 5},
		{"og:description is a different tag", `<meta property="og:description" content="Hello">`, "", 0},
		{"empty content", `<meta name="description" content="">`, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractPageSignals(strings.NewReader(tc.html), false)
			if got.MetaDescription != tc.want {
				t.Errorf("MetaDescription = %q, want %q", got.MetaDescription, tc.want)
			}
			if got.MetaDescLength != tc.wantLen {
				t.Errorf("MetaDescLength = %d, want %d", got.MetaDescLength, tc.wantLen)
			}
		})
	}
}

// H1s are counted, not just detected, because "several main headings" and "no
// main heading" are two different findings in the issue list.
func TestH1Counting(t *testing.T) {
	cases := []struct {
		name string
		html string
		want int
	}{
		{"none", `<body><h2>Sub</h2></body>`, 0},
		{"one", `<body><h1>A</h1></body>`, 1},
		{"three", `<body><h1>A</h1><h1>B</h1><h1>C</h1></body>`, 3},
		{"h1 with attributes still counts", `<body><h1 class="x" id="y">A</h1></body>`, 1},
		{"h11 is not an h1", `<body><h11>A</h11></body>`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractPageSignals(strings.NewReader(tc.html), false).H1Count; got != tc.want {
				t.Fatalf("H1Count = %d, want %d", got, tc.want)
			}
		})
	}
}

// Word counting is what "thin content" is judged on, and it counts only body
// text — head content (titles, JSON-LD in <script>) would inflate every page.
func TestWordCounting(t *testing.T) {
	cases := []struct {
		name string
		html string
		want int
	}{
		{"no body element counts nothing",
			`<html><head><title>one two three</title></head></html>`, 0},
		{"body words counted",
			`<html><body>one two three four</body></html>`, 4},
		{"whitespace runs are one separator",
			"<html><body>one    two\n\nthree</body></html>", 3},
		{"text across several elements accumulates",
			`<html><body><p>one two</p><p>three</p></body></html>`, 3},
		{"empty body counts zero",
			`<html><body>   </body></html>`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractPageSignals(strings.NewReader(tc.html), false).WordCount; got != tc.want {
				t.Fatalf("WordCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// Script and style contents are text tokens too. Documenting the ACTUAL
// behaviour here matters: if inline JS counts toward the word total, a
func TestImagesMissingAlt(t *testing.T) {
	cases := []struct {
		name string
		html string
		want int
	}{
		{"no images", `<body><p>x</p></body>`, 0},
		{"img with alt", `<body><img src="a.png" alt="A cat"></body>`, 0},
		{"img without alt attribute", `<body><img src="a.png"></body>`, 1},
		{"img with empty alt counts as missing", `<body><img src="a.png" alt=""></body>`, 1},
		{"img with whitespace-only alt counts as missing", `<body><img src="a.png" alt="   "></body>`, 1},
		{"uppercase ALT attribute is recognised", `<body><img src="a.png" ALT="A cat"></body>`, 0},
		{"self-closing img counted", `<body><img src="a.png" /></body>`, 1},
		{"several images tallied", `<body><img src="a"><img src="b" alt="b"><img src="c"></body>`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractPageSignals(strings.NewReader(tc.html), false).ImagesMissingAlt; got != tc.want {
				t.Fatalf("ImagesMissingAlt = %d, want %d", got, tc.want)
			}
		})
	}
}

// Mixed content only exists on an https page: the same markup served over
// http is not a browser-blocking problem, so the flag must gate the count.
func TestInsecureRefsOnlyCountedOnHTTPSPages(t *testing.T) {
	doc := `<body>
  <img src="http://cdn.example.dk/a.png" alt="a">
  <script src="http://cdn.example.dk/a.js"></script>
  <iframe src="http://cdn.example.dk/frame"></iframe>
  <img src="https://cdn.example.dk/b.png" alt="b">
  <script src="//cdn.example.dk/proto-relative.js"></script>
  <img src="/relative.png" alt="c">
</body>`

	onHTTPS := ExtractPageSignals(strings.NewReader(doc), true)
	if onHTTPS.InsecureRefs != 3 {
		t.Errorf("https page InsecureRefs = %d, want 3", onHTTPS.InsecureRefs)
	}

	onHTTP := ExtractPageSignals(strings.NewReader(doc), false)
	if onHTTP.InsecureRefs != 0 {
		t.Errorf("http page InsecureRefs = %d, want 0", onHTTP.InsecureRefs)
	}
}

// Scheme comparison must be case-insensitive: "HTTP://" is a legal, if
// unusual, way to write an insecure reference and still gets blocked.
func TestInsecureRefsSchemeCasing(t *testing.T) {
	cases := []struct {
		name string
		html string
		want int
	}{
		{"lowercase http", `<body><img src="http://x/a.png" alt="a"></body>`, 1},
		{"uppercase HTTP", `<body><img src="HTTP://x/a.png" alt="a"></body>`, 1},
		{"https is secure", `<body><img src="https://x/a.png" alt="a"></body>`, 0},
		{"protocol-relative inherits the page scheme", `<body><img src="//x/a.png" alt="a"></body>`, 0},
		{"data URI is not an insecure ref", `<body><img src="data:image/gif;base64,AA" alt="a"></body>`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractPageSignals(strings.NewReader(tc.html), true).InsecureRefs; got != tc.want {
				t.Fatalf("InsecureRefs = %d, want %d", got, tc.want)
			}
		})
	}
}

// Soft-404 detection reads the title only. Matching body text produced false
// positives on pages that merely discuss 404s — a documented design decision
// worth locking down.
func TestSoftNotFoundFromTitleOnly(t *testing.T) {
	cases := []struct {
		name string
		html string
		want bool
	}{
		{"normal title", `<title>Filtre til ventilation</title>`, false},
		{"no title at all", `<body>404 not found</body>`, false},
		{"404 in the title", `<title>404</title>`, true},
		{"Not Found in the title", `<title>Not Found</title>`, true},
		{"Page Not Found in the title", `<title>Page Not Found | Example</title>`, true},
		{"page does not exist", `<title>The page does not exist</title>`, true},
		{"page doesn't exist with apostrophe", `<title>Page doesn't exist</title>`, true},
		{"Danish ikke fundet", `<title>Siden blev ikke fundet</title>`, true},
		{"Danish siden findes ikke", `<title>Siden findes ikke</title>`, true},
		{"uppercase NOT FOUND", `<title>NOT FOUND</title>`, true},
		{"body mentions 404 but title is fine",
			`<title>How we handle errors</title><body>Our 404 page is not found by users</body>`, false},
		{"a product literally numbered 404 is a known false positive",
			`<title>Filter model 404</title>`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractPageSignals(strings.NewReader(tc.html), false).SoftNotFound; got != tc.want {
				t.Fatalf("SoftNotFound = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLooksLikeNotFound(t *testing.T) {
	cases := []struct {
		title string
		want  bool
	}{
		{"", false},
		{"   ", false},
		{"404", true},
		{"Home", false},
		{"  Not Found  ", true},
		{"nOt FoUnD", true},
		{"Confounded", false}, // "found" alone must not match
	}
	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			if got := looksLikeNotFound(tc.title); got != tc.want {
				t.Fatalf("looksLikeNotFound(%q) = %v, want %v", tc.title, got, tc.want)
			}
		})
	}
}

// The extractor must never error or panic. A truncated response is the normal
// case when a connection drops mid-body, and whatever was parsed before the
// break is more useful than nothing.
func TestMalformedAndTruncatedDocuments(t *testing.T) {
	cases := []struct {
		name string
		html string
	}{
		{"empty input", ""},
		{"plain text, not HTML", "just some text"},
		{"unclosed tags", `<html><head><title>Shop`},
		{"truncated mid-attribute", `<html><body><img src="a.png" al`},
		{"stray closing tags", `</div></body></html>`},
		{"nested unclosed h1s", `<h1><h1><h1>`},
		{"binary-looking bytes", "\x00\x01\x02<title>x</title>\xff"},
		{"only a doctype", `<!doctype html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractPageSignals(strings.NewReader(tc.html), true) // must not panic
			if got.TitleLength != len([]rune(got.Title)) {
				t.Errorf("TitleLength %d inconsistent with Title %q", got.TitleLength, got.Title)
			}
		})
	}
}

// A title truncated by a dropped connection still yields the text that arrived,
// which is what makes partial parsing worthwhile.
func TestTruncatedDocumentKeepsWhatWasRead(t *testing.T) {
	got := ExtractPageSignals(strings.NewReader(`<html><head><title>Shop</title><meta name="descrip`), false)
	if got.Title != "Shop" {
		t.Fatalf("Title = %q, want %q from the part that arrived", got.Title, "Shop")
	}
}

// isHTML gates whether the body is parsed at all. Getting the charset suffix
// wrong would mean no page signals for the majority of real responses, which
// send "text/html; charset=utf-8".
func TestIsHTML(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"text/html", true},
		{"text/html; charset=utf-8", true},
		{"text/html;charset=UTF-8", true},
		{"TEXT/HTML", true},
		{"  text/html  ", true},
		{"application/xhtml+xml", true},
		{"application/xhtml+xml; charset=utf-8", true},
		{"text/plain", false},
		{"application/json", false},
		{"image/png", false},
		{"text/xml", false},
		{"", false},
		{"text/htmlish", false},
	}
	for _, tc := range cases {
		t.Run(tc.ct, func(t *testing.T) {
			if got := isHTML(tc.ct); got != tc.want {
				t.Fatalf("isHTML(%q) = %v, want %v", tc.ct, got, tc.want)
			}
		})
	}
}

// extractDomain feeds the per-domain concurrency limiter. If it returned the
// full URL, every URL would be its own "domain" and the politeness limit would
// never apply.
func TestExtractDomain(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"simple https", "https://example.dk/page", "example.dk"},
		{"no path", "https://example.dk", "example.dk"},
		{"with port", "https://example.dk:8443/page", "example.dk"},
		{"http scheme", "http://example.dk/a/b/c", "example.dk"},
		{"subdomain preserved", "https://shop.example.dk/x", "shop.example.dk"},
		{"query string ignored", "https://example.dk/?a=b", "example.dk"},
		{"no scheme returns the input", "example.dk/page", "example.dk/page"},
		{"empty string", "", ""},
		{"userinfo is not stripped", "https://user@example.dk/", "user@example.dk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractDomain(tc.url); got != tc.want {
				t.Fatalf("extractDomain(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}
