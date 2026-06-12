package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webkonsulenterne/crawler-backend/internal/config"
	"github.com/webkonsulenterne/crawler-backend/internal/dedup"
	"github.com/webkonsulenterne/crawler-backend/internal/discovery"
	"github.com/webkonsulenterne/crawler-backend/internal/metrics"
	"github.com/webkonsulenterne/crawler-backend/internal/models"
	"github.com/webkonsulenterne/crawler-backend/internal/queue"
	"github.com/webkonsulenterne/crawler-backend/internal/ratelimiter"
	"github.com/webkonsulenterne/crawler-backend/internal/repository"
	"github.com/webkonsulenterne/crawler-backend/internal/source"
)

type Handler struct {
	cfg          *config.Config
	repo         *repository.MongoRepository
	queue        *queue.DistributedQueue
	stateManager *queue.JobStateManager
	rateLimiter  *ratelimiter.DistributedRateLimiter
	dedup        *dedup.Deduplicator
	parser       *source.URLParser
}

func NewHandler(
	cfg *config.Config,
	repo *repository.MongoRepository,
	q *queue.DistributedQueue,
	sm *queue.JobStateManager,
	rl *ratelimiter.DistributedRateLimiter,
	dd *dedup.Deduplicator,
) *Handler {
	return &Handler{
		cfg:          cfg,
		repo:         repo,
		queue:        q,
		stateManager: sm,
		rateLimiter:  rl,
		dedup:        dd,
		parser:       source.NewURLParser(),
	}
}

// --- Site Endpoints ---

// POST /sites
func (h *Handler) CreateSite(c *gin.Context) {
	var req models.CreateSiteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error(), Code: "INVALID_REQUEST"})
		return
	}

	// url_source is required for csv/xml; for auto it is unused.
	if req.URLSourceType != models.URLSourceTypeAuto && strings.TrimSpace(req.URLSource) == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{
			Error: "url_source is required when url_source_type is 'csv' or 'xml'",
			Code:  "INVALID_REQUEST",
		})
		return
	}

	// Parse extract_data from comma-separated string
	var extractData []string
	if req.ExtractData != "" {
		for _, field := range strings.Split(req.ExtractData, ",") {
			field = strings.TrimSpace(field)
			if field != "" {
				extractData = append(extractData, field)
			}
		}
	}

	userAgent := req.UserAgent
	if userAgent == "" {
		userAgent = h.cfg.DefaultUserAgent
	}

	site := &models.Site{
		Name:          req.Name,
		BaseURL:       req.BaseURL,
		URLLimit:      req.URLLimit,
		URLSource:     req.URLSource,
		URLSourceType: req.URLSourceType,
		UserAgent:     userAgent,
		ExtractData:   extractData,
	}

	if err := h.repo.CreateSite(c.Request.Context(), site); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			c.JSON(http.StatusConflict, models.ErrorResponse{Error: "site with this base_url already exists", Code: "DUPLICATE"})
			return
		}
		log.Error().Err(err).Msg("failed to create site")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to create site"})
		return
	}

	c.JSON(http.StatusCreated, site)
}

// GET /sites
func (h *Handler) ListSites(c *gin.Context) {
	skip, limit := parsePagination(c)
	sites, total, err := h.repo.ListSites(c.Request.Context(), skip, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to list sites")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to list sites"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":  sites,
		"total": total,
		"skip":  skip,
		"limit": limit,
	})
}

// GET /sites/:id
func (h *Handler) GetSite(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	site, err := h.repo.GetSite(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "site not found", Code: "NOT_FOUND"})
		return
	}

	c.JSON(http.StatusOK, site)
}

