package ratelimiter

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/webkonsulenterne/crawler-backend/internal/testsupport"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()

	return testsupport.Redis(t)
}

// Assets have their own budget so images, which an origin serves from disk or
// the CDN edge, are not held to the rate that exists to protect PHP.
func TestAssetBudgetIsSeparateFromPages(t *testing.T) {
	ctx := context.Background()
	rdb := testRedis(t)
	rl := NewDistributedRateLimiter(rdb)

	id := "test-asset-budget"
	defer rl.Cleanup(ctx, id)

	// 3,600 pages/hr (1/sec) and 36,000 assets/hr (10/sec).
	if err := rl.Init(ctx, id, 3600, 36000); err != nil {
		t.Fatal(err)
	}

	// Let both buckets fill.
	time.Sleep(1200 * time.Millisecond)

	pages, err := rl.Acquire(ctx, id, 50)
	if err != nil {
		t.Fatal(err)
	}

	assets, err := rl.AcquireAssets(ctx, id, 50)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("in ~1.2s: %d page tokens, %d asset tokens", pages, assets)

	if assets <= pages {
		t.Errorf("assets (%d) did not outpace pages (%d); the budgets are not separate", assets, pages)
	}
}

// A crawl started before assets had their own budget must behave exactly as it
// did: an asset speed of zero means assets draw on the page bucket.
func TestZeroAssetSpeedFallsBackToPageBudget(t *testing.T) {
	ctx := context.Background()
	rdb := testRedis(t)
	rl := NewDistributedRateLimiter(rdb)

	id := "test-asset-fallback"
	defer rl.Cleanup(ctx, id)

	if err := rl.Init(ctx, id, 3600, 0); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1200 * time.Millisecond)

	// Drain the page bucket, then confirm assets have nothing left either.
	if _, err := rl.Acquire(ctx, id, 100); err != nil {
		t.Fatal(err)
	}

	assets, err := rl.AcquireAssets(ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}

	if assets != 0 {
		t.Errorf("assets acquired %d tokens from an exhausted shared bucket", assets)
	}
}
