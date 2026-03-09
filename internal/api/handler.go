package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webkonsulenterne/crawler-backend/internal/config"
	"github.com/webkonsulenterne/crawler-backend/internal/dedup"
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

	// Create crawling job
	crawling := &models.Crawling{
		SiteID:       siteID,
		Status:       models.CrawlStatusPending,
		Speed:        speed,
		ReloadSource: req.ReloadSource,
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

// ingestURLs fetches the URL source and pushes URLs into the queue.
func (h *Handler) ingestURLs(crawlingID string, site *models.Site, crawling *models.Crawling) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	logger := log.With().Str("crawling_id", crawlingID).Logger()

	oid, _ := primitive.ObjectIDFromHex(crawlingID)

	// Fetch and parse URLs
	logger.Info().Str("source", site.URLSource).Msg("fetching URL source")
	urls, err := h.parser.ParseURLs(ctx, site.URLSource, site.URLSourceType, site.URLLimit)
	if err != nil {
		logger.Error().Err(err).Msg("failed to parse URL source")
		_ = h.repo.SetCrawlingError(ctx, oid, "failed to parse URL source: "+err.Error())
		return
	}

	if len(urls) == 0 {
		logger.Warn().Msg("no URLs found in source")
		_ = h.repo.SetCrawlingError(ctx, oid, "no URLs found in source")
		return
	}

	logger.Info().Int("url_count", len(urls)).Msg("URLs parsed from source")

	// Set total URLs
	_ = h.repo.SetCrawlingTotalURLs(ctx, oid, len(urls))

	// Initialize rate limiter
	if err := h.rateLimiter.Init(ctx, crawlingID, crawling.Speed); err != nil {
		logger.Error().Err(err).Msg("failed to init rate limiter")
		_ = h.repo.SetCrawlingError(ctx, oid, "failed to init rate limiter")
		return
	}

	// Deduplicate and enqueue in batches
	batchSize := 1000
	totalEnqueued := 0

	for i := 0; i < len(urls); i += batchSize {
		end := i + batchSize
		if end > len(urls) {
			end = len(urls)
		}
		batch := urls[i:end]

		var tasks []models.CrawlTask
		for _, u := range batch {
			urlHash := dedup.HashURL(u)

			// Check dedup
			isNew, err := h.dedup.MarkSeen(ctx, crawlingID, urlHash)
			if err != nil {
				logger.Error().Err(err).Str("url", u).Msg("dedup check failed")
				continue
			}
			if !isNew {
				continue // duplicate
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

	// Mark job as running
	_ = h.repo.UpdateCrawlingStatus(ctx, oid, models.CrawlStatusRunning)
	_ = h.stateManager.SetState(ctx, crawlingID, models.CrawlStatusRunning)
	_ = h.stateManager.AddActiveCrawling(ctx, crawlingID)

	metrics.ActiveCrawlingsGauge.Inc()
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