// PUT /sites/:id
func (h *Handler) UpdateSite(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	var req models.UpdateSiteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error(), Code: "INVALID_REQUEST"})
		return
	}

	update := bson.M{}
	if req.Name != nil {
		update["name"] = *req.Name
	}
	if req.BaseURL != nil {
		update["base_url"] = *req.BaseURL
	}
	if req.URLLimit != nil {
		update["url_limit"] = *req.URLLimit
	}
	if req.URLSource != nil {
		update["url_source"] = *req.URLSource
	}
	if req.URLSourceType != nil {
		update["url_source_type"] = *req.URLSourceType
	}
	if req.UserAgent != nil {
		update["user_agent"] = *req.UserAgent
	}
	if req.ExtractData != nil {
		var extractData []string
		if *req.ExtractData != "" {
			for _, field := range strings.Split(*req.ExtractData, ",") {
				field = strings.TrimSpace(field)
				if field != "" {
					extractData = append(extractData, field)
				}
			}
		}
		update["extract_data"] = extractData
	}

	if len(update) == 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "no fields to update", Code: "INVALID_REQUEST"})
		return
	}

	site, err := h.repo.UpdateSite(c.Request.Context(), id, update)
	if err != nil {
		if strings.Contains(err.Error(), "no documents") {
			c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "site not found", Code: "NOT_FOUND"})
			return
		}
		if strings.Contains(err.Error(), "duplicate key") {
			c.JSON(http.StatusConflict, models.ErrorResponse{Error: "site with this base_url already exists", Code: "DUPLICATE"})
			return
		}
		log.Error().Err(err).Msg("failed to update site")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to update site"})
		return
	}

	c.JSON(http.StatusOK, site)
}

// DELETE /sites/:id
func (h *Handler) DeleteSite(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	if err := h.repo.DeleteSite(c.Request.Context(), id); err != nil {
		log.Error().Err(err).Msg("failed to delete site")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to delete site"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "site deleted"})
}

// --- Crawling Endpoints ---

// POST /crawlings/start
func (h *Handler) StartCrawling(c *gin.Context) {
	var req models.StartCrawlingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error(), Code: "INVALID_REQUEST"})
		return
	}

	siteID, err := primitive.ObjectIDFromHex(req.SiteID)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site_id", Code: "INVALID_ID"})
		return
	}

	// Load site config
	site, err := h.repo.GetSite(c.Request.Context(), siteID)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "site not found", Code: "NOT_FOUND"})
		return
	}

	// Apply speed defaults and limits
	speed := req.Speed
	if speed <= 0 {
		speed = 3600
	}
	if speed > 72000 {
		speed = 72000
	}

	// Default URL-type scope; "all" if unset or empty.
	urlType := req.URLType
	if urlType == "" {
		urlType = models.CrawlURLTypeAll
	}

	// Create crawling job
	crawling := &models.Crawling{
		SiteID:       siteID,
		Status:       models.CrawlStatusPending,
		Speed:        speed,
		ReloadSource: req.ReloadSource,
		URLType:      urlType,
	}

	if err := h.repo.CreateCrawling(c.Request.Context(), crawling); err != nil {
		log.Error().Err(err).Msg("failed to create crawling")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to create crawling job"})
		return
	}

	crawlingID := crawling.ID.Hex()

	// Process URL ingestion asynchronously
	go h.ingestURLs(crawlingID, site, crawling)

	c.JSON(http.StatusAccepted, gin.H{
		"id":      crawlingID,
		"status":  crawling.Status,
		"message": "crawling job created, URL ingestion started",
	})
}

// allowsURL reports whether a URL passes the crawling's url_type scope. An
// empty/unset/"all" scope passes everything. Classification is extension-based
// (see discovery.IsStaticURL) — a heuristic, since we don't know Content-Type
// until after fetching.
func allowsURL(scope, rawURL string) bool {
	switch scope {
	case "", models.CrawlURLTypeAll:
		return true
	case models.CrawlURLTypeStatic:
		return discovery.IsStaticURL(rawURL)
	case models.CrawlURLTypeDynamic:
		return !discovery.IsStaticURL(rawURL)
	default:
		return true
	}
}

