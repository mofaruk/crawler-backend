package crawler

import (
	"sort"
	"testing"
)

// WordPress generates six to eight sizes of every upload while a visitor loads
// one or two. Keeping the largest pair covers a desktop and a mobile viewport
// and drops thumbnails that are rarely requested — on a real customer site
// that is 6,364 URLs instead of 13,209.
func TestTopVariantsKeepsTheTwoLargest(t *testing.T) {
	urls := []string{
		"https://s.dk/u/photo-100x100.jpg",
		"https://s.dk/u/photo-247x247.jpg",
		"https://s.dk/u/photo-768x768.jpg",
		"https://s.dk/u/photo-1024x1024.jpg",
		"https://s.dk/u/photo-150x150.jpg",
	}

	got := FilterAssets(urls, "top_variants")
	sort.Strings(got)

	want := []string{
		"https://s.dk/u/photo-1024x1024.jpg",
		"https://s.dk/u/photo-768x768.jpg",
	}

	if len(got) != 2 {
		t.Fatalf("got %d URLs, want 2: %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("URL %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// An image with no size suffix is the original: there is no larger version to
// prefer, so it is always warmed.
func TestTopVariantsAlwaysKeepsOriginals(t *testing.T) {
	urls := []string{
		"https://s.dk/u/photo.jpg",
		"https://s.dk/u/photo-100x100.jpg",
		"https://s.dk/logo.svg",
		"https://s.dk/favicon.ico",
	}

	got := FilterAssets(urls, "top_variants")

	for _, want := range []string{"https://s.dk/u/photo.jpg", "https://s.dk/logo.svg", "https://s.dk/favicon.ico"} {
		if !contains(got, want) {
			t.Errorf("missing %q from %v", want, got)
		}
	}
}

// Each upload competes against its own siblings, not another image's — or one
// large photo would crowd out every other image on the page.
func TestTopVariantsGroupsPerImage(t *testing.T) {
	urls := []string{
		"https://s.dk/u/a-2000x2000.jpg",
		"https://s.dk/u/a-100x100.jpg",
		"https://s.dk/u/b-300x300.jpg",
		"https://s.dk/u/b-150x150.jpg",
	}

	got := FilterAssets(urls, "top_variants")

	for _, want := range []string{"https://s.dk/u/a-2000x2000.jpg", "https://s.dk/u/b-300x300.jpg"} {
		if !contains(got, want) {
			t.Errorf("missing %q — each image keeps its own largest: %v", want, got)
		}
	}
}

// A crawl must warm the same files every round, or the cache report moves
// around for reasons unrelated to the site.
func TestTopVariantsIsDeterministic(t *testing.T) {
	urls := []string{
		"https://s.dk/u/p-400x300.jpg", // same area as the next
		"https://s.dk/u/p-300x400.jpg",
		"https://s.dk/u/p-100x100.jpg",
	}

	first := FilterAssets(urls, "top_variants")
	for i := 0; i < 5; i++ {
		got := FilterAssets(urls, "top_variants")
		if len(got) != len(first) {
			t.Fatalf("run %d returned %d URLs, first returned %d", i, len(got), len(first))
		}
		sort.Strings(got)
		sort.Strings(first)
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d differs: %v vs %v", i, got, first)
			}
		}
	}
}

func TestFilterAssetsModes(t *testing.T) {
	urls := []string{
		"https://s.dk/u/photo.jpg",
		"https://s.dk/u/photo-100x100.jpg",
		"https://s.dk/u/photo-800x800.jpg",
		"https://s.dk/u/photo-1200x1200.jpg",
		"https://s.dk/u/photo.avif",
		"https://s.dk/css/app.css",
		"https://s.dk/js/app.js",
		"https://s.dk/fonts/inter.woff2",
	}

	cases := map[string]struct{ min, max int }{
		"none":         {0, 0},
		"top_variants": {1, 5}, // originals plus the two largest variants
		"images":       {5, 5}, // every image, no css/js/fonts
		"all":          {8, 8},
	}

	for mode, want := range cases {
		got := FilterAssets(urls, mode)
		if len(got) < want.min || len(got) > want.max {
			t.Errorf("mode %q kept %d URLs, want %d..%d: %v", mode, len(got), want.min, want.max, got)
		}
	}
}

// AVIF and WebP are what a modern browser is actually served, so warming only
// the JPEG warms a file most visitors never request.
func TestFilterAssetsTreatsNextGenFormatsAsImages(t *testing.T) {
	urls := []string{"https://s.dk/u/p.avif", "https://s.dk/u/p.webp", "https://s.dk/css/app.css"}

	got := FilterAssets(urls, "images")

	if len(got) != 2 {
		t.Fatalf("got %v, want both next-gen images and no stylesheet", got)
	}
}

// A CDN appending ?v=123 must not defeat the type check.
func TestFilterAssetsIgnoresQueryStrings(t *testing.T) {
	got := FilterAssets([]string{"https://s.dk/u/p.jpg?v=123", "https://s.dk/js/a.js?ver=6"}, "images")

	if len(got) != 1 || got[0] != "https://s.dk/u/p.jpg?v=123" {
		t.Errorf("got %v, want just the image", got)
	}
}

// An unknown or empty mode must land on the default rather than warming
// everything or nothing by accident.
func TestFilterAssetsUnknownModeUsesDefault(t *testing.T) {
	urls := []string{"https://s.dk/u/p-100x100.jpg", "https://s.dk/u/p-800x800.jpg", "https://s.dk/css/a.css"}

	for _, mode := range []string{"", "nonsense", "TOP_VARIANTS"} {
		if got := FilterAssets(urls, mode); contains(got, "https://s.dk/css/a.css") {
			t.Errorf("mode %q kept a stylesheet; the default is images only: %v", mode, got)
		}
	}
}

// The mode may arrive from config, a database row or an API payload. An "ALL"
// falling through to the default would quietly stop warming stylesheets for a
// site that explicitly asked for everything.
func TestFilterAssetsModeIsCaseInsensitive(t *testing.T) {
	urls := []string{"https://s.dk/u/p.jpg", "https://s.dk/css/a.css"}

	for _, mode := range []string{"ALL", "All", " all ", "all"} {
		if got := FilterAssets(urls, mode); len(got) != 2 {
			t.Errorf("mode %q kept %d URLs, want both: %v", mode, len(got), got)
		}
	}

	for _, mode := range []string{"NONE", "None"} {
		if got := FilterAssets(urls, mode); len(got) != 0 {
			t.Errorf("mode %q kept %d URLs, want none", mode, len(got))
		}
	}
}
