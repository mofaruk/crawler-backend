package crawler

import (
	"strings"
	"testing"
)

// Internal links are already visited by the crawl itself, so including them
// here would duplicate findings and double the checking work.
func TestExternalLinksExcludesSameSite(t *testing.T) {
	got := ExternalLinks("https://billigfilter.dk/produkter", []string{
		"/kontakt",                         // relative
		"https://billigfilter.dk/om-os",    // absolute, same host
		"https://www.billigfilter.dk/blog", // www variant of the same site
		"//billigfilter.dk/protocol-relative",
		"https://leverandor.dk/produkt", // external
	})

	if len(got) != 1 {
		t.Fatalf("got %d links, want 1: %+v", len(got), got)
	}
	if got[0].URL != "https://leverandor.dk/produkt" {
		t.Errorf("URL = %q, want the external one", got[0].URL)
	}
	if got[0].FoundOn != "https://billigfilter.dk/produkter" {
		t.Errorf("FoundOn = %q, want the page it was found on", got[0].FoundOn)
	}
}

// Anything that cannot be fetched is not a link to check.
func TestExternalLinksSkipsUnfetchable(t *testing.T) {
	got := ExternalLinks("https://example.dk/", []string{
		"mailto:hej@example.dk",
		"tel:+4512345678",
		"javascript:void(0)",
		"#section",
		"",
		"   ",
		"data:text/plain,hello",
		"ftp://files.example.com/x",
	})

	if len(got) != 0 {
		t.Errorf("got %d links, want none: %+v", len(got), got)
	}
}

// A nav bar linking the same partner eight times is one destination, not
// eight requests against their server.
func TestExternalLinksDeduplicatesPerPage(t *testing.T) {
	got := ExternalLinks("https://example.dk/", []string{
		"https://partner.dk/",
		"https://partner.dk/",
		"https://partner.dk/#top",     // fragment only
		"https://partner.dk/#contact", // different fragment, same page
	})

	if len(got) != 1 {
		t.Fatalf("got %d links, want 1: %+v", len(got), got)
	}
	if strings.Contains(got[0].URL, "#") {
		t.Errorf("URL %q still carries a fragment", got[0].URL)
	}
}

// A subdomain is treated as external on purpose: a broken link to
// shop.example.dk is still worth reporting to the owner of example.dk.
func TestExternalLinksTreatsSubdomainsAsExternal(t *testing.T) {
	got := ExternalLinks("https://example.dk/", []string{"https://shop.example.dk/vare"})

	if len(got) != 1 {
		t.Fatalf("got %d links, want the subdomain reported: %+v", len(got), got)
	}
}

// Relative links must resolve against the page, not the site root, or a link
// two levels deep points at the wrong place.
func TestExternalLinksResolvesRelativePaths(t *testing.T) {
	got := ExternalLinks("https://example.dk/blog/2026/post", []string{
		"../../andet",             // internal after resolving — excluded
		"https://ekstern.dk/side", // external
	})

	if len(got) != 1 || got[0].URL != "https://ekstern.dk/side" {
		t.Fatalf("got %+v, want only the external link", got)
	}
}

func TestExternalLinksHandlesUnusablePageURL(t *testing.T) {
	for _, page := range []string{"", "not a url", "mailto:x@y.dk"} {
		if got := ExternalLinks(page, []string{"https://ekstern.dk/"}); got != nil {
			t.Errorf("page %q returned %+v, want nil", page, got)
		}
	}
}

// The parser has to actually collect hrefs for any of the above to matter.
func TestExtractPageSignalsCollectsLinks(t *testing.T) {
	html := `<html><body>
		<a href="/internal">internal</a>
		<a href="https://ekstern.dk/side">external</a>
		<a>no href</a>
		<a href="">empty</a>
	</body></html>`

	got := ExtractPageSignals(strings.NewReader(html), true)

	if len(got.Links) != 2 {
		t.Fatalf("Links = %v, want the two non-empty hrefs", got.Links)
	}
}