// ingestURLs fetches the URL source and pushes URLs into the queue. For
// "auto" sites it delegates to ingestAutoDiscovery; otherwise it parses the
// CSV/XML source upfront.
func (h *Handler) ingestURLs(crawlingID string, site *models.Site, crawling *models.Crawling) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	logger := log.With().Str("crawling_id", crawlingID).Logger()
	oid, _ := primitive.ObjectIDFromHex(crawlingID)

	if site.URLSourceType == models.URLSourceTypeAuto {
		h.ingestAutoDiscovery(ctx, logger, crawlingID, oid, site, crawling)
		return
	}

	// --- Static-source (CSV / XML) path ---

	logger.Info().
		Str("source", site.URLSource).
		Str("source_type", site.URLSourceType).
		Int("url_limit", site.URLLimit).
		Msg("fetching URL source")

	urls, stats, err := h.parser.ParseURLs(ctx, site.URLSource, site.URLSourceType, site.UserAgent, site.URLLimit)
	if err != nil {
		logger.Error().Err(err).Interface("parse_stats", stats).Msg("failed to parse URL source")
		msg := "failed to parse URL source: " + err.Error()
		if stats != nil {
			if d := stats.Diagnosis(); d != "" && !strings.Contains(msg, d) {
				msg += " (" + d + ")"
			}
		}
		_ = h.repo.SetCrawlingError(ctx, oid, msg)
		return
	}

	if len(urls) == 0 {
		diagnosis := stats.Diagnosis()
		logger.Warn().
			Interface("parse_stats", stats).
			Str("diagnosis", diagnosis).
			Msg("no URLs found in source")
		_ = h.repo.SetCrawlingError(ctx, oid, "no URLs found in source — "+diagnosis)
		return
	}

	logger.Info().
		Int("url_count", len(urls)).
		Interface("parse_stats", stats).
		Msg("URLs parsed from source")

	// Provisional total — corrected at the end to reflect what actually
	// passed the type filter and dedup.
	_ = h.repo.SetCrawlingTotalURLs(ctx, oid, len(urls))

	if err := h.rateLimiter.Init(ctx, crawlingID, crawling.Speed); err != nil {
		logger.Error().Err(err).Msg("failed to init rate limiter")
		_ = h.repo.SetCrawlingError(ctx, oid, "failed to init rate limiter")
		return
	}

	batchSize := 1000
	totalEnqueued := 0

	for i := 0; i < len(urls); i += batchSize {
		end := i + batchSize
		if end > len(urls) {
			end = len(urls)
		}

		var tasks []models.CrawlTask
		for _, u := range urls[i:end] {
			if !allowsURL(crawling.URLType, u) {
				continue
			}
			urlHash := dedup.HashURL(u)
			isNew, err := h.dedup.MarkSeen(ctx, crawlingID, urlHash)
			if err != nil {
				logger.Error().Err(err).Str("url", u).Msg("dedup check failed")
				continue
			}
			if !isNew {
				continue
			}
			tasks = append(tasks, models.CrawlTask{
				CrawlingID:  crawlingID,
				SiteID:      site.ID.Hex(),
				URL:         u,
				URLHash:     urlHash,
				UserAgent:   site.UserAgent,
				ExtractData: site.ExtractData,
				Retries:     0,
				MaxRetries:  h.cfg.CrawlerMaxRetries,
				EnqueuedAt:  time.Now().Unix(),
			})
		}

		if len(tasks) > 0 {
			if err := h.queue.EnqueueBatch(ctx, crawlingID, tasks); err != nil {
				logger.Error().Err(err).Msg("failed to enqueue batch")
				continue
			}
			totalEnqueued += len(tasks)
		}
	}

	logger.Info().Int("enqueued", totalEnqueued).Msg("URL ingestion complete")

	// Correct total to actual enqueued count (post-filter, post-dedup).
	_ = h.repo.SetCrawlingTotalURLs(ctx, oid, totalEnqueued)

	if totalEnqueued == 0 {
		// Type filter excluded everything (or source was all duplicates).
		_ = h.repo.SetCrawlingError(ctx, oid, "no URLs passed the url_type filter; nothing to crawl")
		return
	}

	_ = h.repo.UpdateCrawlingStatus(ctx, oid, models.CrawlStatusRunning)
	_ = h.stateManager.SetState(ctx, crawlingID, models.CrawlStatusRunning)
	_ = h.stateManager.AddActiveCrawling(ctx, crawlingID)

	metrics.ActiveCrawlingsGauge.Inc()
}

