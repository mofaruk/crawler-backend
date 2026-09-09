package models

import (
	"testing"
	"time"

	"github.com/webkonsulenterne/crawler-backend/internal/linkcheck"
)

// The statuses nlphuset.dk's report was full of. Each one loads in a browser,
// and each was listed as a broken link because Broken() carried its own copy
// of the bot-blocked list and that copy never learned about them.
func TestBrokenIgnoresBotChallenges(t *testing.T) {
	checked := time.Now()

	cases := []struct {
		name   string
		status int
		want   bool
	}{
		{"linkedin answers 999 to anything but a signed-in browser", 999, false},
		{"simply.com bot protection answers 454", 454, false},
		{"and 455 to a request with no user agent", 455, false},
		{"403 is a block, not a missing page", 403, false},
		{"451 is a legal block, the page exists", 451, false},
		{"404 really is broken", 404, true},
		{"410 really is broken", 410, true},
		{"500 really is broken", 500, true},
		{"200 is not broken", 200, false},
		{"301 still gets the visitor somewhere", 301, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link := OutboundLink{StatusCode: tc.status, CheckedAt: &checked}
			if got := link.Broken(); got != tc.want {
				t.Errorf("status %d: Broken() = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// Broken() and the checker must agree. They disagreed in production: the
// checker reported "broken: 0" for a batch that /links/broken then listed as
// broken, because each held its own copy of the rule.
func TestBrokenAgreesWithBotBlocked(t *testing.T) {
	checked := time.Now()

	for status := 400; status < 1000; status++ {
		link := OutboundLink{StatusCode: status, CheckedAt: &checked}

		if want := !linkcheck.BotBlocked(status); link.Broken() != want {
			t.Errorf("status %d: Broken() = %v, BotBlocked() = %v — the two rules disagree",
				status, link.Broken(), linkcheck.BotBlocked(status))
		}
	}
}

// An unchecked link is not a broken one: nothing has established that yet.
func TestUncheckedLinkIsNotBroken(t *testing.T) {
	if (OutboundLink{StatusCode: 404}).Broken() {
		t.Error("a link with no check recorded was reported as broken")
	}
}

// A transport error is broken whatever the status field holds.
func TestTransportErrorIsBroken(t *testing.T) {
	checked := time.Now()
	link := OutboundLink{Error: "timed out", CheckedAt: &checked}

	if !link.Broken() {
		t.Error("a link that timed out was not reported as broken")
	}
}
