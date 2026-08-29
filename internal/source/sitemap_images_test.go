package source

import (
	"strings"
	"testing"
)

// A sitemap lists pages, but on an image-heavy site most of what a visitor
// waits for is the images. Yoast and RankMath already publish them as
// <image:image> entries, so they were being fetched and discarded: on one real
// customer sitemap that is 954 image URLs the crawl never warmed.
func TestParseSitemapCollectsImages(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?>
	<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"
	        xmlns:image="http://www.google.com/schemas/sitemap-image/1.1">
		<url>
			<loc>https://example.dk/produkt/en</loc>
			<image:image><image:loc>https://example.dk/uploads/a.jpg</image:loc></image:image>
			<image:image><image:loc>https://example.dk/uploads/b.jpg</image:loc></image:image>
		</url>
		<url>
			<loc>https://example.dk/produkt/to</loc>
			<image:image><image:loc>https://example.dk/uploads/c.png</image:loc></image:image>
		</url>
	</urlset>`

	stats := &ParseStats{SourceType: "xml"}
	got, err := NewURLParser().parseSitemapReader(strings.NewReader(doc), 0, stats)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	want := []string{
		"https://example.dk/produkt/en",
		"https://example.dk/uploads/a.jpg",
		"https://example.dk/uploads/b.jpg",
		"https://example.dk/produkt/to",
		"https://example.dk/uploads/c.png",
	}

	if len(got) != len(want) {
		t.Fatalf("got %d URLs, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("URL %d = %q, want %q", i, got[i], want[i])
		}
	}

	if stats.XMLImageEntries != 3 {
		t.Errorf("XMLImageEntries = %d, want 3", stats.XMLImageEntries)
	}
}

// Go's XML decoder ignores namespaces, so <loc> and <image:loc> both decode as
// "loc". The page URL must still be the page, not the first image.
func TestParseSitemapKeepsPageAndImageApart(t *testing.T) {
	doc := `<urlset xmlns:image="http://www.google.com/schemas/sitemap-image/1.1">
		<url>
			<loc>https://example.dk/side</loc>
			<image:image><image:loc>https://example.dk/billede.jpg</image:loc></image:image>
		</url>
	</urlset>`

	stats := &ParseStats{SourceType: "xml"}
	got, _ := NewURLParser().parseSitemapReader(strings.NewReader(doc), 0, stats)

	if len(got) < 1 || got[0] != "https://example.dk/side" {
		t.Fatalf("first URL = %v, want the page itself", got)
	}
}

// A sitemap with no image entries must behave exactly as before.
func TestParseSitemapWithoutImagesIsUnchanged(t *testing.T) {
	doc := `<urlset>
		<url><loc>https://example.dk/en</loc></url>
		<url><loc>https://example.dk/to</loc></url>
	</urlset>`

	stats := &ParseStats{SourceType: "xml"}
	got, _ := NewURLParser().parseSitemapReader(strings.NewReader(doc), 0, stats)

	if len(got) != 2 {
		t.Errorf("got %d URLs, want 2: %v", len(got), got)
	}
	if stats.XMLImageEntries != 0 {
		t.Errorf("XMLImageEntries = %d, want 0", stats.XMLImageEntries)
	}
}

// Malformed image entries must not add junk to the crawl queue.
func TestParseSitemapSkipsUnusableImages(t *testing.T) {
	doc := `<urlset xmlns:image="http://www.google.com/schemas/sitemap-image/1.1">
		<url>
			<loc>https://example.dk/side</loc>
			<image:image><image:loc></image:loc></image:image>
			<image:image><image:loc>   </image:loc></image:image>
			<image:image><image:loc>not a url</image:loc></image:image>
			<image:image><image:loc>https://example.dk/god.jpg</image:loc></image:image>
		</url>
	</urlset>`

	stats := &ParseStats{SourceType: "xml"}
	got, _ := NewURLParser().parseSitemapReader(strings.NewReader(doc), 0, stats)

	if len(got) != 2 {
		t.Fatalf("got %v, want the page plus one valid image", got)
	}
	if stats.XMLImageEntries != 1 {
		t.Errorf("XMLImageEntries = %d, want 1", stats.XMLImageEntries)
	}
}

// The url_limit is a promise about how many requests a crawl makes, so images
// count toward it like anything else.
func TestParseSitemapImagesRespectTheLimit(t *testing.T) {
	doc := `<urlset xmlns:image="http://www.google.com/schemas/sitemap-image/1.1">
		<url>
			<loc>https://example.dk/en</loc>
			<image:image><image:loc>https://example.dk/a.jpg</image:loc></image:image>
			<image:image><image:loc>https://example.dk/b.jpg</image:loc></image:image>
		</url>
		<url>
			<loc>https://example.dk/to</loc>
			<image:image><image:loc>https://example.dk/c.jpg</image:loc></image:image>
		</url>
	</urlset>`

	stats := &ParseStats{SourceType: "xml"}
	got, _ := NewURLParser().parseSitemapReader(strings.NewReader(doc), 2, stats)

	if len(got) > 3 {
		t.Errorf("got %d URLs for a limit of 2; the limit must bound images too: %v", len(got), got)
	}
}