// ingestAutoDiscovery walks the site BFS-style starting from base_url,
// emitting page and static-asset URLs into the queue as they are found.
//
// The crawl is set to "running" in Redis (so workers fetch immediately) and
// "discovering" in the DB so the UI can show the right state. Once discovery
// finishes the DB status flips to "running".
func (h *Handler) ingestAutoDiscovery(
	ctx context.Context,
	logger zerolog.Logger,
	crawlingID string,
	oid primitive.ObjectID,
	site *models.Site,
	crawling *models.Crawling,
) {
	logger.Info().Str("base_url", site.BaseURL).Int("limit", site.URLLimit).Msg("starting auto discovery")

	if err := h.rateLimiter.Init(ctx, crawlingID, crawling.Speed); err != nil {
		logger.Error().Err(err).Msg("failed to init rate limiter")
		_ = h.repo.SetCrawlingError(ctx, oid, "failed to init rate limiter")
		return
	}

	// Workers need to start crawling as soon as the first URL hits the queue.
	// The discovering flag stops the worker pool from declaring completion on
	// transient empty-queue states between discovery batches.
	_ = h.stateManager.SetDiscovering(ctx, crawlingID)
	_ = h.repo.UpdateCrawlingStatus(ctx, oid, models.CrawlStatusDiscovering)
	_ = h.stateManager.SetState(ctx, crawlingID, models.CrawlStatusRunning)
	_ = h.stateManager.AddActiveCrawling(ctx, crawlingID)
	metrics.ActiveCrawlingsGauge.Inc()

	const flushBatch = 100
	var (
		bufMu         sync.Mutex
		buffer        = make([]models.CrawlTask, 0, flushBatch)
		totalEnqueued int
	)

	flush := func() {
		bufMu.Lock()
		if len(buffer) == 0 {
			bufMu.Unlock()
			return
		}
		batch := buffer
		buffer = make([]models.CrawlTask, 0, flushBatch)
		bufMu.Unlock()

		if err := h.queue.EnqueueBatch(ctx, crawlingID, batch); err != nil {
			logger.Error().Err(err).Int("batch", len(batch)).Msg("failed to enqueue discovered batch")
			return
		}
		if err := h.repo.IncCrawlingTotalURLs(ctx, oid, len(batch)); err != nil {
			logger.Warn().Err(err).Msg("failed to increment total_urls during discovery")
		}
		totalEnqueued += len(batch)
	}

	emit := func(rawURL string) bool {
		if ctx.Err() != nil {
			return false
		}
		// Type-scope filter — discovery still walks HTML pages so we find
		// linked static assets, but URLs that don't match the scope never
		// hit the queue.
		if !allowsURL(crawling.URLType, rawURL) {
			return true
		}
		urlHash := dedup.HashURL(rawURL)
		isNew, err := h.dedup.MarkSeen(ctx, crawlingID, urlHash)
		if err != nil {
			logger.Error().Err(err).Str("url", rawURL).Msg("dedup check failed during discovery")
			return true
		}
		if !isNew {
			return true
		}

		bufMu.Lock()
		buffer = append(buffer, models.CrawlTask{
			CrawlingID:  crawlingID,
			SiteID:      site.ID.Hex(),
			URL:         rawURL,
			URLHash:     urlHash,
			UserAgent:   site.UserAgent,
			ExtractData: site.ExtractData,
			Retries:     0,
			MaxRetries:  h.cfg.CrawlerMaxRetries,
			EnqueuedAt:  time.Now().Unix(),
		})
		shouldFlush := len(buffer) >= flushBatch
		bufMu.Unlock()

		if shouldFlush {
			flush()
		}
		return true
	}

	d := discovery.New(site.UserAgent)
	if err := d.Discover(ctx, site.BaseURL, site.URLLimit, emit); err != nil {
		logger.Error().Err(err).Msg("auto discovery failed")
		flush() // emit whatever we found before the error
		_ = h.repo.SetCrawlingError(ctx, oid, "auto discovery failed: "+err.Error())
		return
	}

	flush()

	// Discovery is done; clear the flag before any further state change so the
	// worker pool can declare completion once the queue drains.
	_ = h.stateManager.ClearDiscovering(ctx, crawlingID)

	if totalEnqueued == 0 {
		logger.Warn().Msg("auto discovery found no URLs")
		_ = h.repo.SetCrawlingError(ctx, oid, "auto discovery found no URLs at base_url")
		return
	}

	logger.Info().Int("enqueued", totalEnqueued).Msg("auto discovery complete")

	// Flip DB status to running for the rest of the crawl.
	_ = h.repo.UpdateCrawlingStatus(ctx, oid, models.CrawlStatusRunning)
}

