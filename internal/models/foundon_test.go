package models

import "testing"

// A broken image is fixed on the page that references it, not at its own
// address. Without the referrer an issue names a URL nobody can act on.
func TestIssuesCarryTheReferringPage(t *testing.T) {
	issues := ClassifyURL(URLState{
		URL:        "https://e.dk/wp-content/uploads/missing.png",
		StatusCode: 404,
		FoundOn:    "https://e.dk/vare/a-product/",
	}, nil)

	issue, ok := issueByKind(issues, "broken")
	if !ok {
		t.Fatal("a 404 must be reported")
	}
	if issue.FoundOn != "https://e.dk/vare/a-product/" {
		t.Fatalf("FoundOn = %q, want the referencing page", issue.FoundOn)
	}
}

// A URL the source listed was not found anywhere — it is the list. Reporting a
// referrer for it would invent one.
func TestAPageFromTheSourceHasNoReferrer(t *testing.T) {
	issues := ClassifyURL(URLState{
		URL:        "https://e.dk/gone/",
		StatusCode: 404,
	}, nil)

	issue, _ := issueByKind(issues, "broken")
	if issue.FoundOn != "" {
		t.Fatalf("FoundOn = %q, want empty for a URL that came from the source", issue.FoundOn)
	}
}
