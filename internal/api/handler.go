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
	"github.com/webkonsulenterne/crawler-backend/internal/linkcheck"
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

	// url_source is required for csv/xml. Auto walks from base_url, and smart
	// finds the sitemap itself, so neither needs one.
	if req.URLSourceType != models.URLSourceTypeAuto &&
		req.URLSourceType != models.URLSourceTypeSmart &&
		strings.TrimSpace(req.URLSource) == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{
			Error: "url_source is required when url_source_type is 'csv' or 'xml'",
			Code:  "INVALID_REQUEST",
		})
		return
	}

	// Customers type a bare domain, so fill the scheme in before anything
	// downstream — validation, storage, crawling — sees the value.
	normalisedBase, err := source.NormalizeSiteURL(req.BaseURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error(), Code: "INVALID_REQUEST"})
		return
	}
	req.BaseURL = normalisedBase

	// Reject infrastructure targets before anything is persisted: both of
	// these are fetched server-side, so an unvalidated value is an SSRF.
	if err := source.ValidateTargetURL(req.BaseURL, h.cfg.AllowPrivateTargets); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "base_url rejected: " + err.Error(), Code: "INVALID_TARGET"})
		return
	}
	if strings.TrimSpace(req.URLSource) != "" {
		if err := source.ValidateTargetURL(req.URLSource, h.cfg.AllowPrivateTargets); err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "url_source rejected: " + err.Error(), Code: "INVALID_TARGET"})
			return
		}
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
		SmartRecrawl:  req.SmartRecrawl,
		AssetMode:     models.NormalizeAssetMode(req.AssetMode),
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
		if normalised, nerr := source.NormalizeSiteURL(*req.BaseURL); nerr == nil {
			req.BaseURL = &normalised
		}
		if err := source.ValidateTargetURL(*req.BaseURL, h.cfg.AllowPrivateTargets); err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "base_url rejected: " + err.Error(), Code: "INVALID_TARGET"})
			return
		}
		update["base_url"] = *req.BaseURL
	}
	if req.URLLimit != nil {
		update["url_limit"] = *req.URLLimit
	}
	if req.URLSource != nil {
		if err := source.ValidateTargetURL(*req.URLSource, h.cfg.AllowPrivateTargets); err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "url_source rejected: " + err.Error(), Code: "INVALID_TARGET"})
			return
		}
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
	if req.SmartRecrawl != nil {
		update["smart_recrawl"] = *req.SmartRecrawl
	}
	if req.AssetMode != nil {
		update["asset_mode"] = models.NormalizeAssetMode(*req.AssetMode)
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

	// Refuse a second concurrent crawl of the same site.
	//
	// Two rounds running together double the request rate against the
	// customer's origin, which can take their site down. Enforced here rather
	// than only in the dashboard so any caller is covered, and returns the
	// existing crawl so the UI can link to it instead of just refusing.
	if active, err := h.repo.ActiveCrawlingForSite(c.Request.Context(), siteID); err != nil {
		log.Error().Err(err).Msg("failed to check for an active crawling")
	} else if active != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":       "this site is already being crawled",
			"code":        "ALREADY_RUNNING",
			"crawling_id": active.ID.Hex(),
			"status":      active.Status,
			"started_at":  active.StartedAt,
		})
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

	// A stored URL list, if one is current, is the whole point of keeping it:
	// deriving the list means fetching the source and parsing every page,
	// which on a real customer site is 341 pages to find 13,209 assets. Doing
	// that every round is most of a crawl's cost spent rediscovering what did
	// not change.
	if !crawling.ReloadSource && h.useStoredURLList(ctx, logger, crawlingID, oid, site, crawling) {
		return
	}

	// Smart: find the sitemap rather than making the customer supply it. The
	// pages it lists are crawled like any XML source; the assets those pages
	// load are harvested as the crawl runs.
	source := site.URLSource
	sourceType := site.URLSourceType

	if sourceType == models.URLSourceTypeSmart {
		found, err := h.resolveSmartSource(ctx, site)
		if err != nil {
			logger.Error().Err(err).Str("base_url", site.BaseURL).Msg("smart source could not find a sitemap")
			_ = h.repo.SetCrawlingError(ctx, oid, "could not find a sitemap for "+site.BaseURL+
				" — set the sitemap URL manually, or upload a CSV")
			return
		}

		logger.Info().Str("sitemap", found).Msg("smart source resolved a sitemap")
		source = found
		sourceType = models.URLSourceTypeXML
	}

	// --- Static-source (CSV / XML) path ---

	logger.Info().
		Str("source", source).
		Str("source_type", sourceType).
		Int("url_limit", site.URLLimit).
		Msg("fetching URL source")

	urls, stats, err := h.parser.ParseURLs(ctx, source, sourceType, site.UserAgent, site.URLLimit)
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

	// Smart recrawl: look up what the previous round found already cached.
	// Those URLs are skipped below and their results copied forward, so the
	// report still describes the whole site rather than only what was
	// re-fetched.
	cached := map[string]models.CrawlingResult{}
	if site.SmartRecrawl {
		found, err := h.repo.CachedResultsFromLastCrawl(ctx, site.ID, oid)
		if err != nil {
			// Not fatal: fall back to crawling everything, which is correct,
			// just slower than the customer asked for.
			logger.Error().Err(err).Msg("smart recrawl lookup failed; crawling all URLs")
		} else {
			cached = found
			logger.Info().Int("cached_last_round", len(cached)).Msg("smart recrawl enabled")
		}
	}

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
	var carryForward []models.CrawlingResult

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
			// Cached last round: copy that result forward instead of
			// spending a request re-confirming it.
			if prev, ok := cached[u]; ok {
				carryForward = append(carryForward, prev)
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

	if len(carryForward) > 0 {
		if err := h.repo.CarryForwardResults(ctx, oid, carryForward); err != nil {
			logger.Error().Err(err).Int("count", len(carryForward)).Msg("failed to carry results forward")
		}
	}

	// Save what the source yielded, so the next crawl can skip re-deriving it.
	// Assets are added by the workers as they parse each page.
	if len(urls) > 0 {
		pageURLs := make([]models.SiteURL, 0, len(urls))
		for _, u := range urls {
			pageURLs = append(pageURLs, models.SiteURL{
				URL:     u,
				URLHash: dedup.HashURL(u),
				Kind:    models.SiteURLKindPage,
			})
		}

		if err := h.repo.RecordSiteURLs(ctx, site.ID, pageURLs); err != nil {
			logger.Warn().Err(err).Msg("failed to store the source URL list")
		} else if err := h.repo.MarkSiteURLsBuilt(ctx, site.ID); err != nil {
			logger.Warn().Err(err).Msg("failed to mark the URL list as built")
		}
	}

	logger.Info().
		Int("enqueued", totalEnqueued).
		Int("carried_forward", len(carryForward)).
		Msg("URL ingestion complete")

	// The total counts carried-forward URLs too: they are part of the report
	// even though they cost no request, and excluding them would make the
	// site look like it shrank.
	_ = h.repo.SetCrawlingTotalURLs(ctx, oid, totalEnqueued+len(carryForward))

	if totalEnqueued == 0 {
		// Everything was carried forward: nothing to fetch, so the round is
		// already complete rather than an error.
		if len(carryForward) > 0 {
			_ = h.repo.SetCrawlingCrawledURLs(ctx, oid, len(carryForward))
			_ = h.repo.UpdateCrawlingStatus(ctx, oid, models.CrawlStatusCompleted)
			_ = h.stateManager.SetState(ctx, crawlingID, models.CrawlStatusCompleted)
			logger.Info().Msg("every URL was still cached; nothing needed fetching")
			return
		}

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

// POST /crawlings/prune — delete a site's finished rounds older than a cutoff
// (and their results/URLs/failures). Drives the dashboard's package-based data
// retention; the dashboard supplies the per-package cutoff.
func (h *Handler) PruneCrawlings(c *gin.Context) {
	var req models.PruneCrawlingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error(), Code: "INVALID_REQUEST"})
		return
	}

	siteID, err := primitive.ObjectIDFromHex(req.SiteID)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site_id", Code: "INVALID_ID"})
		return
	}

	before, err := time.Parse(time.RFC3339, req.Before)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid before timestamp, want RFC3339", Code: "INVALID_REQUEST"})
		return
	}

	deleted, err := h.repo.PruneCrawlingsBefore(c.Request.Context(), siteID, before)
	if err != nil {
		log.Error().Err(err).Str("site_id", req.SiteID).Msg("failed to prune crawlings")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to prune crawlings"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"site_id":           req.SiteID,
		"before":            before.UTC().Format(time.RFC3339),
		"deleted_crawlings": deleted,
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

	// Same window semantics as /issues: rolling days, or an explicit range.
	from, to, days := resolveWindow(c, 7, 90)

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

// GET /sites/:id/issues?days=30&limit=100 — URLs currently failing for a site.
//
// This is the "site health" view: broken links, gone pages and server errors,
// aggregated across every crawl in the window rather than a single round, so a
// customer sees what is wrong with their site rather than what one crawl saw.
func (h *Handler) GetSiteIssues(c *gin.Context) {
	siteID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	if _, err := h.repo.GetSite(c.Request.Context(), siteID); err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "site not found", Code: "NOT_FOUND"})
		return
	}

	// Window: either a rolling `days` count, or an explicit from/to range.
	// An explicit range wins — a caller who names dates means them.
	since, until, days := resolveWindow(c, 30, 365)

	limit := int64(100)
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}

	issues, err := h.repo.GetSiteIssuesBetween(c.Request.Context(), siteID, since, until, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to get site issues")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to get site issues"})
		return
	}

	// Counts by kind, so the UI can headline "3 broken links" without
	// re-deriving it from the list.
	byKind := map[string]int{}
	for _, i := range issues {
		byKind[i.Kind]++
	}

	c.JSON(http.StatusOK, gin.H{
		"site_id": siteID.Hex(),
		"days":    days,
		"since":   since,
		"until":   until,
		"total":   len(issues),
		"by_kind": byKind,
		"data":    issues,
	})
}