// POST /crawlings/:id/pause
func (h *Handler) PauseCrawling(c *gin.Context) {
	crawlingID := c.Param("id")
	oid, err := primitive.ObjectIDFromHex(crawlingID)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	crawling, err := h.repo.GetCrawling(c.Request.Context(), oid)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "crawling not found", Code: "NOT_FOUND"})
		return
	}

	if crawling.Status != models.CrawlStatusRunning {
		c.JSON(http.StatusConflict, models.ErrorResponse{Error: "crawling is not running", Code: "INVALID_STATE"})
		return
	}

	_ = h.stateManager.SetState(c.Request.Context(), crawlingID, models.CrawlStatusPaused)
	_ = h.repo.UpdateCrawlingStatus(c.Request.Context(), oid, models.CrawlStatusPaused)

	c.JSON(http.StatusOK, gin.H{"id": crawlingID, "status": models.CrawlStatusPaused})
}

// POST /crawlings/:id/resume
func (h *Handler) ResumeCrawling(c *gin.Context) {
	crawlingID := c.Param("id")
	oid, err := primitive.ObjectIDFromHex(crawlingID)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	crawling, err := h.repo.GetCrawling(c.Request.Context(), oid)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "crawling not found", Code: "NOT_FOUND"})
		return
	}

	if crawling.Status != models.CrawlStatusPaused {
		c.JSON(http.StatusConflict, models.ErrorResponse{Error: "crawling is not paused", Code: "INVALID_STATE"})
		return
	}

	_ = h.stateManager.SetState(c.Request.Context(), crawlingID, models.CrawlStatusRunning)
	_ = h.repo.UpdateCrawlingStatus(c.Request.Context(), oid, models.CrawlStatusRunning)

	c.JSON(http.StatusOK, gin.H{"id": crawlingID, "status": models.CrawlStatusRunning})
}

// POST /crawlings/:id/stop
func (h *Handler) StopCrawling(c *gin.Context) {
	crawlingID := c.Param("id")
	oid, err := primitive.ObjectIDFromHex(crawlingID)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	crawling, err := h.repo.GetCrawling(c.Request.Context(), oid)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "crawling not found", Code: "NOT_FOUND"})
		return
	}

	if crawling.Status == models.CrawlStatusCompleted || crawling.Status == models.CrawlStatusStopped {
		c.JSON(http.StatusConflict, models.ErrorResponse{Error: "crawling already finished", Code: "INVALID_STATE"})
		return
	}

	_ = h.stateManager.SetState(c.Request.Context(), crawlingID, models.CrawlStatusStopped)
	_ = h.stateManager.ClearDiscovering(c.Request.Context(), crawlingID)
	_ = h.stateManager.RemoveActiveCrawling(c.Request.Context(), crawlingID)
	_ = h.repo.UpdateCrawlingStatus(c.Request.Context(), oid, models.CrawlStatusStopped)
	_ = h.queue.DeleteQueue(c.Request.Context(), crawlingID)
	_ = h.dedup.Cleanup(c.Request.Context(), crawlingID)
	_ = h.rateLimiter.Cleanup(c.Request.Context(), crawlingID)

	metrics.ActiveCrawlingsGauge.Dec()

	c.JSON(http.StatusOK, gin.H{"id": crawlingID, "status": models.CrawlStatusStopped})
}

