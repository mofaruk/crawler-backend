package ratelimiter

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Distributed token bucket rate limiter using Redis + Lua.
//
// Redis keys:
//   crawl:{crawling_id}:tokens        - remaining tokens (STRING)
//   crawl:{crawling_id}:tokens:refill  - last refill timestamp (STRING)
//
// The token bucket refills at `speed / 3600` tokens per second.
// Workers call Acquire() before each URL fetch.

type DistributedRateLimiter struct {
	rdb *redis.Client
}

func NewDistributedRateLimiter(rdb *redis.Client) *DistributedRateLimiter {
	return &DistributedRateLimiter{rdb: rdb}
}

func tokensKey(crawlingID string) string     { return fmt.Sprintf("crawl:%s:tokens", crawlingID) }
func refillKey(crawlingID string) string     { return fmt.Sprintf("crawl:%s:tokens:refill", crawlingID) }
func speedKey(crawlingID string) string      { return fmt.Sprintf("crawl:%s:speed", crawlingID) }

// Init sets up the token bucket for a crawling job.
func (rl *DistributedRateLimiter) Init(ctx context.Context, crawlingID string, speedPerHour int) error {
	pipe := rl.rdb.Pipeline()
	pipe.Set(ctx, speedKey(crawlingID), speedPerHour, 0)
	pipe.Set(ctx, tokensKey(crawlingID), 0, 0) // start empty, refill will add
	pipe.Set(ctx, refillKey(crawlingID), time.Now().UnixMilli(), 0)
	_, err := pipe.Exec(ctx)
	return err
}

// Acquire attempts to consume `count` tokens. Returns the number of tokens actually acquired.
// This is a non-blocking call - returns 0 if no tokens available.
//
// Lua script atomically:
// 1. Calculates elapsed time since last refill
// 2. Adds new tokens based on rate
// 3. Caps at burst size
// 4. Consumes requested tokens
var acquireScript = redis.NewScript(`
local tokens_key = KEYS[1]
local refill_key = KEYS[2]
local speed_key  = KEYS[3]

local requested = tonumber(ARGV[1])
local now_ms    = tonumber(ARGV[2])

-- Get current state
local speed = tonumber(redis.call('GET', speed_key) or 3600)
local tokens = tonumber(redis.call('GET', tokens_key) or 0)
local last_refill = tonumber(redis.call('GET', refill_key) or now_ms)

-- Calculate tokens to add based on elapsed time
local elapsed_ms = now_ms - last_refill
local rate_per_ms = speed / 3600000.0  -- tokens per millisecond
local new_tokens = elapsed_ms * rate_per_ms

-- Burst capacity: allow up to 1 second worth of tokens to accumulate
local burst = math.max(math.ceil(speed / 3600.0) * 2, 10)

-- Add new tokens, cap at burst
tokens = math.min(tokens + new_tokens, burst)

-- Try to consume
local acquired = 0
if tokens >= requested then
    acquired = requested
    tokens = tokens - requested
elseif tokens >= 1 then
    acquired = math.floor(tokens)
    tokens = tokens - acquired
end

-- Update state
redis.call('SET', tokens_key, tokens)
redis.call('SET', refill_key, now_ms)

return acquired
`)

func (rl *DistributedRateLimiter) Acquire(ctx context.Context, crawlingID string, count int) (int, error) {
	result, err := acquireScript.Run(ctx, rl.rdb,
		[]string{tokensKey(crawlingID), refillKey(crawlingID), speedKey(crawlingID)},
		count, time.Now().UnixMilli(),
	).Int()

	if err != nil {
		return 0, err
	}
	return result, nil
}

// UpdateSpeed dynamically changes the crawl speed.
func (rl *DistributedRateLimiter) UpdateSpeed(ctx context.Context, crawlingID string, speedPerHour int) error {
	return rl.rdb.Set(ctx, speedKey(crawlingID), speedPerHour, 0).Err()
}

// Cleanup removes all rate limiter keys for a crawling job.
func (rl *DistributedRateLimiter) Cleanup(ctx context.Context, crawlingID string) error {
	return rl.rdb.Del(ctx,
		tokensKey(crawlingID),
		refillKey(crawlingID),
		speedKey(crawlingID),
	).Err()
}
