package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/webkonsulenterne/crawler-backend/internal/models"
)

// Redis key patterns:
//   crawl:{crawling_id}:pending    - LIST: pending URLs to crawl
//   crawl:{crawling_id}:processing - ZSET: URLs being processed (score = timestamp)
//   crawl:{crawling_id}:retry      - ZSET: URLs to retry (score = next retry timestamp)
//   crawl:{crawling_id}:dead       - LIST: dead letter queue (exceeded max retries)

const (
	processingTimeout = 5 * time.Minute // requeue if not completed within this time
)

type DistributedQueue struct {
	rdb *redis.Client
}

func NewDistributedQueue(rdb *redis.Client) *DistributedQueue {
	return &DistributedQueue{rdb: rdb}
}

// --- Key Helpers ---

func pendingKey(crawlingID string) string    { return fmt.Sprintf("crawl:%s:pending", crawlingID) }
func processingKey(crawlingID string) string { return fmt.Sprintf("crawl:%s:processing", crawlingID) }
func retryKey(crawlingID string) string      { return fmt.Sprintf("crawl:%s:retry", crawlingID) }
func deadKey(crawlingID string) string       { return fmt.Sprintf("crawl:%s:dead", crawlingID) }

// --- Enqueue ---

// EnqueueBatch pushes a batch of tasks into the pending queue using pipelining.
func (q *DistributedQueue) EnqueueBatch(ctx context.Context, crawlingID string, tasks []models.CrawlTask) error {
	if len(tasks) == 0 {
		return nil
	}

	pipe := q.rdb.Pipeline()
	key := pendingKey(crawlingID)

	for _, task := range tasks {
		data, err := json.Marshal(task)
		if err != nil {
			log.Error().Err(err).Str("url", task.URL).Msg("failed to marshal task")
			continue
		}
		pipe.LPush(ctx, key, data)
	}

	_, err := pipe.Exec(ctx)
	return err
}

// --- Dequeue ---

// Dequeue atomically moves a task from pending to processing.
// Uses RPOPLPUSH-equivalent via Lua for atomicity.
var dequeueScript = redis.NewScript(`
local pending = KEYS[1]
local processing = KEYS[2]
local now = tonumber(ARGV[1])

local task = redis.call('RPOP', pending)
if task then
    redis.call('ZADD', processing, now, task)
    return task
end
return nil
`)

