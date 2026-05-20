package worker

import (
	"context"
	"fmt"
	"math/rand/v2"
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

// Pool runs the crawler's task processing pipeline.
//
// Architecture (single-producer / multi-consumer):
//   - One dispatcher goroutine is the sole owner of Redis polling. Each cycle
//     it refreshes the active-crawlings set, then per active crawl peeks at
//     the pending queue length, acquires that many rate-limit tokens (bounded
//     by WorkerBatchSize and channel headroom), dequeues, and forwards tasks
//     to consumer workers via workCh.
//   - N consumer goroutines (WORKER_CONCURRENCY) receive dispatchedTask
//     values and execute the HTTP fetch + Mongo insert + ack lifecycle. They
//     do not touch Redis except via queue.Ack / queue.Retry.
//
// This eliminates the O(N_workers × N_crawls) polling fan-out that the
// previous per-worker design produced under load.

type dispatchedTask struct {
	crawlingID string
	task       *models.CrawlTask
}

type Pool struct {
	cfg          *config.Config
	queue        *queue.DistributedQueue
	stateManager *queue.JobStateManager
	rateLimiter  *ratelimiter.DistributedRateLimiter
	fetcher      *crawler.HTTPFetcher
	repo         *repository.MongoRepository

	workCh        chan dispatchedTask
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

	// Channel buffer matches consumer count so a full buffer means every
	// worker is busy — natural back-pressure on the dispatcher.
	p.workCh = make(chan dispatchedTask, p.cfg.WorkerConcurrency)

	log.Info().Int("concurrency", p.cfg.WorkerConcurrency).Msg("starting worker pool")

	p.wg.Add(1)
	go p.recoveryLoop(ctx)

	p.wg.Add(1)
	go p.dispatcherLoop(ctx)

	for i := 0; i < p.cfg.WorkerConcurrency; i++ {
		p.wg.Add(1)
		go p.consumerLoop(ctx, i)
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

// dispatcherLoop is the sole Redis-polling goroutine. Each cycle it refreshes
// the active-crawlings set and, per crawl, peeks at queue length, acquires
// matching rate-limit tokens, dequeues a batch, and hands the tasks off to
// consumer workers via workCh.
func (p *Pool) dispatcherLoop(ctx context.Context) {
	defer p.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		crawlingIDs, err := p.stateManager.GetActiveCrawlings(ctx)
		if err != nil {
			log.Error().Err(err).Msg("dispatcher: failed to get active crawlings")
			sleepCtx(ctx, p.cfg.WorkerPollInterval)
			continue
		}

		if len(crawlingIDs) == 0 {
			sleepCtx(ctx, p.cfg.WorkerPollInterval)
			continue
		}

		rand.Shuffle(len(crawlingIDs), func(i, j int) {
			crawlingIDs[i], crawlingIDs[j] = crawlingIDs[j], crawlingIDs[i]
		})

		dispatched := 0
		tokensSeen := false
		for _, crawlingID := range crawlingIDs {
			select {
			case <-ctx.Done():
				return
			default:
			}

			// Stop the inner loop early if the worker channel is full — any
			// further dequeues would just block the dispatcher.
			if len(p.workCh) == cap(p.workCh) {
				break
			}

			sent, gotTokens, err := p.dispatchOne(ctx, crawlingID)
			if err != nil {
				log.Error().Err(err).Str("crawling_id", crawlingID).Msg("dispatcher: dispatch error")
			}
			dispatched += sent
			if gotTokens {
				tokensSeen = true
			}
		}

		if dispatched == 0 {
			delay := p.cfg.WorkerPollInterval
			if !tokensSeen {
				// Everyone throttled — back off harder so buckets can refill.
				delay = 5 * p.cfg.WorkerPollInterval
			}
			sleepCtx(ctx, delay)
		}
	}
}

// dispatchOne processes a single crawl: validates state, peeks at queue
// length, acquires rate-limit tokens (capped at WorkerBatchSize and channel
// headroom), dequeues a matching batch, and forwards each task to workCh.
//
// Returns the number of tasks actually sent and whether any tokens were
// acquired (used by the caller to choose its backoff).
func (p *Pool) dispatchOne(ctx context.Context, crawlingID string) (sent int, gotTokens bool, err error) {
	state, err := p.stateManager.GetState(ctx, crawlingID)
	if err != nil || state != models.CrawlStatusRunning {
		return 0, false, err
	}

	// Peek at pending length so we don't consume tokens we can't use.
	pending, err := p.queue.PendingLen(ctx, crawlingID)
	if err != nil {
		return 0, false, err
	}
	if pending == 0 {
		p.checkJobCompletion(ctx, crawlingID)
		return 0, false, nil
	}

	free := cap(p.workCh) - len(p.workCh)
	if free <= 0 {
		return 0, false, nil
	}

	want := int(pending)
	if want > p.cfg.WorkerBatchSize {
		want = p.cfg.WorkerBatchSize
	}
	if want > free {
		want = free
	}

	acquired, err := p.rateLimiter.Acquire(ctx, crawlingID, want)
	if err != nil {
		return 0, false, err
	}
	if acquired == 0 {
		metrics.RateLimitWaits.WithLabelValues(crawlingID).Inc()
		return 0, false, nil
	}
	metrics.RateLimitTokensAcquired.WithLabelValues(crawlingID).Add(float64(acquired))

	tasks, err := p.queue.DequeueBatch(ctx, crawlingID, acquired)
	if err != nil {
		return 0, true, err
	}

	for i := range tasks {
		t := tasks[i]
		select {
		case p.workCh <- dispatchedTask{crawlingID: crawlingID, task: &t}:
			sent++
		case <-ctx.Done():
			return sent, true, nil
		}
	}
	return sent, true, nil
}

// consumerLoop is the main loop for each worker goroutine. It blocks on the
// dispatcher channel and only does real work — no Redis polling.
func (p *Pool) consumerLoop(ctx context.Context, workerID int) {
	defer p.wg.Done()

	logger := log.With().Int("worker_id", workerID).Logger()
	logger.Debug().Msg("worker started")

	metrics.WorkerIdleGauge.Inc()
	defer metrics.WorkerIdleGauge.Dec()

	for {
		select {
		case <-ctx.Done():
			logger.Debug().Msg("worker shutting down")
			return
		case dt, ok := <-p.workCh:
			if !ok {
				return
			}

			metrics.WorkerIdleGauge.Dec()
			p.activeWorkers.Add(1)
			metrics.WorkerActiveGauge.Set(float64(p.activeWorkers.Load()))

			p.processTask(ctx, dt.crawlingID, dt.task)

			p.activeWorkers.Add(-1)
			metrics.WorkerActiveGauge.Set(float64(p.activeWorkers.Load()))
			metrics.WorkerIdleGauge.Inc()
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
