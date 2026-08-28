package dedup

import (
	"strings"
	"testing"
)

// The hash is the dedup key for an entire crawl. It must be stable across
// runs — a changed hash makes every previously-seen URL look new and doubles
// the work of a resumed crawl.
func TestHashURLIsStableAndCompact(t *testing.T) {
	const u = "https://example.dk/produkter/filter?size=large"

	first := HashURL(u)
	if first != HashURL(u) {
		t.Fatal("HashURL is not deterministic")
	}
	// 16 bytes rendered as hex. The width is a memory budget: at 10M URLs the
	// difference between 32 and 64 hex characters is ~300MB of Redis.
	if len(first) != 32 {
		t.Fatalf("hash length = %d, want 32 hex characters", len(first))
	}
	if strings.ToLower(first) != first {
		t.Fatalf("hash must be lowercase hex, got %q", first)
	}
	for _, r := range first {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("non-hex character %q in hash %q", r, first)
		}
	}
}

// URLs that differ in any way must hash differently, including in ways that
// are semantically equivalent to a browser — normalisation is deliberately
// not this function's job.
func TestHashURLDistinguishesSimilarURLs(t *testing.T) {
	urls := []string{
		"",
		"https://example.dk/a",
		"https://example.dk/a/",
		"https://example.dk/A",
		"http://example.dk/a",
		"https://example.dk/a?x=1",
		"https://example.dk/a#frag",
		"https://www.example.dk/a",
	}

	seen := map[string]string{}
	for _, u := range urls {
		h := HashURL(u)
		if prev, dup := seen[h]; dup {
			t.Fatalf("%q and %q collided on hash %s", prev, u, h)
		}
		seen[h] = u
	}
}

// The Redis key is namespaced per crawling job so two concurrent crawls of the
// same site cannot suppress each other's URLs.
func TestSeenKeyIsNamespacedPerCrawl(t *testing.T) {
	a := seenKey("6512f0a1b2c3d4e5f6a7b8c9")
	b := seenKey("aaaabbbbccccddddeeeeffff")

	if a == b {
		t.Fatal("two crawls must not share a dedup key")
	}
	if !strings.HasPrefix(a, "crawl:") || !strings.HasSuffix(a, ":seen") {
		t.Fatalf("unexpected key shape: %q", a)
	}
	if !strings.Contains(a, "6512f0a1b2c3d4e5f6a7b8c9") {
		t.Fatalf("key %q does not contain the crawling id", a)
	}
}