// GET /crawlings/:id/progress
func (h *Handler) GetCrawlingProgress(c *gin.Context) {
	crawlingID := c.Param("id")
	oid, err := primitive.ObjectIDFromHex(crawlingID)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	crawling, err := h.repo.GetCrawling(c.Request.Context(), oid)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "crawling not found", Code: "NOT_FOUND"})
		return
	}

	progress := float64(0)
	if crawling.TotalURLs > 0 {
		progress = float64(crawling.CrawledURLs+crawling.FailedURLs) / float64(crawling.TotalURLs) * 100
	}

	// Get queue stats for real-time view
	queueStats, _ := h.queue.GetStats(c.Request.Context(), crawlingID)

	// Discovery flag — total_urls is still rising while this is true.
	discovering, _ := h.stateManager.IsDiscovering(c.Request.Context(), crawlingID)

	resp := gin.H{
		"id":           crawlingID,
		"site_id":      crawling.SiteID.Hex(),
		"status":       crawling.Status,
		"total_urls":   crawling.TotalURLs,
		"crawled_urls": crawling.CrawledURLs,
		"failed_urls":  crawling.FailedURLs,
		"progress":     progress,
		"speed":        crawling.Speed,
		"started_at":   crawling.StartedAt,
		"created_at":   crawling.CreatedAt,
		"discovering":  discovering,
	}

	if queueStats != nil {
		resp["queue"] = gin.H{
			"pending":    queueStats.Pending,
			"processing": queueStats.Processing,
			"retry":      queueStats.Retry,
			"dead":       queueStats.Dead,
		}
	}

	c.JSON(http.StatusOK, resp)
}

// GET /crawlings
func (h *Handler) ListCrawlings(c *gin.Context) {
	skip, limit := parsePagination(c)

	filter := bson.M{}
	if siteID := c.Query("site_id"); siteID != "" {
		oid, err := primitive.ObjectIDFromHex(siteID)
		if err == nil {
			filter["site_id"] = oid
		}
	}
	if status := c.Query("status"); status != "" {
		filter["status"] = status
	}

	crawlings, total, err := h.repo.ListCrawlings(c.Request.Context(), filter, skip, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to list crawlings")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to list crawlings"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":  crawlings,
		"total": total,
		"skip":  skip,
		"limit": limit,
	})
}

// GET /crawlings/:id
func (h *Handler) GetCrawling(c *gin.Context) {
	oid, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	crawling, err := h.repo.GetCrawling(c.Request.Context(), oid)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "crawling not found", Code: "NOT_FOUND"})
		return
	}

	c.JSON(http.StatusOK, crawling)
}

// GET /crawlings/:id/failures
func (h *Handler) GetCrawlingFailures(c *gin.Context) {
	oid, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	skip, limit := parsePagination(c)
	failures, err := h.repo.GetCrawlFailures(c.Request.Context(), oid, skip, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to get failures")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to get failures"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": failures})
}

// --- Crawling Results ---

// GET /crawlings/:id/results/analytics?header=cf-cache-status
func (h *Handler) GetHeaderAnalytics(c *gin.Context) {
	oid, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	headerName := c.Query("header")
	if headerName == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "header query parameter is required", Code: "INVALID_REQUEST"})
		return
	}

	values, total, err := h.repo.GetHeaderAnalytics(c.Request.Context(), oid, headerName)
	if err != nil {
		log.Error().Err(err).Msg("failed to get header analytics")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to get header analytics"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"header": headerName,
		"total":  total,
		"values": values,
	})
}

// GET /crawlings/:id/status-analytics — HTTP status code distribution for a crawl.
func (h *Handler) GetCrawlingStatusAnalytics(c *gin.Context) {
	oid, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	values, total, err := h.repo.GetCrawlingStatusAnalytics(c.Request.Context(), oid)
	if err != nil {
		log.Error().Err(err).Msg("failed to get crawl status analytics")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to get status analytics"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"metric": "status_code",
		"total":  total,
		"values": values,
	})
}

