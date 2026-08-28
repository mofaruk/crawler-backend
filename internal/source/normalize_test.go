package source

import "testing"

func TestNormalizeSiteURL(t *testing.T) {
	ok := []struct{ in, want string }{
		// The case this exists for: customers type a bare domain.
		{"billigfilter.dk", "https://billigfilter.dk"},
		{"  billigfilter.dk  ", "https://billigfilter.dk"},
		{"BilligFilter.DK", "https://billigfilter.dk"},
		{"billigfilter.dk/", "https://billigfilter.dk"},
		{"www.billigfilter.dk", "https://www.billigfilter.dk"},
		{"http://billigfilter.dk", "http://billigfilter.dk"},
		{"https://billigfilter.dk", "https://billigfilter.dk"},
		{"billigfilter.dk/shop", "https://billigfilter.dk/shop"},
		{"billigfilter.dk:8080", "https://billigfilter.dk:8080"},
		{"https://billigfilter.dk#top", "https://billigfilter.dk"},
		{"sub.domain.co.uk", "https://sub.domain.co.uk"},
	}
	for _, c := range ok {
		got, err := NormalizeSiteURL(c.in)
		if err != nil {
			t.Errorf("NormalizeSiteURL(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeSiteURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	bad := []string{
		"",
		"   ",
		"localhost",          // no dot: internal name or typo
		"mailto:x@y.dk",      // not a web address
		"ftp://example.dk",   // not crawlable over http(s)
		".example.dk",
		"example.dk.",
	}
	for _, in := range bad {
		if got, err := NormalizeSiteURL(in); err == nil {
			t.Errorf("NormalizeSiteURL(%q) = %q, want an error", in, got)
		}
	}
}