// GET /sites/:id/timeline?days=30&limit=100 — how a site changed over time.
//
// One point per crawl, several measures each, so the caller can plot them
// together: coverage, staleness, speed and errors move independently, and
// seeing them on one axis is what separates a CDN problem from an origin one.
func (h *Handler) GetSiteTimeline(c *gin.Context) {
	siteID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	if _, err := h.repo.GetSite(c.Request.Context(), siteID); err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "site not found", Code: "NOT_FOUND"})
		return
	}

	since, _, days := resolveWindow(c, 30, 365)

	limit := int64(100)
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	points, err := h.repo.GetSiteTimeline(c.Request.Context(), siteID, since, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to build site timeline")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to build site timeline"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"site_id": siteID.Hex(),
		"days":    days,
		"since":   since,
		"total":   len(points),
		"data":    points,
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

// resolveWindow reads a time window from the query string.
//
// Two forms are accepted, because they answer different questions:
//   - days=7                     "how have things been lately"
//   - from=2026-08-01&to=...     "what did it look like on this date"
//
// An explicit from/to wins over days: a caller who names dates means them.
// `to` is inclusive of the whole day, so from=to=today returns today.
// Returns the resolved bounds plus the equivalent day count for display.
func resolveWindow(c *gin.Context, defaultDays, maxDays int) (since, until time.Time, days int) {
	now := time.Now().UTC()
	until = now

	fromRaw := strings.TrimSpace(c.Query("from"))
	toRaw := strings.TrimSpace(c.Query("to"))

	if fromRaw != "" {
		if t, err := parseDay(fromRaw); err == nil {
			since = t

			// A `from` with no usable `to` means "that day onwards, up to
			// today". Anchoring the end to the start of tomorrow rather than
			// leaving it at time.Now() keeps the window a whole number of days
			// and stops a saved link from silently widening every time it is
			// opened.
			until = parseDayOf(now).AddDate(0, 0, 1)

			if toRaw != "" {
				if u, err := parseDay(toRaw); err == nil {
					// Inclusive end: advance to the start of the next day.
					until = u.AddDate(0, 0, 1)
				}
			}
			if until.Before(since) {
				since, until = until, since
			}

			// until is already the start of the day *after* the range, so the
			// difference is the day count — adding one more would report a
			// single-day window as two days.
			return since, until, int(until.Sub(since).Hours() / 24)
		}
	}

	days = defaultDays
	if raw := c.Query("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			days = n
		}
	}
	if days > maxDays {
		days = maxDays
	}

	return now.AddDate(0, 0, -days), until, days
}

