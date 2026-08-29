// Package adaptive slows a crawl down when the site it is crawling starts
// struggling, and speeds it back up when the site recovers.
//
// The customer's requirement, in their words: "if it gets a slow response time
// for 10 URLs, need to instantly back off (pause). Otherwise it is starting to
// overload the site." And the part that makes it work: "the response time of
// the image URLs needs to be excluded here because they say nothing about
// whether the crawler is overloading the site or not."
//
// That exclusion is the whole design. An image is served from disk or the CDN
// edge and stays fast while PHP is drowning, so averaging images in would mask
// exactly the overload this exists to detect.
package adaptive

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Tuning. Deliberately unhurried: a crawler that reacts to every slow page
// oscillates, and one that never reacts is the problem being solved.
const (
	// Samples needed before the baseline is trusted. Below this the site has
	// not shown enough of itself to judge a change against.
	baselineSamples = 20

	// Consecutive slow pages before backing off. The customer asked for ten.
	slowStreakToBackOff = 10

	// Consecutive normal pages before speeding back up.
	fastStreakToRecover = 30

	// How much slower than baseline counts as struggling.
	slowMultiple = 2.0

	// Backing off halves the rate; recovery adds a quarter. Fast down, slow
	// up — the asymmetry is deliberate, because being too fast hurts the
	// customer's site and being too slow only costs time.
	backOffFactor = 0.5
	recoverFactor = 1.25

	// However bad it gets, keep crawling: a stalled crawl looks broken.
	minPagesPerHour = 360 // 0.1/sec

	sampleTTL = 6 * time.Hour
)

// Controller adjusts a crawl's page rate from the page response times it sees.
type Controller struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Controller {
	return &Controller{rdb: rdb}
}

func baselineKey(crawlingID string) string {
	return fmt.Sprintf("crawl:%s:rt:baseline", crawlingID)
}

func samplesKey(crawlingID string) string {
	return fmt.Sprintf("crawl:%s:rt:samples", crawlingID)
}

func slowStreakKey(crawlingID string) string {
	return fmt.Sprintf("crawl:%s:rt:slow_streak", crawlingID)
}

func fastStreakKey(crawlingID string) string {
	return fmt.Sprintf("crawl:%s:rt:fast_streak", crawlingID)
}

// Decision is what the controller concluded from one page.
type Decision struct {
	// Changed reports whether the rate should move.
	Changed bool
	// NewSpeed is the page rate in URLs per hour when Changed.
	NewSpeed int
	// Reason is human-readable, for the crawl log.
	Reason string
}

// Observe records one page's response time and decides whether the rate should
// change.
//
// Only pages may be passed here. Callers must exclude assets: an image's
// response time says nothing about whether the origin is under strain.
func (c *Controller) Observe(
	ctx context.Context,
	crawlingID string,
	responseMs int64,
	currentSpeed, configuredSpeed int,
) (Decision, error) {
	if responseMs <= 0 {
		return Decision{}, nil
	}

	// Build the baseline from the first samples, before judging anything
	// against it.
	count, err := c.rdb.Incr(ctx, samplesKey(crawlingID)).Result()
	if err != nil {
		return Decision{}, err
	}
	c.rdb.Expire(ctx, samplesKey(crawlingID), sampleTTL)

	if count <= baselineSamples {
		// A running mean is enough here: the baseline only needs to describe
		// the site's ordinary speed, not its distribution.
		if err := c.updateBaseline(ctx, crawlingID, responseMs, count); err != nil {
			return Decision{}, err
		}

		return Decision{}, nil
	}

	baseline, err := c.rdb.Get(ctx, baselineKey(crawlingID)).Float64()
	if err != nil || baseline <= 0 {
		return Decision{}, nil
	}

	if float64(responseMs) > baseline*slowMultiple {
		return c.recordSlow(ctx, crawlingID, responseMs, baseline, currentSpeed)
	}

	return c.recordFast(ctx, crawlingID, currentSpeed, configuredSpeed)
}

func (c *Controller) updateBaseline(ctx context.Context, crawlingID string, responseMs, count int64) error {
	current, _ := c.rdb.Get(ctx, baselineKey(crawlingID)).Float64()

	mean := current + (float64(responseMs)-current)/float64(count)

	return c.rdb.Set(ctx, baselineKey(crawlingID), mean, sampleTTL).Err()
}

func (c *Controller) recordSlow(
	ctx context.Context,
	crawlingID string,
	responseMs int64,
	baseline float64,
	currentSpeed int,
) (Decision, error) {
	// A recovery streak is broken by any slow page: the site is not better yet.
	c.rdb.Del(ctx, fastStreakKey(crawlingID))

	streak, err := c.rdb.Incr(ctx, slowStreakKey(crawlingID)).Result()
	if err != nil {
		return Decision{}, err
	}
	c.rdb.Expire(ctx, slowStreakKey(crawlingID), sampleTTL)

	if streak < slowStreakToBackOff {
		return Decision{}, nil
	}

	// Backed off, so start counting again from zero rather than halving on
	// every subsequent slow page.
	c.rdb.Del(ctx, slowStreakKey(crawlingID))

	newSpeed := int(float64(currentSpeed) * backOffFactor)
	if newSpeed < minPagesPerHour {
		newSpeed = minPagesPerHour
	}
	if newSpeed >= currentSpeed {
		return Decision{}, nil
	}

	return Decision{
		Changed:  true,
		NewSpeed: newSpeed,
		Reason: fmt.Sprintf(
			"slowed down: %d pages in a row took over %.0fms, about %.1fx this site's usual %.0fms",
			slowStreakToBackOff, baseline*slowMultiple, float64(responseMs)/baseline, baseline,
		),
	}, nil
}

func (c *Controller) recordFast(
	ctx context.Context,
	crawlingID string,
	currentSpeed, configuredSpeed int,
) (Decision, error) {
	c.rdb.Del(ctx, slowStreakKey(crawlingID))

	// Nothing to recover to.
	if currentSpeed >= configuredSpeed {
		return Decision{}, nil
	}

	streak, err := c.rdb.Incr(ctx, fastStreakKey(crawlingID)).Result()
	if err != nil {
		return Decision{}, err
	}
	c.rdb.Expire(ctx, fastStreakKey(crawlingID), sampleTTL)

	if streak < fastStreakToRecover {
		return Decision{}, nil
	}

	c.rdb.Del(ctx, fastStreakKey(crawlingID))

	newSpeed := int(float64(currentSpeed) * recoverFactor)
	if newSpeed > configuredSpeed {
		newSpeed = configuredSpeed
	}
	if newSpeed <= currentSpeed {
		return Decision{}, nil
	}

	return Decision{
		Changed:  true,
		NewSpeed: newSpeed,
		Reason:   "speeding back up: the site has been responding normally again",
	}, nil
}

// Cleanup removes a crawl's response-time state.
func (c *Controller) Cleanup(ctx context.Context, crawlingID string) error {
	return c.rdb.Del(ctx,
		baselineKey(crawlingID),
		samplesKey(crawlingID),
		slowStreakKey(crawlingID),
		fastStreakKey(crawlingID),
	).Err()
}