func (q *DistributedQueue) Dequeue(ctx context.Context, crawlingID string) (*models.CrawlTask, error) {
	result, err := dequeueScript.Run(ctx, q.rdb,
		[]string{pendingKey(crawlingID), processingKey(crawlingID)},
		time.Now().Unix(),
	).Result()

	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var task models.CrawlTask
	if err := json.Unmarshal([]byte(result.(string)), &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// DequeueBatch dequeues up to `count` tasks atomically.
var dequeueBatchScript = redis.NewScript(`
local pending = KEYS[1]
local processing = KEYS[2]
local count = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local results = {}

for i = 1, count do
    local task = redis.call('RPOP', pending)
    if not task then
        break
    end
    redis.call('ZADD', processing, now, task)
    table.insert(results, task)
end

return results
`)

func (q *DistributedQueue) DequeueBatch(ctx context.Context, crawlingID string, count int) ([]models.CrawlTask, error) {
	results, err := dequeueBatchScript.Run(ctx, q.rdb,
		[]string{pendingKey(crawlingID), processingKey(crawlingID)},
		count, time.Now().Unix(),
	).StringSlice()

	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	tasks := make([]models.CrawlTask, 0, len(results))
	for _, r := range results {
		var task models.CrawlTask
		if err := json.Unmarshal([]byte(r), &task); err != nil {
			log.Error().Err(err).Msg("failed to unmarshal task from queue")
			continue
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// --- Acknowledge ---

// Ack removes a completed task from the processing set.
func (q *DistributedQueue) Ack(ctx context.Context, crawlingID string, task *models.CrawlTask) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return q.rdb.ZRem(ctx, processingKey(crawlingID), data).Err()
}

// --- Retry ---

// Retry moves a failed task to the retry queue with exponential backoff.
func (q *DistributedQueue) Retry(ctx context.Context, crawlingID string, task *models.CrawlTask) error {
	// Remove from processing
	origData, _ := json.Marshal(task)
	q.rdb.ZRem(ctx, processingKey(crawlingID), origData)

	task.Retries++

	if task.Retries > task.MaxRetries {
		return q.SendToDead(ctx, crawlingID, task)
	}

	// Exponential backoff: 2^retries seconds, max 300s
	backoff := time.Duration(1<<uint(task.Retries)) * time.Second
	if backoff > 300*time.Second {
		backoff = 300 * time.Second
	}
	retryAt := time.Now().Add(backoff)

	data, err := json.Marshal(task)
	if err != nil {
		return err
	}

	return q.rdb.ZAdd(ctx, retryKey(crawlingID), redis.Z{
		Score:  float64(retryAt.Unix()),
		Member: data,
	}).Err()
}

// --- Dead Letter ---

func (q *DistributedQueue) SendToDead(ctx context.Context, crawlingID string, task *models.CrawlTask) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return q.rdb.LPush(ctx, deadKey(crawlingID), data).Err()
}

// --- Recovery ---

// RequeueRetries moves tasks whose retry time has passed back to the pending queue.
var requeueRetryScript = redis.NewScript(`
local retry_key = KEYS[1]
local pending_key = KEYS[2]
local now = tonumber(ARGV[1])
local max = tonumber(ARGV[2])

local tasks = redis.call('ZRANGEBYSCORE', retry_key, '-inf', now, 'LIMIT', 0, max)
if #tasks > 0 then
    for _, task in ipairs(tasks) do
        redis.call('LPUSH', pending_key, task)
    end
    redis.call('ZREMRANGEBYSCORE', retry_key, '-inf', now)
end
return #tasks
`)

func (q *DistributedQueue) RequeueRetries(ctx context.Context, crawlingID string, maxCount int) (int, error) {
	result, err := requeueRetryScript.Run(ctx, q.rdb,
		[]string{retryKey(crawlingID), pendingKey(crawlingID)},
		time.Now().Unix(), maxCount,
	).Int()

	if err != nil && err != redis.Nil {
		return 0, err
	}
	return result, nil
}

// RequeueStale moves processing tasks that have timed out back to pending.
var requeueStaleScript = redis.NewScript(`
local processing_key = KEYS[1]
local pending_key = KEYS[2]
local cutoff = tonumber(ARGV[1])
local max = tonumber(ARGV[2])

local tasks = redis.call('ZRANGEBYSCORE', processing_key, '-inf', cutoff, 'LIMIT', 0, max)
if #tasks > 0 then
    for _, task in ipairs(tasks) do
        redis.call('LPUSH', pending_key, task)
    end
    redis.call('ZREMRANGEBYSCORE', processing_key, '-inf', cutoff)
end
return #tasks
`)

func (q *DistributedQueue) RequeueStale(ctx context.Context, crawlingID string) (int, error) {
	cutoff := time.Now().Add(-processingTimeout).Unix()
	result, err := requeueStaleScript.Run(ctx, q.rdb,
		[]string{processingKey(crawlingID), pendingKey(crawlingID)},
		cutoff, 1000,
	).Int()

	if err != nil && err != redis.Nil {
		return 0, err
	}
	return result, nil
}

// --- Queue Stats ---

type QueueStats struct {
	Pending    int64
	Processing int64
	Retry      int64
	Dead       int64
}

func (q *DistributedQueue) GetStats(ctx context.Context, crawlingID string) (*QueueStats, error) {
	pipe := q.rdb.Pipeline()
	pendingCmd := pipe.LLen(ctx, pendingKey(crawlingID))
	processingCmd := pipe.ZCard(ctx, processingKey(crawlingID))
	retryCmd := pipe.ZCard(ctx, retryKey(crawlingID))
	deadCmd := pipe.LLen(ctx, deadKey(crawlingID))

	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}

	return &QueueStats{
		Pending:    pendingCmd.Val(),
		Processing: processingCmd.Val(),
		Retry:      retryCmd.Val(),
		Dead:       deadCmd.Val(),
	}, nil
}

// --- Cleanup ---

func (q *DistributedQueue) DeleteQueue(ctx context.Context, crawlingID string) error {
	return q.rdb.Del(ctx,
		pendingKey(crawlingID),
		processingKey(crawlingID),
		retryKey(crawlingID),
		deadKey(crawlingID),
	).Err()
}

// QueueLength returns total remaining work.
func (q *DistributedQueue) QueueLength(ctx context.Context, crawlingID string) (int64, error) {
	stats, err := q.GetStats(ctx, crawlingID)
	if err != nil {
		return 0, err
	}
	return stats.Pending + stats.Processing + stats.Retry, nil
}

// PendingLen returns just the length of the pending list (cheap, single LLEN).
// Used by the dispatcher to avoid acquiring more rate-limit tokens than there
// are tasks ready to dispatch.
func (q *DistributedQueue) PendingLen(ctx context.Context, crawlingID string) (int64, error) {
	n, err := q.rdb.LLen(ctx, pendingKey(crawlingID)).Result()
	if err == redis.Nil {
		return 0, nil
	}
	return n, err
}
