package source

import "testing"

func TestValidateTargetURLRejectsInfrastructure(t *testing.T) {
	// Literal addresses only — no DNS, so the test is hermetic.
	blocked := []struct{ name, url string }{
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/"},
		{"loopback", "http://127.0.0.1:8088/sites"},
		{"loopback name", "http://localhost/admin"},
		{"private 10/8", "http://10.0.0.5/"},
		{"private 172.16/12", "http://172.16.0.1/"},
		{"private 192.168/16", "http://192.168.1.1/"},
		{"IPv6 loopback", "http://[::1]/"},
		{"carrier-grade NAT", "http://100.64.0.1/"},
		{"unspecified", "http://0.0.0.0/"},
		{"file scheme", "file:///etc/passwd"},
		{"gopher scheme", "gopher://127.0.0.1/"},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateTargetURL(c.url, false); err == nil {
				t.Fatalf("expected %s to be rejected, got nil", c.url)
			}
		})
	}
}

func TestValidateTargetURLAllowsPublic(t *testing.T) {
	for _, u := range []string{"http://93.184.216.34/", "https://8.8.8.8/"} {
		if err := ValidateTargetURL(u, false); err != nil {
			t.Fatalf("expected %s to be allowed, got %v", u, err)
		}
	}
}

// The escape hatch must actually work — local development points sources at
// host.docker.internal, which resolves to a private address.
func TestAllowPrivateBypassesTheCheck(t *testing.T) {
	if err := ValidateTargetURL("http://127.0.0.1:9999/sitemap.xml", true); err != nil {
		t.Fatalf("allowPrivate should bypass validation, got %v", err)
	}
}

// ALLOW_PRIVATE_TARGETS lets local development reach fixtures on
// host.docker.internal. It must not become a blanket "skip validation":
// before this was split out, `javascript:alert(1)` was accepted and stored as
// a site's base_url whenever the flag was on.
func TestAllowPrivateStillRejectsNonHTTPSchemes(t *testing.T) {
	rejected := []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"ftp://example.dk/pub",
		"gopher://example.dk",
		"",
		"not a url at all",
	}

	for _, raw := range rejected {
		if err := ValidateTargetURL(raw, true); err == nil {
			t.Errorf("ValidateTargetURL(%q, allowPrivate=true) = nil, want rejection", raw)
		}
	}
}

// The flag must still do its job: a private address is allowed through when
// it is set, which is the whole reason it exists.
func TestAllowPrivatePermitsPrivateAddresses(t *testing.T) {
	allowed := []string{
		"http://host.docker.internal:9999/sitemap.xml",
		"http://127.0.0.1:9999",
		"http://192.168.1.10",
	}

	for _, raw := range allowed {
		if err := ValidateTargetURL(raw, true); err != nil {
			t.Errorf("ValidateTargetURL(%q, allowPrivate=true) = %v, want nil", raw, err)
		}
	}
}