// parseDay accepts a plain calendar date (2026-08-29) or a full RFC3339
// timestamp, so the UI can send either.
func parseDay(raw string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC(), nil
	}
	return time.Parse(time.RFC3339, raw)
}

// parseDayOf truncates a timestamp to the start of its UTC day, so a window
// built from it covers whole days rather than a partial one ending at whatever
// time the request happened to arrive.
func parseDayOf(t time.Time) time.Time {
	u := t.UTC()

	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// --- Sitemap Discovery ---

type discoverSitemapRequest struct {
	BaseURL   string `json:"base_url" binding:"required"`
	UserAgent string `json:"user_agent"`
}

// DiscoverSitemap finds a site's sitemap so the customer does not have to know
// where it lives. It is a read-only probe: nothing is stored, and the caller
// decides whether to use what comes back.
//
// The URL is normalised before validation because customers type bare domains
// ("billigfilter.dk"), not full URLs.
func (h *Handler) DiscoverSitemap(c *gin.Context) {
	var req discoverSitemapRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	normalised, err := source.NormalizeSiteURL(req.BaseURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// The same guard every other user-supplied target goes through: without it
	// this endpoint would fetch arbitrary internal addresses on request.
	if err := source.ValidateTargetURL(normalised, h.cfg.AllowPrivateTargets); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userAgent := strings.TrimSpace(req.UserAgent)
	if userAgent == "" {
		userAgent = h.cfg.DefaultUserAgent
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()

	candidates, err := h.parser.FindSitemaps(ctx, normalised, userAgent, h.cfg.AllowPrivateTargets)
	if err != nil {
		log.Error().Err(err).Str("base_url", normalised).Msg("sitemap discovery failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not reach the site"})
		return
	}

	// Report the winner separately so the caller does not have to re-derive it
	// from the candidate list.
	var best *source.SitemapCandidate
	for i := range candidates {
		if candidates[i].Found && candidates[i].URLCount > 0 {
			best = &candidates[i]
			break
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"base_url":   normalised,
		"found":      best != nil,
		"sitemap":    best,
		"candidates": candidates,
	})
}

// --- Live Feed ---

// TailCrawledURLs streams what a running crawl has just checked.
//
// Kept deliberately small: the caller polls with the last id it saw and gets
// only what has landed since, so a long-running crawl does not re-send its
// whole history on every poll.
func (h *Handler) TailCrawledURLs(c *gin.Context) {
	oid, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid crawling ID", Code: "INVALID_ID"})
		return
	}

	limit, _ := strconv.ParseInt(c.DefaultQuery("limit", "50"), 10, 64)
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var after primitive.ObjectID
	if raw := c.Query("after"); raw != "" {
		parsed, err := primitive.ObjectIDFromHex(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid after cursor", Code: "INVALID_CURSOR"})
			return
		}
		after = parsed
	}

	results, err := h.repo.TailCrawlingResults(c.Request.Context(), oid, after, limit)
	if err != nil {
		log.Error().Err(err).Str("crawling_id", c.Param("id")).Msg("failed to tail crawl results")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to read results", Code: "INTERNAL"})
		return
	}

	// The caller polls with this rather than tracking ids itself.
	cursor := c.Query("after")
	if len(results) > 0 {
		cursor = results[len(results)-1].ID.Hex()
	}

	crawling, err := h.repo.GetCrawling(c.Request.Context(), oid)
	status := ""
	if err == nil && crawling != nil {
		status = string(crawling.Status)
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   results,
		"cursor": cursor,
		"status": status,
	})
}

