package worker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webkonsulenterne/crawler-backend/internal/config"
	"github.com/webkonsulenterne/crawler-backend/internal/crawler"
	"github.com/webkonsulenterne/crawler-backend/internal/metrics"
	"github.com/webkonsulenterne/crawler-backend/internal/models"
	"github.com/webkonsulenterne/crawler-backend/internal/queue"
	"github.com/webkonsulenterne/crawler-backend/internal/ratelimiter"
	"github.com/webkonsulenterne/crawler-backend/internal/repository"
)

// Pool manages a set of worker goroutines that process crawl tasks.
//
// Lifecycle:
//   1. Pool starts N goroutines (WORKER_CONCURRENCY)
//   2. Each goroutine polls for active crawling jobs
//   3. For each job, it acquires rate limit tokens
//   4. Dequeues tasks from the job's queue
//   5. Fetches URLs using the HTTP fetcher
//   6. Stores results in MongoDB
//   7. Acknowledges tasks or sends to retry
//   8. Respects job state changes (pause/stop) in near real-time

type Pool struct {
	cfg          *config.Config
	queue        *queue.DistributedQueue
	stateManager *queue.JobStateManager
	rateLimiter  *ratelimiter.DistributedRateLimiter
	fetcher      *crawler.HTTPFetcher
	repo         *repository.MongoRepository

	activeWorkers atomic.Int64
	wg            sync.WaitGroup
	cancel        context.CancelFunc
}

func NewPool(
	cfg *config.Config,
	q *queue.DistributedQueue,
	sm *queue.JobStateManager,
	rl *ratelimiter.DistributedRateLimiter,
	fetcher *crawler.HTTPFetcher,
	repo *repository.MongoRepository,
) *Pool {
	return &Pool{
		cfg:          cfg,
		queue:        q,
		stateManager: sm,
		rateLimiter:  rl,
		fetcher:      fetcher,
		repo:         repo,
	}
}

// Start launches the worker pool.
func (p *Pool) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)

	log.Info().Int("concurrency", p.cfg.WorkerConcurrency).Msg("starting worker pool")

	// Launch recovery goroutine
	p.wg.Add(1)
	go p.recoveryLoop(ctx)

	// Launch worker goroutines
	for i := 0; i < p.cfg.WorkerConcurrency; i++ {
		p.wg.Add(1)
		go p.workerLoop(ctx, i)
	}

	log.Info().Int("workers", p.cfg.WorkerConcurrency).Msg("worker pool started")
}

// Stop gracefully shuts down the worker pool.
func (p *Pool) Stop() {
	log.Info().Msg("stopping worker pool")
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	p.fetcher.Close()
	log.Info().Msg("worker pool stopped")
}

