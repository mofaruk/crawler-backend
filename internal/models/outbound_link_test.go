package models

import (
	"testing"
	"time"
)

// A customer's own Facebook page answering 400 to a bot must not be reported
// as a broken link. Social platforms and bot-protected sites reject any
// non-browser request while serving people perfectly well, and a report full
// of false alarms about links that work is worse than no report.
func TestBrokenIgnoresBotBlockingStatuses(t *testing.T) {
	checked := time.Now()

	for _, status := range []int{400, 403, 429, 451} {
		link := OutboundLink{StatusCode: status, CheckedAt: &checked}
		if link.Broken() {
			t.Errorf("status %d was reported broken; it usually means bot blocking", status)
		}
	}
}

// The statuses that genuinely mean the destination is gone must still count.
func TestBrokenReportsRealFailures(t *testing.T) {
	checked := time.Now()

	for _, status := range []int{404, 410, 500, 502, 503} {
		link := OutboundLink{StatusCode: status, CheckedAt: &checked}
		if !link.Broken() {
			t.Errorf("status %d was NOT reported broken", status)
		}
	}
}

// A transport failure is unambiguous: nothing answered at all.
func TestBrokenReportsTransportErrors(t *testing.T) {
	checked := time.Now()

	link := OutboundLink{Error: "domain does not resolve", CheckedAt: &checked}
	if !link.Broken() {
		t.Error("a link whose domain does not resolve was not reported broken")
	}
}

// A link that redirects still gets the visitor somewhere.
func TestBrokenIgnoresSuccessAndRedirects(t *testing.T) {
	checked := time.Now()

	for _, status := range []int{200, 204, 301, 302, 308} {
		link := OutboundLink{StatusCode: status, CheckedAt: &checked}
		if link.Broken() {
			t.Errorf("status %d was reported broken", status)
		}
	}
}

// Never checked is not the same as working: an unchecked link must not appear
// in a broken-link report either way.
func TestBrokenRequiresACheck(t *testing.T) {
	if (OutboundLink{StatusCode: 404}).Broken() {
		t.Error("an unchecked link was reported broken")
	}
}