// --- External Link Checking ---

// CheckSiteLinks verifies a batch of the site's outbound links.
//
// Deliberately batched and separate from crawling: these requests go to third
// parties, so they run on their own budget and are spread over repeated calls
// rather than hitting every destination at once. Callers poll until `remaining`
// reaches zero.
func (h *Handler) CheckSiteLinks(c *gin.Context) {
	siteID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	site, err := h.repo.GetSite(c.Request.Context(), siteID)
	if err != nil || site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "site not found", Code: "NOT_FOUND"})
		return
	}

	limit, _ := strconv.ParseInt(c.DefaultQuery("limit", "50"), 10, 64)
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	// A destination checked recently is not worth re-checking: the answer will
	// not have changed, and it is someone else's server.
	maxAge, _ := strconv.Atoi(c.DefaultQuery("max_age_hours", "24"))
	if maxAge <= 0 {
		maxAge = 24
	}
	cutoff := time.Now().Add(-time.Duration(maxAge) * time.Hour)

	pending, err := h.repo.OutboundLinksToCheck(c.Request.Context(), siteID, cutoff, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to list links to check")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to read links", Code: "INTERNAL"})
		return
	}

	if len(pending) == 0 {
		c.JSON(http.StatusOK, gin.H{"checked": 0, "broken": 0, "remaining": 0})
		return
	}

	urls := make([]string, len(pending))
	for i, l := range pending {
		urls[i] = l.URL
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()

	checker := linkcheck.New(site.UserAgent, 15*time.Second)
	results := checker.CheckAll(ctx, urls, h.cfg.LinkCheckConcurrency)

	broken := 0
	for i, res := range results {
		if err := h.repo.SaveLinkCheck(ctx, pending[i].ID, res.StatusCode, res.Error, res.ResponseTime.Milliseconds()); err != nil {
			log.Warn().Err(err).Str("url", res.URL).Msg("failed to save link check")
		}
		// Counted the same way models.OutboundLink.Broken() decides, so the
		// number here matches what /links/broken later lists.
		if res.Error != "" || (res.StatusCode >= 400 && !linkcheck.BotBlocked(res.StatusCode)) {
			broken++
		}
	}

	// What is still unchecked after this batch, so the caller knows to continue.
	stillPending, err := h.repo.OutboundLinksToCheck(c.Request.Context(), siteID, cutoff, 1000)
	remaining := 0
	if err == nil {
		remaining = len(stillPending)
	}

	c.JSON(http.StatusOK, gin.H{
		"checked":   len(results),
		"broken":    broken,
		"remaining": remaining,
	})
}

