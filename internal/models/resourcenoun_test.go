package models

import "testing"

// A 404 has no useful Content-Type — the body is an error page, usually HTML —
// so a broken image is only ever seen with the wrong type. The URL decides.
func TestResourceNounNamesWhatTheURLIs(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		url         string
		want        string
	}{
		{"an image 404ing as an HTML error page is still an image", "text/html; charset=UTF-8", "https://e.dk/wp-content/uploads/a.png", "image"},
		{"served as an image", "image/jpeg", "https://e.dk/a", "image"},
		{"a stylesheet", "text/css", "https://e.dk/a.css", "stylesheet"},
		{"a script by type", "application/javascript", "https://e.dk/a", "script"},
		{"a script by extension when the type is an error page", "text/html", "https://e.dk/app.js", "script"},
		{"a font", "font/woff2", "https://e.dk/a", "font"},
		{"a font by extension", "text/html", "https://e.dk/x.woff2", "font"},
		{"video is a media file", "video/mp4", "https://e.dk/a", "media file"},
		{"a PDF", "text/html", "https://e.dk/doc.pdf", "PDF"},
		{"an ordinary page", "text/html; charset=UTF-8", "https://e.dk/about/", "page"},
		{"a page with no extension", "", "https://e.dk/about", "page"},
		{"an extension inside a query string is not the resource", "", "https://e.dk/search?q=a.png", "page"},
		{"but a real image with a query string still is", "", "https://e.dk/a.png?v=2", "image"},
		{"a dot in a directory is not an extension", "", "https://e.dk/v1.2/about", "page"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resourceNoun(tc.contentType, tc.url); got != tc.want {
				t.Fatalf("resourceNoun(%q, %q) = %q, want %q", tc.contentType, tc.url, got, tc.want)
			}
		})
	}
}

// The reported case: a 404ing .png was titled "Broken page", which sent people
// looking for a routing problem instead of a page referencing a missing image.
func TestBrokenImageIsNotCalledABrokenPage(t *testing.T) {
	issues := ClassifyURL(URLState{
		URL:         "https://elyxxa.com/wp-content/uploads/2025/04/ChatGPT-Image.png",
		StatusCode:  404,
		ContentType: "text/html; charset=UTF-8",
	}, nil)

	issue, ok := issueByKind(issues, "broken")
	if !ok {
		t.Fatal("a 404 must still be reported")
	}
	if issue.Title != "Broken image" {
		t.Fatalf("title = %q, want %q", issue.Title, "Broken image")
	}
}
