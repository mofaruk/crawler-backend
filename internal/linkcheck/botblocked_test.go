package linkcheck

import "testing"

// A link is only worth reporting if a person following it would also fail.
// Anything that comes back from bot protection is a fact about us, not about
// the destination, and reporting it sends a customer to fix a page that works.
func TestBotBlockedSeparatesChallengesFromRealFailures(t *testing.T) {
	blocked := map[int]string{
		400: "some CDNs answer 400 to unrecognised clients",
		403: "the most common bot block",
		429: "rate limited, which says nothing about the page",
		451: "legal block, not a missing page",
		999: "LinkedIn's answer to anything but a signed-in browser",
		454: "seen from Cloudflare-fronted sites that load fine in a browser",
		455: "the same, from the same sites",
	}
	for code, why := range blocked {
		if !BotBlocked(code) {
			t.Errorf("BotBlocked(%d) = false, want true — %s", code, why)
		}
	}

	real := map[int]string{
		404: "the page is genuinely gone",
		410: "gone, deliberately",
		500: "the destination is broken",
		502: "the destination is broken",
		503: "the destination is down",
		200: "not an error at all",
		301: "a redirect, followed before this is reached",
	}
	for code, why := range real {
		if BotBlocked(code) {
			t.Errorf("BotBlocked(%d) = true, want false — %s", code, why)
		}
	}
}
