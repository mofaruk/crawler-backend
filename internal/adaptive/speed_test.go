package adaptive

import (
	"context"
	"testing"

	"github.com/webkonsulenterne/crawler-backend/internal/testsupport"
)

func testController(t *testing.T) (*Controller, string) {
	t.Helper()

	c := New(testsupport.Redis(t))
	id := "test-adaptive-" + t.Name()
	t.Cleanup(func() { c.Cleanup(context.Background(), id) })

	return c, id
}

// The site's own normal speed is the reference, so a naturally slow site is
// not throttled forever and a fast one is not hammered until it is already in
// trouble.
func TestBacksOffWhenPagesSlowRelativeToBaseline(t *testing.T) {
	ctx := context.Background()
	c, id := testController(t)

	const configured = 10800 // 3/sec

	// Teach it this site answers in about 200ms.
	for i := 0; i < baselineSamples; i++ {
		if _, err := c.Observe(ctx, id, 200, configured, configured); err != nil {
			t.Fatal(err)
		}
	}

	// Now it degrades. Nothing should happen until the streak is reached.
	var decision Decision
	for i := 0; i < slowStreakToBackOff; i++ {
		d, err := c.Observe(ctx, id, 900, configured, configured)
		if err != nil {
			t.Fatal(err)
		}
		if i < slowStreakToBackOff-1 && d.Changed {
			t.Fatalf("backed off after %d slow pages, want %d", i+1, slowStreakToBackOff)
		}
		decision = d
	}

	if !decision.Changed {
		t.Fatalf("did not back off after %d slow pages", slowStreakToBackOff)
	}
	if decision.NewSpeed != configured/2 {
		t.Errorf("new speed = %d, want half of %d", decision.NewSpeed, configured)
	}
	t.Logf("reason: %s", decision.Reason)
}

// A site that is simply slow, consistently, is not overloaded — throttling it
// would mean never crawling it properly.
func TestDoesNotBackOffOnAConsistentlySlowSite(t *testing.T) {
	ctx := context.Background()
	c, id := testController(t)

	const configured = 10800

	// This site always takes about 2 seconds.
	for i := 0; i < baselineSamples+slowStreakToBackOff*2; i++ {
		d, err := c.Observe(ctx, id, 2000, configured, configured)
		if err != nil {
			t.Fatal(err)
		}
		if d.Changed {
			t.Fatalf("backed off on a site whose normal speed is 2s (sample %d)", i)
		}
	}
}

// Recovery is slower than back-off on purpose: being too fast hurts the
// customer's site, being too slow only costs time.
func TestRecoversGraduallyOnceResponsesNormalise(t *testing.T) {
	ctx := context.Background()
	c, id := testController(t)

	const configured = 10800
	current := configured / 2 // as if already backed off once

	for i := 0; i < baselineSamples; i++ {
		c.Observe(ctx, id, 200, current, configured)
	}

	var recovered Decision
	for i := 0; i < fastStreakToRecover+1; i++ {
		d, err := c.Observe(ctx, id, 190, current, configured)
		if err != nil {
			t.Fatal(err)
		}
		if d.Changed {
			recovered = d
			break
		}
	}

	if !recovered.Changed {
		t.Fatal("never recovered after a long run of normal responses")
	}
	if recovered.NewSpeed <= current {
		t.Errorf("recovered to %d, which is not faster than %d", recovered.NewSpeed, current)
	}
	if recovered.NewSpeed > configured {
		t.Errorf("recovered to %d, above the configured %d", recovered.NewSpeed, configured)
	}
}

// A crawl that stalls looks broken, so there is a floor however bad it gets.
func TestNeverStopsCompletely(t *testing.T) {
	ctx := context.Background()
	c, id := testController(t)

	for i := 0; i < baselineSamples; i++ {
		c.Observe(ctx, id, 100, minPagesPerHour, minPagesPerHour)
	}

	for i := 0; i < slowStreakToBackOff; i++ {
		d, _ := c.Observe(ctx, id, 5000, minPagesPerHour, minPagesPerHour)
		if d.Changed && d.NewSpeed < minPagesPerHour {
			t.Fatalf("dropped to %d, below the floor of %d", d.NewSpeed, minPagesPerHour)
		}
	}
}

// One good page does not mean recovery; the streak has to start again.
func TestSlowStreakResetsOnANormalPage(t *testing.T) {
	ctx := context.Background()
	c, id := testController(t)

	const configured = 10800

	for i := 0; i < baselineSamples; i++ {
		c.Observe(ctx, id, 200, configured, configured)
	}

	// Nine slow, then one normal — that must not add up to a back-off.
	for i := 0; i < slowStreakToBackOff-1; i++ {
		c.Observe(ctx, id, 900, configured, configured)
	}
	c.Observe(ctx, id, 210, configured, configured)

	for i := 0; i < slowStreakToBackOff-1; i++ {
		d, _ := c.Observe(ctx, id, 900, configured, configured)
		if d.Changed {
			t.Fatalf("backed off at slow page %d after the streak was broken", i+1)
		}
	}
}
