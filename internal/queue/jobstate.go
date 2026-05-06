package queue

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/webkonsulenterne/crawler-backend/internal/models"
)

// Job state is stored in Redis for near-real-time state propagation to workers.
//
// Redis key:
//   crawl:{crawling_id}:state - STRING: current job state

type JobStateManager struct {
	rdb *redis.Client
}

func NewJobStateManager(rdb *redis.Client) *JobStateManager {
	return &JobStateManager{rdb: rdb}
}

func stateKey(crawlingID string) string {
	return fmt.Sprintf("crawl:%s:state", crawlingID)
}

func (m *JobStateManager) SetState(ctx context.Context, crawlingID string, state models.CrawlStatus) error {
	return m.rdb.Set(ctx, stateKey(crawlingID), string(state), 0).Err()
}

func (m *JobStateManager) GetState(ctx context.Context, crawlingID string) (models.CrawlStatus, error) {
	result, err := m.rdb.Get(ctx, stateKey(crawlingID)).Result()
	if err == redis.Nil {
		return "", fmt.Errorf("no state found for crawling %s", crawlingID)
	}
	if err != nil {
		return "", err
	}
	return models.CrawlStatus(result), nil
}

func (m *JobStateManager) DeleteState(ctx context.Context, crawlingID string) error {
	return m.rdb.Del(ctx, stateKey(crawlingID)).Err()
}

// Discovery flag: set while auto-discovery is still streaming URLs into the
// queue. Workers consult this to avoid prematurely declaring the job complete
// during a transient empty queue between discovery batches.
//
// Redis key:
//   crawl:{crawling_id}:discovering - STRING "1" while discovery is in progress

func discoveringKey(crawlingID string) string {
	return fmt.Sprintf("crawl:%s:discovering", crawlingID)
}

func (m *JobStateManager) SetDiscovering(ctx context.Context, crawlingID string) error {
	return m.rdb.Set(ctx, discoveringKey(crawlingID), "1", 0).Err()
}

func (m *JobStateManager) ClearDiscovering(ctx context.Context, crawlingID string) error {
	return m.rdb.Del(ctx, discoveringKey(crawlingID)).Err()
}

func (m *JobStateManager) IsDiscovering(ctx context.Context, crawlingID string) (bool, error) {
	res, err := m.rdb.Get(ctx, discoveringKey(crawlingID)).Result()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return res == "1", nil
}

// ActiveCrawlings tracks which crawling jobs are currently active.
//
// Redis key:
//   active:crawlings - SET of crawling IDs

const activeCrawlingsKey = "active:crawlings"

func (m *JobStateManager) AddActiveCrawling(ctx context.Context, crawlingID string) error {
	return m.rdb.SAdd(ctx, activeCrawlingsKey, crawlingID).Err()
}

func (m *JobStateManager) RemoveActiveCrawling(ctx context.Context, crawlingID string) error {
	return m.rdb.SRem(ctx, activeCrawlingsKey, crawlingID).Err()
}

func (m *JobStateManager) GetActiveCrawlings(ctx context.Context) ([]string, error) {
	return m.rdb.SMembers(ctx, activeCrawlingsKey).Result()
}
