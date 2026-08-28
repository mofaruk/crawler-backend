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