// GET /sites/:id/analytics?days=7 — combined per-site analytics over the last
// N days: HTTP status distribution plus a distribution for every header in the
// site's extract_data, aggregated across all of the site's crawl results.
func (h *Handler) GetSiteAnalytics(c *gin.Context) {
	siteID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	site, err := h.repo.GetSite(c.Request.Context(), siteID)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "site not found", Code: "NOT_FOUND"})
		return
	}

	// Window: last `days` days (default 7, clamped 1..90).
	days := 7
	if raw := c.Query("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			days = n
		}
	}
	if days > 90 {
		days = 90
	}
	to := time.Now().UTC()
	from := to.AddDate(0, 0, -days)

	statusValues, statusTotal, err := h.repo.GetSiteStatusAnalytics(c.Request.Context(), siteID, from, to)
	if err != nil {
		log.Error().Err(err).Msg("failed to get site status analytics")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to get site analytics"})
		return
	}

	headers := gin.H{}
	for _, header := range site.ExtractData {
		header = strings.TrimSpace(header)
		if header == "" {
			continue
		}
		values, total, err := h.repo.GetSiteHeaderAnalytics(c.Request.Context(), siteID, header, from, to)
		if err != nil {
			log.Error().Err(err).Str("header", header).Msg("failed to get site header analytics")
			continue // skip this header rather than failing the whole response
		}
		headers[header] = gin.H{"total": total, "values": values}
	}

	c.JSON(http.StatusOK, gin.H{
		"site_id": siteID.Hex(),
		"days":    days,
		"from":    from,
		"to":      to,
		"status":  gin.H{"total": statusTotal, "values": statusValues},
		"headers": headers,
	})
}

// GET /crawlings/:id/results?header=cf-cache-status&value=MISS&skip=0&limit=20
func (h *Handler) GetCrawlingResults(c *gin.Context) {
	oid, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	skip, limit := parsePagination(c)

	filter := bson.M{}
	if headerName := c.Query("header"); headerName != "" {
		if headerValue := c.Query("value"); headerValue != "" {
			filter["headers."+headerName] = headerValue
		} else {
			filter["headers."+headerName] = bson.M{"$exists": true}
		}
	}
	if statusCode := c.Query("status_code"); statusCode != "" {
		if code, err := strconv.Atoi(statusCode); err == nil {
			filter["status_code"] = code
		}
	}

	results, total, err := h.repo.GetCrawlingResults(c.Request.Context(), oid, filter, skip, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to get crawling results")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to get crawling results"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":  results,
		"total": total,
		"skip":  skip,
		"limit": limit,
	})
}

// --- Crawled URL List (cursor pagination) ---

// GET /crawlings/:id/urls?cursor=&limit=50&q=&url_type=&status_code=&header=&value=
func (h *Handler) ListCrawledURLs(c *gin.Context) {
	oid, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	filter := buildResultsFilter(c)

	limit, _ := strconv.ParseInt(c.DefaultQuery("limit", "50"), 10, 64)
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var cursor primitive.ObjectID
	if raw := c.Query("cursor"); raw != "" {
		parsed, err := primitive.ObjectIDFromHex(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid cursor", Code: "INVALID_CURSOR"})
			return
		}
		cursor = parsed
	}

	results, hasMore, err := h.repo.ListCrawlingResultsByCursor(c.Request.Context(), oid, filter, cursor, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to list crawled URLs")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to list crawled URLs"})
		return
	}

	var nextCursor string
	if hasMore && len(results) > 0 {
		nextCursor = results[len(results)-1].ID.Hex()
	}

	c.JSON(http.StatusOK, gin.H{
		"data":        results,
		"limit":       limit,
		"has_more":    hasMore,
		"next_cursor": nextCursor,
	})
}

