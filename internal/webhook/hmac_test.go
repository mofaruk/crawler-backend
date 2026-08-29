package webhook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The signature is the only thing that lets a receiver distinguish a genuine
// webhook from anyone who learned the endpoint URL. It must be deterministic
// for a given (body, secret) pair and change when either does.
func TestComputeHMAC(t *testing.T) {
	body := []byte(`{"event":"crawl.completed"}`)

	sig := computeHMAC(body, "secret")
	if sig != computeHMAC(body, "secret") {
		t.Fatal("signature is not deterministic")
	}
	// HMAC-SHA256 rendered as hex.
	if len(sig) != 64 {
		t.Fatalf("signature length = %d, want 64 hex characters", len(sig))
	}
	if strings.ToLower(sig) != sig {
		t.Fatalf("signature must be lowercase hex, got %q", sig)
	}

	cases := []struct {
		name   string
		body   []byte
		secret string
	}{
		{"a different secret", body, "other-secret"},
		{"a changed body", []byte(`{"event":"crawl.failed"}`), "secret"},
		{"a single flipped byte", []byte(`{"event":"crawl.completeD"}`), "secret"},
		{"an empty body", nil, "secret"},
		{"an empty secret", body, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeHMAC(tc.body, tc.secret); got == sig {
				t.Fatalf("%s produced the same signature — the MAC does not cover it", tc.name)
			}
		})
	}
}

// The signature is computed over the marshalled JSON that is actually sent, so
// a receiver re-marshalling the payload must get the same bytes. Any field
// ordering instability would break every verification.
func TestPayloadMarshalsStably(t *testing.T) {
	p := WebhookPayload{
		Event:     EventCrawlProgress,
		Timestamp: time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC),
		Data:      map[string]interface{}{"crawled": 10, "total": 100, "site": "example.dk"},
	}

	first, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("marshalling is unstable:\n%s\n%s", first, again)
		}
	}
	if computeHMAC(first, "k") != computeHMAC([]byte(string(first)), "k") {
		t.Fatal("identical bytes produced different signatures")
	}
}