// GetBrokenLinks lists the site's outbound links whose last check failed.
func (h *Handler) GetBrokenLinks(c *gin.Context) {
	siteID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	limit, _ := strconv.ParseInt(c.DefaultQuery("limit", "100"), 10, 64)
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	links, err := h.repo.BrokenOutboundLinks(c.Request.Context(), siteID, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to list broken links")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to read links", Code: "INTERNAL"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": links, "count": len(links)})
}

// resolveSmartSource locates the sitemap a smart-source site should crawl.
//
// The result is not stored on the site: a customer who moves from Yoast to
// RankMath gets the new location on the next crawl without anyone editing
// anything, which is the point of the mode.
func (h *Handler) resolveSmartSource(ctx context.Context, site *models.Site) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	candidates, err := h.parser.FindSitemaps(ctx, site.BaseURL, site.UserAgent, h.cfg.AllowPrivateTargets)
	if err != nil {
		return "", err
	}

	for _, c := range candidates {
		if c.Found && c.URLCount > 0 {
			return c.URL, nil
		}
	}

	return "", fmt.Errorf("no sitemap found at %s", site.BaseURL)
}

// useStoredURLList enqueues a site's saved URL list, if there is a current
// one, and reports whether it did.
//
// Returning false means the caller should derive the list the slow way: fetch
// the source, parse the pages, harvest the assets — and store the result as it
// goes, so the next crawl can take this path instead.
func (h *Handler) useStoredURLList(
	ctx context.Context,
	logger zerolog.Logger,
	crawlingID string,
	oid primitive.ObjectID,
	site *models.Site,
	crawling *models.Crawling,
) bool {
	// Never built, or aged out. A list that is too old stops reflecting the
	// site: pages get added, images get replaced, and nobody would think to
	// press refresh.
	if site.URLsBuiltAt == nil || time.Since(*site.URLsBuiltAt) > models.SiteURLListAge {
		return false
	}

	stored, err := h.repo.SiteURLList(ctx, site.ID, int64(site.URLLimit))
	if err != nil {
		logger.Error().Err(err).Msg("could not read the stored URL list; rebuilding")
		return false
	}
	if len(stored) == 0 {
		return false
	}

	if err := h.rateLimiter.Init(ctx, crawlingID, crawling.Speed); err != nil {
		logger.Error().Err(err).Msg("failed to init rate limiter")
		_ = h.repo.SetCrawlingError(ctx, oid, "failed to init rate limiter")
		return true
	}

	// Smart recrawl still applies: a stored list says what to crawl, not what
	// is already warm.
	cached := map[string]models.CrawlingResult{}
	if site.SmartRecrawl {
		if found, err := h.repo.CachedResultsFromLastCrawl(ctx, site.ID, oid); err == nil {
			cached = found
		}
	}

	var (
		tasks        []models.CrawlTask
		carryForward []models.CrawlingResult
	)

	for _, su := range stored {
		if !allowsURL(crawling.URLType, su.URL) {
			continue
		}

		isNew, err := h.dedup.MarkSeen(ctx, crawlingID, su.URLHash)
		if err != nil || !isNew {
			continue
		}

		if prev, ok := cached[su.URL]; ok {
			carryForward = append(carryForward, prev)
			continue
		}

		tasks = append(tasks, models.CrawlTask{
			CrawlingID:  crawlingID,
			SiteID:      site.ID.Hex(),
			URL:         su.URL,
			URLHash:     su.URLHash,
			UserAgent:   site.UserAgent,
			ExtractData: site.ExtractData,
			MaxRetries:  h.cfg.CrawlerMaxRetries,
			EnqueuedAt:  time.Now().Unix(),
		})
	}

	if len(carryForward) > 0 {
		if err := h.repo.CarryForwardResults(ctx, oid, carryForward); err != nil {
			logger.Error().Err(err).Msg("failed to carry results forward")
		}
	}

	if len(tasks) == 0 && len(carryForward) == 0 {
		return false
	}

	for i := 0; i < len(tasks); i += 1000 {
		end := min(i+1000, len(tasks))
		if err := h.queue.EnqueueBatch(ctx, crawlingID, tasks[i:end]); err != nil {
			logger.Error().Err(err).Msg("failed to enqueue batch from the stored list")
		}
	}

	_ = h.repo.SetCrawlingTotalURLs(ctx, oid, len(tasks)+len(carryForward))

	logger.Info().
		Int("from_stored_list", len(tasks)).
		Int("carried_forward", len(carryForward)).
		Time("list_built", *site.URLsBuiltAt).
		Msg("crawling from the stored URL list")

	if len(tasks) == 0 {
		// Everything was still cached; nothing to fetch.
		_ = h.repo.SetCrawlingCrawledURLs(ctx, oid, len(carryForward))
		_ = h.repo.UpdateCrawlingStatus(ctx, oid, models.CrawlStatusCompleted)
		_ = h.stateManager.SetState(ctx, crawlingID, models.CrawlStatusCompleted)
		return true
	}

	_ = h.repo.UpdateCrawlingStatus(ctx, oid, models.CrawlStatusRunning)
	_ = h.stateManager.SetState(ctx, crawlingID, models.CrawlStatusRunning)
	_ = h.stateManager.AddActiveCrawling(ctx, crawlingID)

	metrics.ActiveCrawlingsGauge.Inc()

	return true
}