// GET /crawlings/:id/urls/export — streams CSV of every matching result.
// Same filter set as ListCrawledURLs; no pagination, no row cap.
func (h *Handler) ExportCrawledURLs(c *gin.Context) {
	oid, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	crawling, err := h.repo.GetCrawling(c.Request.Context(), oid)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "crawling not found", Code: "NOT_FOUND"})
		return
	}

	site, err := h.repo.GetSite(c.Request.Context(), crawling.SiteID)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "site not found", Code: "NOT_FOUND"})
		return
	}

	filter := buildResultsFilter(c)

	filename := fmt.Sprintf("crawl-%s-urls-%s.csv", oid.Hex(), time.Now().UTC().Format("20060102-150405"))
	c.Writer.Header().Set("Content-Type", "text/csv; charset=utf-8")
	c.Writer.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Writer.Header().Set("Cache-Control", "no-store")
	c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
	c.Writer.WriteHeader(http.StatusOK)

	w := csv.NewWriter(c.Writer)
	flusher, _ := c.Writer.(http.Flusher)

	header := append([]string{"url", "status_code", "content_type", "response_time_ms", "crawled_at"}, site.ExtractData...)
	if err := w.Write(header); err != nil {
		log.Error().Err(err).Msg("csv export: failed to write header")
		return
	}

	rowCount := 0
	streamErr := h.repo.StreamCrawlingResults(c.Request.Context(), oid, filter, func(doc *models.CrawlingResult) error {
		row := make([]string, 0, len(header))
		row = append(row,
			doc.URL,
			strconv.Itoa(doc.StatusCode),
			doc.ContentType,
			strconv.FormatInt(doc.ResponseTime, 10),
			doc.CrawledAt.UTC().Format(time.RFC3339),
		)
		for _, h := range site.ExtractData {
			row = append(row, doc.Headers[h])
		}
		if err := w.Write(row); err != nil {
			return err
		}
		rowCount++
		// Flush periodically so the browser shows progress and TCP keepalives stay alive.
		if rowCount%500 == 0 {
			w.Flush()
			if flusher != nil {
				flusher.Flush()
			}
		}
		return nil
	})

	w.Flush()
	if flusher != nil {
		flusher.Flush()
	}

	if streamErr != nil {
		// Headers already sent — best effort to log; client sees a truncated file.
		log.Error().Err(streamErr).Int("rows_written", rowCount).Msg("csv export stream interrupted")
	}
}

// buildResultsFilter assembles the MongoDB filter for the URL list / export
// endpoints from the request query string. Caller supplies crawling_id.
func buildResultsFilter(c *gin.Context) bson.M {
	filter := bson.M{}

	if statusCode := c.Query("status_code"); statusCode != "" {
		if code, err := strconv.Atoi(statusCode); err == nil {
			filter["status_code"] = code
		}
	}

	if header := strings.TrimSpace(c.Query("header")); header != "" {
		// HTTP header names are case-insensitive (RFC 7230) but Mongo field
		// paths are exact. Iterate the headers map at query time and match by
		// regex so the user's casing doesn't matter, regardless of what casing
		// the source server returned.
		nameRegex := "^" + regexp.QuoteMeta(header) + "$"

		conds := bson.A{
			bson.M{"$regexMatch": bson.M{
				"input":   "$$pair.k",
				"regex":   nameRegex,
				"options": "i",
			}},
		}
		if value := strings.TrimSpace(c.Query("value")); value != "" {
			valueRegex := "^" + regexp.QuoteMeta(value) + "$"
			conds = append(conds, bson.M{"$regexMatch": bson.M{
				"input":   "$$pair.v",
				"regex":   valueRegex,
				"options": "i",
			}})
		}

		filter["$expr"] = bson.M{
			"$anyElementTrue": bson.A{
				bson.M{"$map": bson.M{
					"input": bson.M{"$objectToArray": bson.M{"$ifNull": bson.A{"$headers", bson.M{}}}},
					"as":    "pair",
					"in":    bson.M{"$and": conds},
				}},
			},
		}
	}

	if q := strings.TrimSpace(c.Query("q")); q != "" {
		filter["url"] = bson.M{
			"$regex":   regexp.QuoteMeta(q),
			"$options": "i",
		}
	}

	switch strings.ToLower(c.Query("url_type")) {
	case "static":
		filter["content_type"] = bson.M{
			"$regex":   `^(text/css|application/javascript|application/x-javascript|image/|font/|video/|audio/|application/octet-stream|application/font-)`,
			"$options": "i",
		}
	case "dynamic":
		filter["content_type"] = bson.M{
			"$regex":   `^(text/html|application/json|application/xml|text/xml|text/plain|application/xhtml)`,
			"$options": "i",
		}
	}

	return filter
}

// --- Health ---

func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": h.cfg.ServiceRole})
}

// --- Helpers ---

func parsePagination(c *gin.Context) (int64, int64) {
	skip, _ := strconv.ParseInt(c.DefaultQuery("skip", "0"), 10, 64)
	limit, _ := strconv.ParseInt(c.DefaultQuery("limit", "20"), 10, 64)
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if skip < 0 {
		skip = 0
	}
	return skip, limit
}