// workerLoop is the main loop for each worker goroutine.
func (p *Pool) workerLoop(ctx context.Context, workerID int) {
	defer p.wg.Done()

	logger := log.With().Int("worker_id", workerID).Logger()
	logger.Debug().Msg("worker started")

	for {
		select {
		case <-ctx.Done():
			logger.Debug().Msg("worker shutting down")
			return
		default:
		}

		// Get active crawling jobs
		crawlingIDs, err := p.stateManager.GetActiveCrawlings(ctx)
		if err != nil {
			logger.Error().Err(err).Msg("failed to get active crawlings")
			sleepCtx(ctx, p.cfg.WorkerPollInterval)
			continue
		}

		if len(crawlingIDs) == 0 {
			metrics.WorkerIdleGauge.Inc()
			sleepCtx(ctx, p.cfg.WorkerPollInterval)
			metrics.WorkerIdleGauge.Dec()
			continue
		}

		// Round-robin across active jobs for fairness
		processed := false
		for _, crawlingID := range crawlingIDs {
			select {
			case <-ctx.Done():
				return
			default:
			}

			// Check job state
			state, err := p.stateManager.GetState(ctx, crawlingID)
			if err != nil {
				continue
			}

			if state != models.CrawlStatusRunning {
				continue
			}

			// Try to acquire a rate limit token
			acquired, err := p.rateLimiter.Acquire(ctx, crawlingID, 1)
			if err != nil {
				logger.Error().Err(err).Str("crawling_id", crawlingID).Msg("rate limiter error")
				continue
			}

			if acquired == 0 {
				metrics.RateLimitWaits.WithLabelValues(crawlingID).Inc()
				continue
			}

			metrics.RateLimitTokensAcquired.WithLabelValues(crawlingID).Add(float64(acquired))

			// Dequeue a task
			task, err := p.queue.Dequeue(ctx, crawlingID)
			if err != nil {
				logger.Error().Err(err).Str("crawling_id", crawlingID).Msg("dequeue error")
				continue
			}

			if task == nil {
				// Queue might be empty - check if job should complete
				p.checkJobCompletion(ctx, crawlingID)
				continue
			}

			// Process the task
			p.activeWorkers.Add(1)
			metrics.WorkerActiveGauge.Set(float64(p.activeWorkers.Load()))

			p.processTask(ctx, crawlingID, task)

			p.activeWorkers.Add(-1)
			metrics.WorkerActiveGauge.Set(float64(p.activeWorkers.Load()))
			processed = true
		}

		if !processed {
			sleepCtx(ctx, p.cfg.WorkerPollInterval)
		}
	}
}

// processTask fetches a URL and stores the result.
func (p *Pool) processTask(ctx context.Context, crawlingID string, task *models.CrawlTask) {
	logger := log.With().Str("crawling_id", crawlingID).Str("url", task.URL).Logger()

	// Re-check state before processing
	state, err := p.stateManager.GetState(ctx, crawlingID)
	if err != nil || (state != models.CrawlStatusRunning) {
		// Job paused/stopped - requeue the task
		_ = p.queue.EnqueueBatch(ctx, crawlingID, []models.CrawlTask{*task})
		return
	}

	// Fetch the URL
	result := p.fetcher.Fetch(ctx, task)

	// Record metrics
	metrics.CrawlDuration.WithLabelValues(crawlingID).Observe(result.ResponseTime.Seconds())

	if result.Error != nil {
		logger.Warn().Err(result.Error).Msg("fetch failed")
		metrics.CrawlErrorsTotal.WithLabelValues(crawlingID, "fetch_error").Inc()
		metrics.URLsCrawledTotal.WithLabelValues(crawlingID, "failed").Inc()

		// Retry the task
		if err := p.queue.Retry(ctx, crawlingID, task); err != nil {
			logger.Error().Err(err).Msg("failed to retry task")
		}

		// Update progress
		_ = p.repo.UpdateCrawlingProgress(ctx, mustObjectID(crawlingID), 0, 1)
		return
	}

	metrics.HTTPStatusCodes.WithLabelValues(crawlingID, fmt.Sprintf("%d", result.StatusCode)).Inc()

	// Check for server errors (retry-worthy)
	if result.StatusCode >= 500 {
		metrics.CrawlErrorsTotal.WithLabelValues(crawlingID, "server_error").Inc()
		metrics.URLsCrawledTotal.WithLabelValues(crawlingID, "failed").Inc()

		if err := p.queue.Retry(ctx, crawlingID, task); err != nil {
			logger.Error().Err(err).Msg("failed to retry task")
		}
		_ = p.repo.UpdateCrawlingProgress(ctx, mustObjectID(crawlingID), 0, 1)
		return
	}

	// Store successful result
	crawlingResult := models.CrawlingResult{
		CrawlingID:   mustObjectID(crawlingID),
		SiteID:       mustObjectID(task.SiteID),
		URL:          task.URL,
		StatusCode:   result.StatusCode,
		Headers:      result.Headers,
		ContentType:  result.ContentType,
		ResponseTime: result.ResponseTime.Milliseconds(),
		CrawledAt:    time.Now(),
	}

	if err := p.repo.InsertCrawlingResult(ctx, &crawlingResult); err != nil {
		logger.Error().Err(err).Msg("failed to store result")
		// Don't retry - the crawl itself succeeded
	}

	// Acknowledge the task
	if err := p.queue.Ack(ctx, crawlingID, task); err != nil {
		logger.Error().Err(err).Msg("failed to ack task")
	}

	metrics.URLsCrawledTotal.WithLabelValues(crawlingID, "success").Inc()

	// Update progress
	_ = p.repo.UpdateCrawlingProgress(ctx, mustObjectID(crawlingID), 1, 0)
}

