package source

import (
	"context"
	"testing"
)

// The finder must honour the caller's policy, not assume private targets are
// allowed: with the guard on, a metadata address has to be refused before any
// request is made.
func TestFindSitemapsRespectsPrivatePolicy(t *testing.T) {
	p := NewURLParser()

	for _, target := range []string{
		"http://169.254.169.254",
		"http://127.0.0.1",
		"http://10.0.0.1",
	} {
		if _, err := p.FindSitemaps(context.Background(), target, "test", false); err == nil {
			t.Errorf("FindSitemaps(%q, allowPrivate=false) = nil error, want rejection", target)
		}
	}
}
