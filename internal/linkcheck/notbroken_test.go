package linkcheck

import "testing"

// NotBrokenStatuses feeds a database query that cannot call BotBlocked. If the
// two ever diverge, the query silently reports links the checker cleared.
func TestNotBrokenStatusesMatchesBotBlocked(t *testing.T) {
	listed := make(map[int]bool)
	for _, status := range NotBrokenStatuses() {
		listed[status] = true
	}

	for status := 400; status < 1000; status++ {
		if listed[status] != BotBlocked(status) {
			t.Errorf("status %d: in NotBrokenStatuses = %v, BotBlocked = %v",
				status, listed[status], BotBlocked(status))
		}
	}
}

// The codes that actually caused the false reports.
func TestNotBrokenStatusesCoversObservedChallenges(t *testing.T) {
	listed := make(map[int]bool)
	for _, status := range NotBrokenStatuses() {
		listed[status] = true
	}

	for _, status := range []int{403, 429, 451, 454, 455, 999} {
		if !listed[status] {
			t.Errorf("status %d is a bot challenge but is not excluded from broken links", status)
		}
	}

	for _, status := range []int{404, 410, 500, 503} {
		if listed[status] {
			t.Errorf("status %d is genuinely broken but was excluded", status)
		}
	}
}
