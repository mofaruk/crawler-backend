package crawler

import "testing"

// Which budget a request is charged to depends on this, so a page classified
// as an asset would be crawled at the asset rate — and the page rate, which
// exists to protect the origin, would stop meaning anything.
func TestIsAssetURL(t *testing.T) {
	assets := []string{
		"https://s.dk/u/photo.jpg",
		"https://s.dk/u/photo-800x600.webp",
		"https://s.dk/u/photo.avif",
		"https://s.dk/logo.svg",
		"https://s.dk/css/app.css",
		"https://s.dk/js/app.js",
		"https://s.dk/fonts/inter.woff2",
		"https://s.dk/video/clip.mp4",
		"https://s.dk/docs/manual.pdf",
		"https://s.dk/favicon.ico",
		// A query string must not defeat the check.
		"https://s.dk/u/photo.jpg?v=123",
	}

	for _, u := range assets {
		if !IsAssetURL(u) {
			t.Errorf("%q was not treated as an asset", u)
		}
	}

	pages := []string{
		"https://s.dk/",
		"https://s.dk/produkter",
		"https://s.dk/vare/filter-a/",
		"https://s.dk/index.php",
		"https://s.dk/side.html",
		"https://s.dk/?s=search",
		// A directory containing a dot must not read as an extension.
		"https://s.dk/v1.2/produkter",
	}

	for _, u := range pages {
		if IsAssetURL(u) {
			t.Errorf("%q was treated as an asset", u)
		}
	}
}
