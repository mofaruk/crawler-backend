package crawler

import (
	"strings"
	"testing"
)

// srcset is where the responsive variants live, and those are what a visitor
// on a phone actually downloads — a sitemap lists the original only.
func TestExtractPageSignalsCollectsSrcsetVariants(t *testing.T) {
	html := `<html><body>
		<img src="/uploads/photo.jpg"
		     srcset="/uploads/photo-247x247.jpg 247w, /uploads/photo-800x600.jpg 800w, /uploads/photo-1600x1200.jpg 1600w"
		     alt="x">
	</body></html>`

	got := ExtractPageSignals(strings.NewReader(html), true)

	for _, want := range []string{
		"/uploads/photo.jpg",
		"/uploads/photo-247x247.jpg",
		"/uploads/photo-800x600.jpg",
		"/uploads/photo-1600x1200.jpg",
	} {
		if !contains(got.Assets, want) {
			t.Errorf("missing %q from %v", want, got.Assets)
		}
	}
}

// A URL may contain a comma — CDNs emit them in query strings routinely
// (?resize=800,600). Splitting naively on commas truncates those into
// unfetchable fragments.
func TestParseSrcsetHandlesCommasInsideURLs(t *testing.T) {
	got := parseSrcset("https://cdn.dk/i.jpg?resize=800,600 800w, https://cdn.dk/i.jpg?resize=1600,1200 1600w")

	want := []string{
		"https://cdn.dk/i.jpg?resize=800,600",
		"https://cdn.dk/i.jpg?resize=1600,1200",
	}

	if len(got) != len(want) {
		t.Fatalf("got %d URLs, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("URL %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseSrcsetPlainCases(t *testing.T) {
	cases := map[string][]string{
		"":                             nil,
		"   ":                          nil,
		"a.jpg":                        {"a.jpg"},
		"a.jpg 1x, b.jpg 2x":           {"a.jpg", "b.jpg"},
		"a.jpg 400w,b.jpg 800w":        {"a.jpg", "b.jpg"},
		"  a.jpg 400w ,  b.jpg 800w  ": {"a.jpg", "b.jpg"},
	}

	for input, want := range cases {
		got := parseSrcset(input)
		if len(got) != len(want) {
			t.Errorf("parseSrcset(%q) = %v, want %v", input, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseSrcset(%q)[%d] = %q, want %q", input, i, got[i], want[i])
			}
		}
	}
}

// Lazy-loading themes leave src as a placeholder and put the real image in
// data-src, so warming src alone would warm a 1px spacer.
func TestExtractPageSignalsCollectsLazyLoadedImages(t *testing.T) {
	html := `<html><body>
		<img src="/placeholder.gif" data-src="/uploads/real.jpg"
		     data-srcset="/uploads/real-400x300.jpg 400w" alt="x">
	</body></html>`

	got := ExtractPageSignals(strings.NewReader(html), true)

	for _, want := range []string{"/uploads/real.jpg", "/uploads/real-400x300.jpg"} {
		if !contains(got.Assets, want) {
			t.Errorf("missing lazy-loaded %q from %v", want, got.Assets)
		}
	}
}

// Stylesheets, scripts, fonts and media are all fetched by a visitor and none
// appear in a sitemap.
func TestExtractPageSignalsCollectsNonImageAssets(t *testing.T) {
	html := `<html><head>
		<link rel="stylesheet" href="/css/app.css">
		<link rel="preload" href="/fonts/inter.woff2" as="font">
		<link rel="icon" href="/favicon.png">
		<link rel="canonical" href="https://example.dk/side">
		<script src="/js/app.js"></script>
	</head><body>
		<video poster="/video/cover.jpg"><source src="/video/clip.mp4"></video>
	</body></html>`

	got := ExtractPageSignals(strings.NewReader(html), true)

	for _, want := range []string{
		"/css/app.css", "/fonts/inter.woff2", "/favicon.png",
		"/js/app.js", "/video/cover.jpg", "/video/clip.mp4",
	} {
		if !contains(got.Assets, want) {
			t.Errorf("missing %q from %v", want, got.Assets)
		}
	}

	// The canonical link is metadata, not something to fetch.
	if contains(got.Assets, "https://example.dk/side") {
		t.Error("the canonical URL was collected as an asset")
	}
}

// data: URIs are the bytes themselves — nothing to fetch, and they can be huge.
func TestExtractPageSignalsSkipsUnfetchableAssets(t *testing.T) {
	html := `<html><body>
		<img src="data:image/gif;base64,R0lGODlhAQABAAAAACw=" alt="x">
		<img src="" alt="empty">
		<img src="   " alt="blank">
	</body></html>`

	if got := ExtractPageSignals(strings.NewReader(html), true); len(got.Assets) != 0 {
		t.Errorf("Assets = %v, want none", got.Assets)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
