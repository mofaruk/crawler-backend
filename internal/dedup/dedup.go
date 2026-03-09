package dedup

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Distributed URL deduplication using Redis SETs.
//
// Redis key:
//   crawl:{crawling_id}:seen - SET of URL hashes
//
// Each URL is hashed with SHA-256 (truncated to 16 bytes for memory efficiency).
// For a crawl job with 10M URLs, this uses ~160MB of Redis memory.
//
// Trade-off: We use a SET instead of a Bloom filter for zero false positives.
// For extremely large URL sets (100M+), switch to a Redis Bloom filter module.

type Deduplicator struct {
	rdb *redis.Client
}

func NewDeduplicator(rdb *redis.Client) *Deduplicator {
	return &Deduplicator{rdb: rdb}
}

func seenKey(crawlingID string) string {
	return fmt.Sprintf("crawl:%s:seen", crawlingID)
}

// HashURL produces a compact hash of a URL for dedup purposes.
func HashURL(url string) string {
	h := sha256.Sum256([]byte(url))
	return fmt.Sprintf("%x", h[:16]) // 128-bit, collision-safe for billions of URLs
}

// IsSeen checks if a URL has already been queued for this crawling job.
func (d *Deduplicator) IsSeen(ctx context.Context, crawlingID, urlHash string) (bool, error) {
	return d.rdb.SIsMember(ctx, seenKey(crawlingID), urlHash).Result()
}

// MarkSeen marks a URL as seen. Returns true if it was newly added (not duplicate).
func (d *Deduplicator) MarkSeen(ctx context.Context, crawlingID, urlHash string) (bool, error) {
	result, err := d.rdb.SAdd(ctx, seenKey(crawlingID), urlHash).Result()
	if err != nil {
		return false, err
	}
	return result > 0, nil // result is number of elements added (0 if already exists)
}

// MarkSeenBatch marks multiple URL hashes and returns which ones were new.
var markSeenBatchScript = redis.NewScript(`
local key = KEYS[1]
local new_count = 0
local new_hashes = {}

for i = 1, #ARGV do
    local added = redis.call('SADD', key, ARGV[i])
    if added == 1 then
        new_count = new_count + 1
        table.insert(new_hashes, ARGV[i])
    end
end

return new_count
`)

func (d *Deduplicator) MarkSeenBatch(ctx context.Context, crawlingID string, hashes []string) (int, error) {
	args := make([]interface{}, len(hashes))
	for i, h := range hashes {
		args[i] = h
	}

	result, err := markSeenBatchScript.Run(ctx, d.rdb,
		[]string{seenKey(crawlingID)},
		args...,
	).Int()

	if err != nil {
		return 0, err
	}
	return result, nil
}

// Count returns the number of unique URLs seen.
func (d *Deduplicator) Count(ctx context.Context, crawlingID string) (int64, error) {
	return d.rdb.SCard(ctx, seenKey(crawlingID)).Result()
}

// Cleanup removes the dedup set for a crawling job.
func (d *Deduplicator) Cleanup(ctx context.Context, crawlingID string) error {
	return d.rdb.Del(ctx, seenKey(crawlingID)).Err()
}
