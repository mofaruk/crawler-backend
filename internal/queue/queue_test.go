package queue

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/webkonsulenterne/crawler-backend/internal/models"
	"github.com/webkonsulenterne/crawler-backend/internal/testsupport"
)

// newTestQueue dials the Redis given by REDIS_ADDR (default localhost:6382,
// the port docker-compose publishes) and skips the test when it is absent, so
// the suite stays runnable without infrastructure.
func newTestQueue(t *testing.T) (*DistributedQueue, *redis.Client) {
	t.Helper()

	rdb := testsupport.Redis(t)

	return NewDistributedQueue(rdb), rdb
}

func task(url string, retries, max int) models.CrawlTask {
	return models.CrawlTask{CrawlingID: "t", SiteID: "s", URL: url, Retries: retries, MaxRetries: max}
}

// Dequeue pops with RPOP, so EnqueueBatch must RPUSH to stay FIFO. LPUSH here
// made the queue LIFO and starved the earliest-enqueued URLs.
func TestEnqueueBatchIsFIFO(t *testing.T) {
	q, rdb := newTestQueue(t)
	ctx := context.Background()
	id := "test-fifo"
	t.Cleanup(func() { _ = q.DeleteQueue(ctx, id); _ = rdb.Close() })
	_ = q.DeleteQueue(ctx, id)

	if err := q.EnqueueBatch(ctx, id, []models.CrawlTask{
		task("http://a", 0, 3), task("http://b", 0, 3), task("http://c", 0, 3),
	}); err != nil {
		t.Fatalf("EnqueueBatch: %v", err)
	}

	for _, want := range []string{"http://a", "http://b", "http://c"} {
		got, err := q.Dequeue(ctx, id)
		if err != nil {
			t.Fatalf("Dequeue: %v", err)
		}
		if got == nil || got.URL != want {
			t.Fatalf("got %v, want %s", got, want)
		}
	}
}

// Retry must report dead=true only once the retry chain is exhausted, so the
// caller counts a URL as failed exactly once instead of on every attempt.
func TestRetryReportsDeadOnlyAfterMaxRetries(t *testing.T) {
	q, rdb := newTestQueue(t)
	ctx := context.Background()
	id := "test-retry"
	t.Cleanup(func() { _ = q.DeleteQueue(ctx, id); _ = rdb.Close() })
	_ = q.DeleteQueue(ctx, id)

	tk := task("http://x", 0, 2)
	for attempt := 1; attempt <= 2; attempt++ {
		dead, err := q.Retry(ctx, id, &tk)
		if err != nil {
			t.Fatalf("Retry %d: %v", attempt, err)
		}
		if dead {
			t.Fatalf("attempt %d: dead=true too early (retries=%d, max=%d)", attempt, tk.Retries, tk.MaxRetries)
		}
	}

	dead, err := q.Retry(ctx, id, &tk)
	if err != nil {
		t.Fatalf("final Retry: %v", err)
	}
	if !dead {
		t.Fatalf("expected dead=true once retries (%d) exceed max (%d)", tk.Retries, tk.MaxRetries)
	}

	stats, err := q.GetStats(ctx, id)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.Dead != 1 {
		t.Fatalf("dead-letter length = %d, want 1", stats.Dead)
	}
}

// RequeueRetries must remove exactly the members it moved. The old
// ZREMRANGEBYSCORE deleted every member below the cutoff, silently dropping
// the ones past LIMIT that were never requeued.
func TestRequeueRetriesDoesNotDropTasksBeyondLimit(t *testing.T) {
	q, rdb := newTestQueue(t)
	ctx := context.Background()
	id := "test-requeue"
	t.Cleanup(func() { _ = q.DeleteQueue(ctx, id); _ = rdb.Close() })
	_ = q.DeleteQueue(ctx, id)

	// 5 tasks all due now.
	due := float64(time.Now().Add(-time.Minute).Unix())
	for _, u := range []string{"a", "b", "c", "d", "e"} {
		tk := task("http://"+u, 1, 3)
		data, _ := marshalTask(tk)
		if err := rdb.ZAdd(ctx, retryKey(id), redis.Z{Score: due, Member: data}).Err(); err != nil {
			t.Fatalf("seed ZAdd: %v", err)
		}
	}

	// Requeue only 2 of them.
	moved, err := q.RequeueRetries(ctx, id, 2)
	if err != nil {
		t.Fatalf("RequeueRetries: %v", err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2", moved)
	}

	stats, err := q.GetStats(ctx, id)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.Pending != 2 {
		t.Fatalf("pending = %d, want 2", stats.Pending)
	}
	// The 3 unmoved tasks must still be in the retry set, not deleted.
	if stats.Retry != 3 {
		t.Fatalf("retry = %d, want 3 (the rest must not be dropped)", stats.Retry)
	}
}