// GetSiteURLList reports what is in a site's stored URL list.
func (h *Handler) GetSiteURLList(c *gin.Context) {
	siteID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	site, err := h.repo.GetSite(c.Request.Context(), siteID)
	if err != nil || site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "site not found", Code: "NOT_FOUND"})
		return
	}

	pages, assets, err := h.repo.CountSiteURLs(c.Request.Context(), siteID)
	if err != nil {
		log.Error().Err(err).Msg("failed to count stored URLs")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to read the list", Code: "INTERNAL"})
		return
	}

	stale := site.URLsBuiltAt == nil || time.Since(*site.URLsBuiltAt) > models.SiteURLListAge

	c.JSON(http.StatusOK, gin.H{
		"pages":    pages,
		"assets":   assets,
		"total":    pages + assets,
		"built_at": site.URLsBuiltAt,
		// stale means the next crawl rebuilds the list rather than reusing it.
		"stale": stale,
	})
}

// RefreshSiteURLList discards a site's stored list so the next crawl rebuilds
// it from the source.
//
// The rows are removed rather than left to be overwritten: a URL that no
// longer exists on the site would otherwise linger indefinitely, since nothing
// would refresh its last_seen_at.
func (h *Handler) RefreshSiteURLList(c *gin.Context) {
	siteID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	if err := h.repo.ClearSiteURLs(c.Request.Context(), siteID); err != nil {
		log.Error().Err(err).Msg("failed to clear the stored URL list")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to clear the list", Code: "INTERNAL"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"cleared": true,
		"message": "The URL list will be rebuilt on the next crawl.",
	})
}
