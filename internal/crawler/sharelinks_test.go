package crawler

import "testing"

// The share buttons that filled nlphuset.dk's report: 172 AddToAny entries
// against 22 real links, every one of them answering 400 to a bare request.
func TestExternalLinksSkipsShareButtons(t *testing.T) {
	page := "https://nlphuset.dk/gruppeterapi/"

	hrefs := []string{
		"https://www.addtoany.com/add_to/facebook?linkurl=https%3A%2F%2Fnlphuset.dk%2F",
		"https://www.addtoany.com/add_to/whatsapp?linkurl=https%3A%2F%2Fnlphuset.dk%2F",
		"https://www.facebook.com/sharer/sharer.php?u=https%3A%2F%2Fnlphuset.dk%2F",
		"https://twitter.com/intent/tweet?url=https%3A%2F%2Fnlphuset.dk%2F",
		"https://www.linkedin.com/shareArticle?url=https%3A%2F%2Fnlphuset.dk%2F",
		"https://pinterest.com/pin/create/button/?url=https%3A%2F%2Fnlphuset.dk%2F",
		"https://l.facebook.com/l.php?u=https%3A%2F%2Fpaludans.dk%2F",
	}

	if got := ExternalLinks(page, hrefs); len(got) != 0 {
		t.Errorf("share buttons were collected as outbound links: %v", got)
	}
}

// A link someone actually wrote to one of those sites is still a link. Only
// the widget endpoints are dropped, not the whole domain.
func TestExternalLinksKeepsRealLinksToShareHosts(t *testing.T) {
	page := "https://nlphuset.dk/gruppeterapi/"

	hrefs := []string{
		"https://www.linkedin.com/in/nlphuset/",
		"https://www.facebook.com/nlphuset",
		"https://twitter.com/someone/status/123456",
	}

	got := ExternalLinks(page, hrefs)
	if len(got) != len(hrefs) {
		t.Fatalf("got %d links, want %d: %v", len(got), len(hrefs), got)
	}
}

// Ordinary editorial links are unaffected.
func TestExternalLinksKeepsOrdinaryLinks(t *testing.T) {
	page := "https://nlphuset.dk/gruppeterapi/"

	hrefs := []string{
		"https://altomledelse.dk/pomodoroteknikken/",
		"https://christianhjortkjaer.dk/",
	}

	if got := ExternalLinks(page, hrefs); len(got) != 2 {
		t.Errorf("got %d links, want 2: %v", len(got), got)
	}
}
