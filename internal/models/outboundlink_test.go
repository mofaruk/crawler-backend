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

// Blocked and Broken must never both be true: a link is reported in one list
// or the other, and a link appearing in both would be a contradiction the
// customer has to resolve.
func TestBlockedAndBrokenAreExclusive(t *testing.T) {
	checked := time.Now()

	for status := 100; status < 1000; status++ {
		link := OutboundLink{StatusCode: status, CheckedAt: &checked}

		if link.Blocked() && link.Broken() {
			t.Errorf("status %d is reported as both blocked and broken", status)
		}
	}
}

// The codes that made nlphuset.dk's report unreadable. Each is a refusal to
// answer a crawler, not a missing page.
func TestBlockedRecognisesTheObservedChallenges(t *testing.T) {
	checked := time.Now()

	for _, status := range []int{403, 429, 451, 454, 455, 999} {
		link := OutboundLink{StatusCode: status, CheckedAt: &checked}
		if !link.Blocked() {
			t.Errorf("status %d should be reported as blocked", status)
		}
	}

	for _, status := range []int{200, 301, 404, 410, 500} {
		link := OutboundLink{StatusCode: status, CheckedAt: &checked}
		if link.Blocked() {
			t.Errorf("status %d should not be reported as blocked", status)
		}
	}
}

// A transport error means nothing answered at all, so there was no refusal to
// read. Those belong in the broken list, and the repository query relies on
// this to keep the two lists disjoint.
func TestTransportErrorIsBrokenNotBlocked(t *testing.T) {
	checked := time.Now()
	link := OutboundLink{Error: "timed out", CheckedAt: &checked}

	if link.Blocked() {
		t.Error("a link that never answered was reported as blocked rather than broken")
	}
	if !link.Broken() {
		t.Error("a link that timed out was not reported as broken")
	}
}

// Nothing is blocked before it has been checked.
func TestUncheckedLinkIsNotBlocked(t *testing.T) {
	if (OutboundLink{StatusCode: 999}).Blocked() {
		t.Error("a link with no check recorded was reported as blocked")
	}
}
