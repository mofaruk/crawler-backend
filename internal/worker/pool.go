package worker

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webkonsulenterne/crawler-backend/internal/config"
	"github.com/webkonsulenterne/crawler-backend/internal/crawler"
	"github.com/webkonsulenterne/crawler-backend/internal/dedup"
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
	dedup        *dedup.Deduplicator

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
	dd *dedup.Deduplicator,
) *Pool {
	return &Pool{
		cfg:          cfg,
		queue:        q,
		stateManager: sm,
		rateLimiter:  rl,
		fetcher:      fetcher,
		repo:         repo,
		dedup:        dd,
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

	// Pages and assets have separate budgets: a page costs the origin a PHP
	// request and database queries, while an asset is usually served straight
	// from disk or already sitting at the CDN edge.
	//
	// The queue is FIFO and mixed, so the dispatcher cannot know what is next
	// without looking. It draws on both buckets, then keeps only as many tasks
	// as each budget actually covers and puts the rest back — so a page is
	// never dispatched on the asset budget, and the page rate stays meaningful
	// however the queue is ordered.
	pageTokens, err := p.rateLimiter.Acquire(ctx, crawlingID, want)
	if err != nil {
		return 0, false, err
	}

	assetTokens, err := p.rateLimiter.AcquireAssets(ctx, crawlingID, want-pageTokens)
	if err != nil {
		assetTokens = 0
	}

	if pageTokens+assetTokens == 0 {
		metrics.RateLimitWaits.WithLabelValues(crawlingID).Inc()
		return 0, false, nil
	}

	tasks, err := p.queue.DequeueBatch(ctx, crawlingID, pageTokens+assetTokens)
	if err != nil {
		return 0, true, err
	}

	var (
		dispatch []models.CrawlTask
		requeue  []models.CrawlTask
		pages    int
		assets   int
	)

	for _, t := range tasks {
		if crawler.IsAssetURL(t.URL) {
			if assets < assetTokens {
				assets++
				dispatch = append(dispatch, t)
				continue
			}
			// Out of asset budget, but a page token can cover it.
			if pages < pageTokens {
				pages++
				dispatch = append(dispatch, t)
				continue
			}
		} else if pages < pageTokens {
			pages++
			dispatch = append(dispatch, t)
			continue
		}

		requeue = append(requeue, t)
	}

	// Whatever neither budget covered goes back at the head of the queue, so
	// nothing is lost and ordering is preserved.
	if len(requeue) > 0 {
		if err := p.queue.EnqueueBatch(ctx, crawlingID, requeue); err != nil {
			log.Error().Err(err).Int("count", len(requeue)).Msg("failed to requeue rate-limited tasks")
		}
	}

	metrics.RateLimitTokensAcquired.WithLabelValues(crawlingID).Add(float64(len(dispatch)))

	for i := range dispatch {
		t := dispatch[i]
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

		p.recordAttemptFailure(ctx, logger, crawlingID, task, result.Error.Error(), 0)
		return
	}

	metrics.HTTPStatusCodes.WithLabelValues(crawlingID, fmt.Sprintf("%d", result.StatusCode)).Inc()

	// Check for server errors (retry-worthy)
	if result.StatusCode >= 500 {
		metrics.CrawlErrorsTotal.WithLabelValues(crawlingID, "server_error").Inc()

		p.recordAttemptFailure(ctx, logger, crawlingID, task,
			fmt.Sprintf("server returned HTTP %d", result.StatusCode), result.StatusCode)
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
		RedirectedTo: result.RedirectedTo,
	}

	// Assets the page references — images, stylesheets, scripts, fonts. A
	// sitemap lists pages and at best original-size images, but a visitor
	// fetches the responsive variant, so warming only what the sitemap names
	// leaves most of what people actually wait for cold.
	if result.Signals != nil && len(result.Signals.Assets) > 0 {
		p.queueAssets(ctx, crawlingID, task, result.Signals.Assets)
	}

	// Outbound links are recorded separately from the crawl result: link
	// checking is its own pipeline with its own budget, and a page's link graph
	// would dwarf the result document if stored inline.
	if result.Signals != nil && len(result.Signals.Links) > 0 {
		if external := crawler.ExternalLinks(task.URL, result.Signals.Links); len(external) > 0 {
			links := make([]models.OutboundLink, 0, len(external))
			for _, l := range external {
				links = append(links, models.OutboundLink{
					URL:     l.URL,
					FoundOn: []string{l.FoundOn},
				})
			}

			// Never fatal to the crawl: link collection is a side benefit, and
			// the cache report is what the customer is paying for.
			if err := p.repo.RecordOutboundLinks(ctx, mustObjectID(task.SiteID), links); err != nil {
				log.Warn().Err(err).Str("url", task.URL).Msg("failed to record outbound links")
			}
		}
	}

	if result.Signals != nil {
		crawlingResult.Page = &models.PageSignals{
			Title:            result.Signals.Title,
			TitleLength:      result.Signals.TitleLength,
			MetaDescription:  result.Signals.MetaDescription,
			MetaDescLength:   result.Signals.MetaDescLength,
			Canonical:        result.Signals.Canonical,
			NoIndex:          result.Signals.NoIndex,
			H1Count:          result.Signals.H1Count,
			WordCount:        result.Signals.WordCount,
			ImagesMissingAlt: result.Signals.ImagesMissingAlt,
			InsecureRefs:     result.Signals.InsecureRefs,
			SoftNotFound:     result.Signals.SoftNotFound,
		}
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

// recordAttemptFailure handles one failed fetch attempt: it reschedules the
// task and, only once the retry chain is exhausted, counts the URL as failed
// and persists a CrawlFailure row.
//
// Counting on every attempt (the previous behaviour) inflated failed_urls by
// up to MaxRetries+1 per dead URL, pushing crawled+failed past total_urls and
// progress past 100%.
func (p *Pool) recordAttemptFailure(
	ctx context.Context,
	logger zerolog.Logger,
	crawlingID string,
	task *models.CrawlTask,
	errMsg string,
	statusCode int,
) {
	dead, err := p.queue.Retry(ctx, crawlingID, task)
	if err != nil {
		logger.Error().Err(err).Msg("failed to retry task")
	}
	if !dead {
		// Still has attempts left — not a failure yet.
		return
	}

	metrics.URLsCrawledTotal.WithLabelValues(crawlingID, "failed").Inc()
	_ = p.repo.UpdateCrawlingProgress(ctx, mustObjectID(crawlingID), 0, 1)

	// Persist the failure so GET /crawlings/:id/failures can report it.
	if err := p.repo.InsertCrawlFailure(ctx, &models.CrawlFailure{
		CrawlingID: mustObjectID(crawlingID),
		SiteID:     mustObjectID(task.SiteID),
		URL:        task.URL,
		Error:      errMsg,
		StatusCode: statusCode,
		Retries:    task.Retries,
		FailedAt:   time.Now(),
	}); err != nil {
		logger.Error().Err(err).Msg("failed to store crawl failure")
	}
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

// queueAssets adds a page's asset references to the running crawl.
//
// Same-host only: a CDN or a third party's images are not the customer's cache
// to warm, and fetching them would spend the crawl's budget on someone else's
// infrastructure. Deduplicated against the same set the sitemap URLs used, so
// a logo referenced from every page is fetched once.
func (p *Pool) queueAssets(ctx context.Context, crawlingID string, task *models.CrawlTask, refs []string) {
	page, err := url.Parse(task.URL)
	if err != nil || page.Host == "" {
		return
	}

	// The site's url_limit is a promise about how many requests a crawl makes
	// against the customer's origin. Assets are requests, so discovery stops
	// once the crawl has as many URLs as the limit allows.
	var (
		limit int
		mode  = models.AssetModeTopVariants
	)
	if oid, err := primitive.ObjectIDFromHex(task.SiteID); err == nil {
		if site, err := p.repo.GetSite(ctx, oid); err == nil && site != nil {
			limit = site.URLLimit
			mode = models.NormalizeAssetMode(site.AssetMode)
		}
	}

	// Narrow to what this site actually wants warmed before spending any of
	// the budget on it.
	refs = crawler.FilterAssets(refs, mode)
	if len(refs) == 0 {
		return
	}

	// Counted once and tracked locally: a Count per asset would be hundreds of
	// Redis round trips per page. Slight overshoot when several pages queue
	// assets at the same moment is acceptable — the limit guards the customer's
	// origin from a runaway crawl, not an exact quota.
	var remaining int
	if limit > 0 {
		seen, err := p.dedup.Count(ctx, crawlingID)
		if err != nil {
			return
		}
		if seen >= int64(limit) {
			return
		}
		remaining = limit - int(seen)
	}

	var tasks []models.CrawlTask

	for _, raw := range refs {
		if limit > 0 && len(tasks) >= remaining {
			break
		}

		ref, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}

		target := page.ResolveReference(ref)
		if target.Scheme != "http" && target.Scheme != "https" {
			continue
		}
		if !strings.EqualFold(target.Host, page.Host) {
			continue
		}

		// The fragment is a position within a file, not a separate one.
		target.Fragment = ""
		assetURL := target.String()

		urlHash := dedup.HashURL(assetURL)
		isNew, err := p.dedup.MarkSeen(ctx, crawlingID, urlHash)
		if err != nil || !isNew {
			continue
		}

		tasks = append(tasks, models.CrawlTask{
			CrawlingID:  crawlingID,
			SiteID:      task.SiteID,
			URL:         assetURL,
			URLHash:     urlHash,
			UserAgent:   task.UserAgent,
			ExtractData: task.ExtractData,
			MaxRetries:  p.cfg.CrawlerMaxRetries,
			EnqueuedAt:  time.Now().Unix(),
		})
	}

	if len(tasks) == 0 {
		return
	}

	if err := p.queue.EnqueueBatch(ctx, crawlingID, tasks); err != nil {
		log.Warn().Err(err).Int("assets", len(tasks)).Msg("failed to queue page assets")
		return
	}

	// Remember them, so the next crawl does not have to re-parse every page to
	// find the same assets again.
	if siteOID, err := primitive.ObjectIDFromHex(task.SiteID); err == nil {
		stored := make([]models.SiteURL, 0, len(tasks))
		for _, t := range tasks {
			stored = append(stored, models.SiteURL{
				URL:     t.URL,
				URLHash: t.URLHash,
				Kind:    models.SiteURLKindAsset,
			})
		}

		if err := p.repo.RecordSiteURLs(ctx, siteOID, stored); err != nil {
			log.Warn().Err(err).Msg("failed to store discovered assets")
		}
	}

	// The progress bar counts what will actually be fetched, so the total has
	// to grow as assets are discovered rather than only counting sitemap URLs.
	if oid, err := primitive.ObjectIDFromHex(crawlingID); err == nil {
		_ = p.repo.IncCrawlingTotalURLs(ctx, oid, len(tasks))
	}
}
