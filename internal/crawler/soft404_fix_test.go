package crawler

import (
	"strings"
	"testing"
)

// soft_404 is a critical-severity finding, so a "404" appearing in a product
// name must not trigger it. Matching the bare substring flagged legitimate
// pages like "Filter model 404 - BilligFilter" as broken; a real error page
// announces itself in a few words, a product title does not.
func TestSoftNotFoundIgnores404InsideWords(t *testing.T) {
	notErrors := []string{
		"Filter model 404 — BilligFilter",
		"Ventilation 4045 spare part",
		"Rapport 2404 om ventilation",
		"Model A404B",
		"Product 404X in stock",
	}

	for _, title := range notErrors {
		page := "<html><head><title>" + title + "</title></head><body><p>x</p></body></html>"
		if got := ExtractPageSignals(strings.NewReader(page), true); got.SoftNotFound {
			t.Errorf("title %q was flagged as a soft 404", title)
		}
	}
}

// A real error page still has to be caught, however it phrases itself.
func TestSoftNotFoundStillCatchesRealErrorPages(t *testing.T) {
	errors := []string{
		"404",
		"404 - Page Not Found",
		"Error 404",
		"404 | BilligFilter",
		"404 Not Found",
		"Page not found",
		"Siden findes ikke",
		"Ikke fundet",
	}

	for _, title := range errors {
		page := "<html><head><title>" + title + "</title></head><body><p>x</p></body></html>"
		if got := ExtractPageSignals(strings.NewReader(page), true); !got.SoftNotFound {
			t.Errorf("title %q was NOT flagged as a soft 404", title)
		}
	}
}
