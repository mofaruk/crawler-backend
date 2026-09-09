package repository

import "testing"

// NormalizeHost decides whether two site documents count as the same site for
// the concurrent-crawl guard, so every spelling that should collide is worth
// pinning: a missed one lets two accounts crawl one origin at double rate.
func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare domain", "example.com", "example.com"},
		{"https", "https://example.com", "example.com"},
		{"http", "http://example.com", "example.com"},
		{"www stripped", "https://www.example.com", "example.com"},
		{"trailing slash", "https://example.com/", "example.com"},
		{"path ignored", "https://example.com/shop/page", "example.com"},
		{"port ignored", "https://example.com:8443", "example.com"},
		{"uppercase", "HTTPS://WWW.Example.COM", "example.com"},
		{"surrounding space", "  https://example.com  ", "example.com"},
		{"query ignored", "https://example.com/?utm=1", "example.com"},
		{"subdomain kept", "https://shop.example.com", "shop.example.com"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeHost(tc.in); got != tc.want {
				t.Fatalf("NormalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A subdomain is a different origin and must not collide with its parent:
// crawling shop.example.com does not put load on example.com.
func TestNormalizeHostSeparatesSubdomains(t *testing.T) {
	if NormalizeHost("https://shop.example.com") == NormalizeHost("https://example.com") {
		t.Fatal("a subdomain must not compare equal to its parent domain")
	}
}