// checkJobCompletion checks if a crawling job has finished all its work.
func (p *Pool) checkJobCompletion(ctx context.Context, crawlingID string) {
	remaining, err := p.queue.QueueLength(ctx, crawlingID)
	if err != nil {
		return
	}
	if remaining != 0 {
		return
	}

	// Auto-discovery streams URLs in over time; an empty queue mid-discovery
	// is not a finished job.
	if discovering, err := p.stateManager.IsDiscovering(ctx, crawlingID); err == nil && discovering {
		return
	}

	state, err := p.stateManager.GetState(ctx, crawlingID)
	if err != nil || state != models.CrawlStatusRunning {
		return
	}

	log.Info().Str("crawling_id", crawlingID).Msg("crawling job completed")

	_ = p.stateManager.SetState(ctx, crawlingID, models.CrawlStatusCompleted)
	_ = p.stateManager.RemoveActiveCrawling(ctx, crawlingID)
	_ = p.repo.UpdateCrawlingStatus(ctx, mustObjectID(crawlingID), models.CrawlStatusCompleted)

	metrics.ActiveCrawlingsGauge.Dec()
}

// recoveryLoop periodically requeues stale processing tasks and retry tasks.
func (p *Pool) recoveryLoop(ctx context.Context) {
	defer p.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			crawlingIDs, err := p.stateManager.GetActiveCrawlings(ctx)
			if err != nil {
				log.Error().Err(err).Msg("recovery: failed to get active crawlings")
				continue
			}

			for _, crawlingID := range crawlingIDs {
				// Requeue stale processing tasks
				requeued, err := p.queue.RequeueStale(ctx, crawlingID)
				if err != nil {
					log.Error().Err(err).Str("crawling_id", crawlingID).Msg("recovery: failed to requeue stale")
				} else if requeued > 0 {
					log.Warn().Int("count", requeued).Str("crawling_id", crawlingID).Msg("recovery: requeued stale tasks")
				}

				// Move retries that are ready back to pending
				retried, err := p.queue.RequeueRetries(ctx, crawlingID, 1000)
				if err != nil {
					log.Error().Err(err).Str("crawling_id", crawlingID).Msg("recovery: failed to requeue retries")
				} else if retried > 0 {
					log.Info().Int("count", retried).Str("crawling_id", crawlingID).Msg("recovery: moved retries to pending")
				}

				// Update queue metrics
				stats, err := p.queue.GetStats(ctx, crawlingID)
				if err == nil {
					metrics.QueuePendingGauge.WithLabelValues(crawlingID).Set(float64(stats.Pending))
					metrics.QueueProcessingGauge.WithLabelValues(crawlingID).Set(float64(stats.Processing))
					metrics.QueueRetryGauge.WithLabelValues(crawlingID).Set(float64(stats.Retry))
					metrics.QueueDeadGauge.WithLabelValues(crawlingID).Set(float64(stats.Dead))
				}
			}
		}
	}
}

// --- Helpers ---

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func mustObjectID(hex string) primitive.ObjectID {
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		log.Error().Err(err).Str("hex", hex).Msg("invalid object ID")
		return primitive.NilObjectID
	}
	return id
}
