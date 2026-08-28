package models

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// --- Crawl Job States ---

type CrawlStatus string

const (
	CrawlStatusPending     CrawlStatus = "pending"
	CrawlStatusDiscovering CrawlStatus = "discovering"
	CrawlStatusRunning     CrawlStatus = "running"
	CrawlStatusPaused      CrawlStatus = "paused"
	CrawlStatusStopped     CrawlStatus = "stopped"
	CrawlStatusCompleted   CrawlStatus = "completed"
	CrawlStatusFailed      CrawlStatus = "failed"
)

// URL source types for a Site.
const (
	URLSourceTypeCSV  = "csv"
	URLSourceTypeXML  = "xml"
	URLSourceTypeAuto = "auto" // auto-discover by crawling base_url
)

// CrawlURLType narrows which URLs a crawling job actually fetches. Classified
// at ingestion time using the URL's path extension, so the response Content-Type
// is irrelevant — this is a *pre-fetch* filter, distinct from the post-fetch
// content_type filter on the URL list view.
const (
	CrawlURLTypeAll     = "all"     // default — crawl everything
	CrawlURLTypeStatic  = "static"  // only URLs with static-asset extensions
	CrawlURLTypeDynamic = "dynamic" // only URLs without static-asset extensions
)

// --- Site ---

type Site struct {
	ID            primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Name          string             `bson:"name" json:"name"`
	BaseURL       string             `bson:"base_url" json:"base_url"`
	URLLimit      int                `bson:"url_limit" json:"url_limit"`
	URLSource     string             `bson:"url_source" json:"url_source"`
	URLSourceType string             `bson:"url_source_type" json:"url_source_type"` // "csv" or "xml"
	UserAgent     string             `bson:"user_agent" json:"user_agent"`
	ExtractData   []string           `bson:"extract_data" json:"extract_data"` // HTTP header names to extract
	// SmartRecrawl skips URLs the previous round found already cached, and
	// carries their last result forward. On a site that is 95% cached this
	// avoids almost all the work — at the cost of not learning whether those
	// pages are *still* cached, which is why it is opt-in per site.
	SmartRecrawl  bool               `bson:"smart_recrawl" json:"smart_recrawl"`
	CreatedAt     time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt     time.Time          `bson:"updated_at" json:"updated_at"`
}

// --- Crawling (Job) ---

type Crawling struct {
	ID             primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	SiteID         primitive.ObjectID `bson:"site_id" json:"site_id"`
	Status         CrawlStatus        `bson:"status" json:"status"`
	Speed          int                `bson:"speed" json:"speed"`                       // URLs per hour
	ReloadSource   bool               `bson:"reload_source" json:"reload_source"`
	URLType        string             `bson:"url_type,omitempty" json:"url_type,omitempty"` // "all" | "static" | "dynamic"
	TotalURLs      int                `bson:"total_urls" json:"total_urls"`
	CrawledURLs    int                `bson:"crawled_urls" json:"crawled_urls"`
	FailedURLs     int                `bson:"failed_urls" json:"failed_urls"`
	StartedAt      *time.Time         `bson:"started_at,omitempty" json:"started_at,omitempty"`
	CompletedAt    *time.Time         `bson:"completed_at,omitempty" json:"completed_at,omitempty"`
	PausedAt       *time.Time         `bson:"paused_at,omitempty" json:"paused_at,omitempty"`
	CreatedAt      time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt      time.Time          `bson:"updated_at" json:"updated_at"`
	ErrorMessage   string             `bson:"error_message,omitempty" json:"error_message,omitempty"`
}

// --- Crawl URL ---

type URLStatus string

const (
	URLStatusPending    URLStatus = "pending"
	URLStatusProcessing URLStatus = "processing"
	URLStatusCompleted  URLStatus = "completed"
	URLStatusFailed     URLStatus = "failed"
	URLStatusDead       URLStatus = "dead" // exceeded max retries
)

type CrawlURL struct {
	ID         primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CrawlingID primitive.ObjectID `bson:"crawling_id" json:"crawling_id"`
	SiteID     primitive.ObjectID `bson:"site_id" json:"site_id"`
	URL        string             `bson:"url" json:"url"`
	URLHash    string             `bson:"url_hash" json:"url_hash"` // SHA-256 for dedup
	Status     URLStatus          `bson:"status" json:"status"`
	Retries    int                `bson:"retries" json:"retries"`
	CreatedAt  time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt  time.Time          `bson:"updated_at" json:"updated_at"`
}

// --- Crawling Result ---

type CrawlingResult struct {
	ID           primitive.ObjectID     `bson:"_id,omitempty" json:"id"`
	CrawlingID   primitive.ObjectID     `bson:"crawling_id" json:"crawling_id"`
	SiteID       primitive.ObjectID     `bson:"site_id" json:"site_id"`
	URL          string                 `bson:"url" json:"url"`
	StatusCode   int                    `bson:"status_code" json:"status_code"`
	Headers      map[string]string      `bson:"headers" json:"headers"`        // extracted headers
	BodyData     map[string]interface{} `bson:"body_data" json:"body_data"`    // extracted body fields
	ContentType  string                 `bson:"content_type" json:"content_type"`
	ResponseTime int64                  `bson:"response_time_ms" json:"response_time_ms"`
	CrawledAt    time.Time              `bson:"crawled_at" json:"crawled_at"`
	// RedirectedTo is the final URL when the request was redirected.
	RedirectedTo string `bson:"redirected_to,omitempty" json:"redirected_to,omitempty"`
	// Page holds on-page signals parsed from an HTML body; nil for assets.
	Page *PageSignals `bson:"page,omitempty" json:"page,omitempty"`
	// CarriedForward marks a result copied from the previous round rather
	// than fetched in this one, because smart recrawl skipped the URL. Without
	// the flag a stale row is indistinguishable from a fresh one.
	CarriedForward bool `bson:"carried_forward,omitempty" json:"carried_forward,omitempty"`
	// OriginalCrawledAt is when the carried-forward result was actually
	// fetched. CrawledAt stays the current round so ordering and windowing
	// still work.
	OriginalCrawledAt *time.Time `bson:"original_crawled_at,omitempty" json:"original_crawled_at,omitempty"`
}

// PageSignals mirrors crawler.PageSignals for storage. Duplicated rather than
// imported so the models package stays dependency-free.
type PageSignals struct {
	Title            string `bson:"title,omitempty" json:"title,omitempty"`
	TitleLength      int    `bson:"title_length,omitempty" json:"title_length,omitempty"`
	MetaDescription  string `bson:"meta_description,omitempty" json:"meta_description,omitempty"`
	MetaDescLength   int    `bson:"meta_desc_length,omitempty" json:"meta_desc_length,omitempty"`
	Canonical        string `bson:"canonical,omitempty" json:"canonical,omitempty"`
	NoIndex          bool   `bson:"noindex,omitempty" json:"noindex,omitempty"`
	H1Count          int    `bson:"h1_count,omitempty" json:"h1_count,omitempty"`
	WordCount        int    `bson:"word_count,omitempty" json:"word_count,omitempty"`
	ImagesMissingAlt int    `bson:"images_missing_alt,omitempty" json:"images_missing_alt,omitempty"`
	InsecureRefs     int    `bson:"insecure_refs,omitempty" json:"insecure_refs,omitempty"`
	SoftNotFound     bool   `bson:"soft_not_found,omitempty" json:"soft_not_found,omitempty"`
}

// --- Crawl Failure ---

type CrawlFailure struct {
	ID         primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CrawlingID primitive.ObjectID `bson:"crawling_id" json:"crawling_id"`
	SiteID     primitive.ObjectID `bson:"site_id" json:"site_id"`
	URL        string             `bson:"url" json:"url"`
	Error      string             `bson:"error" json:"error"`
	StatusCode int                `bson:"status_code,omitempty" json:"status_code,omitempty"`
	Retries    int                `bson:"retries" json:"retries"`
	FailedAt   time.Time          `bson:"failed_at" json:"failed_at"`
}

// --- Queue Task (serialized into Redis) ---

type CrawlTask struct {
	CrawlingID  string   `json:"crawling_id"`
	SiteID      string   `json:"site_id"`
	URL         string   `json:"url"`
	URLHash     string   `json:"url_hash"`
	UserAgent   string   `json:"user_agent"`
	ExtractData []string `json:"extract_data"`
	Retries     int      `json:"retries"`
	MaxRetries  int      `json:"max_retries"`
	EnqueuedAt  int64    `json:"enqueued_at"`
}

// --- External Links ---

// OutboundLink is one external destination a site links to, with the pages it
// was found on.
//
// Deduplicated per site rather than per page: a dead supplier link in the
// footer appears on every page, and checking it four hundred times would be
// four hundred requests against someone else's server to learn one fact.
type OutboundLink struct {
	ID     primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	SiteID primitive.ObjectID `bson:"site_id" json:"site_id"`
	URL    string             `bson:"url" json:"url"`

	// FoundOn lists the pages linking here, capped when stored — the count is
	// what matters at scale, not the full list.
	FoundOn      []string `bson:"found_on" json:"found_on"`
	FoundOnCount int      `bson:"found_on_count" json:"found_on_count"`

	// Check results, set by the link checker rather than the crawl.
	StatusCode   int        `bson:"status_code,omitempty" json:"status_code,omitempty"`
	Error        string     `bson:"error,omitempty" json:"error,omitempty"`
	ResponseTime int64      `bson:"response_time_ms,omitempty" json:"response_time_ms,omitempty"`
	CheckedAt    *time.Time `bson:"checked_at,omitempty" json:"checked_at,omitempty"`

	FirstSeenAt time.Time `bson:"first_seen_at" json:"first_seen_at"`
	LastSeenAt  time.Time `bson:"last_seen_at" json:"last_seen_at"`
}

// Broken reports whether the last check found the destination unreachable.
//
// A transport error or a 404/410/5xx counts. Redirects do not: a link that
// redirects still gets the visitor somewhere.
//
// Statuses that usually mean "we block bots" (400, 403, 429) are deliberately
// excluded — social platforms answer those to any non-browser request while
// serving people fine, and reporting a customer's own Facebook page as broken
// would discredit the whole report.
func (l OutboundLink) Broken() bool {
	if l.CheckedAt == nil {
		return false
	}

	if l.Error != "" {
		return true
	}

	switch l.StatusCode {
	case 400, 403, 429, 451:
		return false
	}

	return l.StatusCode >= 400
}

// --- API Request/Response DTOs ---

type CreateSiteRequest struct {
	Name          string `json:"name" binding:"required"`
	BaseURL       string `json:"base_url" binding:"required,url"`
	URLLimit      int    `json:"url_limit" binding:"required,min=1"`
	URLSource     string `json:"url_source" binding:"omitempty,url"` // required unless url_source_type is "auto"
	URLSourceType string `json:"url_source_type" binding:"required,oneof=csv xml auto"`
	UserAgent     string `json:"user_agent"`
	ExtractData   string `json:"extract_data"` // comma-separated header names
	SmartRecrawl  bool   `json:"smart_recrawl"`
}

type UpdateSiteRequest struct {
	Name          *string `json:"name"`
	BaseURL       *string `json:"base_url" binding:"omitempty,url"`
	URLLimit      *int    `json:"url_limit" binding:"omitempty,min=1"`
	URLSource     *string `json:"url_source" binding:"omitempty,url"`
	URLSourceType *string `json:"url_source_type" binding:"omitempty,oneof=csv xml auto"`
	UserAgent     *string `json:"user_agent"`
	ExtractData   *string `json:"extract_data"`
	SmartRecrawl  *bool   `json:"smart_recrawl"`
}

type StartCrawlingRequest struct {
	SiteID       string `json:"site_id" binding:"required"`
	Speed        int    `json:"speed"`
	ReloadSource bool   `json:"reload_source"`
	URLType      string `json:"url_type" binding:"omitempty,oneof=all static dynamic"`
}

// PruneCrawlingsRequest purges a site's finished crawl rounds (and their
// results/URLs/failures) created before the given cutoff. Used by the
// dashboard's package-driven data-retention job. Before is RFC3339.
type PruneCrawlingsRequest struct {
	SiteID string `json:"site_id" binding:"required"`
	Before string `json:"before" binding:"required"`
}

type CrawlProgressResponse struct {
	ID          string      `json:"id"`
	SiteID      string      `json:"site_id"`
	Status      CrawlStatus `json:"status"`
	TotalURLs   int         `json:"total_urls"`
	CrawledURLs int         `json:"crawled_urls"`
	FailedURLs  int         `json:"failed_urls"`
	Progress    float64     `json:"progress_percent"`
	Speed       int         `json:"speed"`
	StartedAt   *time.Time  `json:"started_at,omitempty"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
}

type HeaderValueCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// --- Site Issues (error detection) ---

// URLState is the current state of one URL, assembled from its most recent
// crawl result. It is the input to classification.
type URLState struct {
	URL          string            `bson:"url"`
	StatusCode   int               `bson:"status_code"`
	ContentType  string            `bson:"content_type"`
	ResponseTime int64             `bson:"response_time"`
	RedirectedTo string            `bson:"redirected_to"`
	Headers      map[string]string `bson:"headers"`
	Page         *PageSignals      `bson:"page"`
	FirstSeen    time.Time         `bson:"first_seen"`
	LastSeen     time.Time         `bson:"last_seen"`
	Occurrences  int               `bson:"occurrences"`
}

// Severity orders issues for display. Higher is worse.
const (
	SeverityInfo     = 1 // worth knowing, not broken
	SeverityWarning  = 2 // degrades quality or performance
	SeverityCritical = 3 // visitors or search engines see something broken
)

// SiteIssue is one problem found at one URL. A single URL can produce several
// (a slow page that is also uncached and missing a title), so issues are
// reported per finding rather than per URL.
type SiteIssue struct {
	URL         string    `json:"url"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`             // human-readable summary
	Detail      string    `json:"detail,omitempty"`  // the specific value found
	Severity    int       `json:"severity"`
	StatusCode  int       `json:"status_code,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Occurrences int       `json:"occurrences"`
}

// Thresholds for the quality checks. Chosen to flag real problems rather than
// stylistic preferences: a 4-second page is slow by any standard, whereas a
// 55-character title is fine even though SEO tools like 50-60.
const (
	slowPageMs      = 2000
	verySlowPageMs  = 5000
	shortTitleChars = 10
	longTitleChars  = 70
	thinContentWords = 100
)

// ClassifyURL turns one URL's current state into zero or more issues.
//
// Every check here runs on data the crawler already stored, so breadth costs
// an aggregation rather than another crawl.
func ClassifyURL(s URLState, titleCounts map[string]int) []SiteIssue {
	var out []SiteIssue

	add := func(kind, title, detail string, severity int) {
		out = append(out, SiteIssue{
			URL: s.URL, Kind: kind, Title: title, Detail: detail,
			Severity: severity, StatusCode: s.StatusCode,
			FirstSeen: s.FirstSeen, LastSeen: s.LastSeen, Occurrences: s.Occurrences,
		})
	}

	// --- Availability ---
	switch {
	case s.StatusCode >= 500:
		add("server_error", "Server error", fmt.Sprintf("Returns HTTP %d", s.StatusCode), SeverityCritical)
	case s.StatusCode == 410:
		add("gone", "Page permanently gone", "Returns HTTP 410", SeverityWarning)
	case s.StatusCode >= 400:
		add("broken", "Broken page", fmt.Sprintf("Returns HTTP %d", s.StatusCode), SeverityCritical)
	case s.StatusCode == 0:
		add("unreachable", "Could not be reached", "No response from the server", SeverityCritical)
	}

	// A 200 that reads as an error page is worse than a real 404: search
	// engines index it and visitors get no signal the link is dead.
	if s.Page != nil && s.Page.SoftNotFound && s.StatusCode == 200 {
		add("soft_404", "Looks like an error page but returns OK",
			"Title: "+s.Page.Title, SeverityCritical)
	}

	// --- Redirects ---
	if s.RedirectedTo != "" {
		add("redirect", "Redirects elsewhere", "Now serves "+s.RedirectedTo, SeverityInfo)
	}

	// --- Performance ---
	switch {
	case s.ResponseTime >= verySlowPageMs:
		add("very_slow", "Very slow to load",
			fmt.Sprintf("Took %.1fs", float64(s.ResponseTime)/1000), SeverityCritical)
	case s.ResponseTime >= slowPageMs:
		add("slow", "Slow to load",
			fmt.Sprintf("Took %.1fs", float64(s.ResponseTime)/1000), SeverityWarning)
	}

	// --- Caching (the product's core subject) ---
	cacheStatus := headerLookup(s.Headers, "CF-Cache-Status")
	switch strings.ToUpper(cacheStatus) {
	case "BYPASS":
		add("cache_bypass", "Never cached", "CDN is bypassing the cache for this URL", SeverityWarning)
	case "DYNAMIC":
		add("cache_dynamic", "Not cacheable", "CDN treats this URL as dynamic", SeverityWarning)
	case "EXPIRED":
		add("cache_expired", "Cache expired", "Served from origin because the cached copy expired", SeverityInfo)
	}

	// A cached copy older than the policy allows. This is the failure the
	// product exists to catch and it hides behind a healthy-looking cache
	// percentage: the page IS cached, it is just serving content the origin
	// stopped vouching for.
	if age, maxAge, ok := cacheFreshness(s.Headers); ok && age > maxAge {
		over := age - maxAge

		// Warn only past a margin. A copy a few seconds beyond its lifetime is
		// normal CDN behaviour, not a fault worth reporting on every page.
		if over > staleGraceSeconds {
			severity := SeverityWarning
			if age > maxAge*staleCriticalMultiple {
				severity = SeverityCritical
			}

			add("cache_stale", "Serving stale content",
				fmt.Sprintf("Cached copy is %s old but the policy allows %s",
					humanDuration(age), humanDuration(maxAge)),
				severity)
		}
	}

	if s.StatusCode == 200 && headerLookup(s.Headers, "Cache-Control") == "" && cacheStatus != "" {
		add("no_cache_control", "No caching policy",
			"The origin sends no Cache-Control header, so the CDN must guess", SeverityWarning)
	}

	// --- Page quality (HTML only) ---
	if p := s.Page; p != nil && s.StatusCode == 200 {
		switch {
		case p.Title == "":
			add("missing_title", "No page title", "Search results will show a URL instead of a name", SeverityWarning)
		case p.TitleLength < shortTitleChars:
			add("short_title", "Very short page title", "Title: "+p.Title, SeverityInfo)
		case p.TitleLength > longTitleChars:
			add("long_title", "Page title will be truncated",
				fmt.Sprintf("%d characters; search results show about %d", p.TitleLength, longTitleChars), SeverityInfo)
		}

		if p.Title != "" && titleCounts[strings.ToLower(p.Title)] > 1 {
			add("duplicate_title", "Duplicate page title",
				fmt.Sprintf("%d pages share the title %q", titleCounts[strings.ToLower(p.Title)], p.Title), SeverityWarning)
		}

		if p.MetaDescription == "" {
			add("missing_meta_description", "No meta description",
				"Search engines will invent a snippet for this page", SeverityInfo)
		}

		if p.NoIndex {
			add("noindex", "Hidden from search engines",
				"This page carries a noindex tag", SeverityCritical)
		}

		if p.Canonical == "" {
			add("missing_canonical", "No canonical URL",
				"Duplicate versions of this page may compete with each other", SeverityInfo)
		}

		switch {
		case p.H1Count == 0:
			add("missing_h1", "No main heading", "The page has no <h1>", SeverityInfo)
		case p.H1Count > 1:
			add("multiple_h1", "Several main headings",
				fmt.Sprintf("%d <h1> elements", p.H1Count), SeverityInfo)
		}

		if p.WordCount > 0 && p.WordCount < thinContentWords {
			add("thin_content", "Very little content",
				fmt.Sprintf("About %d words", p.WordCount), SeverityInfo)
		}

		if p.InsecureRefs > 0 {
			add("mixed_content", "Loads insecure resources",
				fmt.Sprintf("%d http:// resources on an https page; browsers block these", p.InsecureRefs), SeverityCritical)
		}

		if p.ImagesMissingAlt > 0 {
			add("images_missing_alt", "Images without alt text",
				fmt.Sprintf("%d images", p.ImagesMissingAlt), SeverityInfo)
		}
	}

	return out
}

// headerLookup finds a header case-insensitively — servers vary in casing and
// Mongo field paths are exact.
func headerLookup(headers map[string]string, name string) string {
	if headers == nil {
		return ""
	}
	if v, ok := headers[name]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// TimelinePoint is one crawl of a site, reduced to the handful of numbers
// worth plotting against time.
type TimelinePoint struct {
	CrawlingID       string    `json:"crawling_id"`
	CrawledAt        time.Time `json:"crawled_at"`
	URLs             int       `json:"urls"`
	CachePercent     float64   `json:"cache_percent"`
	MedianAgeSeconds int64     `json:"median_age_seconds"`
	AvgResponseMs    int64     `json:"avg_response_ms"`
	Errors           int       `json:"errors"`
}

// Stale-cache thresholds.
const (
	// A copy slightly past its lifetime is ordinary CDN behaviour — the
	// refresh has not happened yet. Only a meaningful overrun is a fault.
	staleGraceSeconds = 60

	// Far past the policy stops being a lag and becomes content the origin
	// stopped vouching for a long time ago.
	staleCriticalMultiple = 10
)

// cacheFreshness reports how old the served copy is and how old the policy
// permits, in seconds. ok is false when either cannot be determined.
//
// s-maxage takes precedence over max-age because it is the directive aimed at
// shared caches, which is exactly what a CDN is. Comparing Age against max-age
// alone reports false positives on the common WordPress/Cloudflare setup of a
// short browser lifetime with a long CDN one — "s-maxage=31557600,
// max-age=600" is a correctly cached page, not a stale one.
func cacheFreshness(headers map[string]string) (age, maxAge int, ok bool) {
	rawAge := strings.TrimSpace(headerLookup(headers, "Age"))
	if rawAge == "" {
		return 0, 0, false
	}

	age, err := strconv.Atoi(rawAge)
	if err != nil || age < 0 {
		return 0, 0, false
	}

	cc := strings.ToLower(headerLookup(headers, "Cache-Control"))
	if cc == "" {
		return 0, 0, false
	}

	// A copy the origin forbids caching cannot be judged against a lifetime;
	// cache_bypass and friends already cover that case.
	if strings.Contains(cc, "no-store") || strings.Contains(cc, "no-cache") {
		return 0, 0, false
	}

	if v, found := cacheDirectiveSeconds(cc, "s-maxage"); found {
		return age, v, true
	}
	if v, found := cacheDirectiveSeconds(cc, "max-age"); found {
		return age, v, true
	}

	return 0, 0, false
}

// cacheDirectiveSeconds pulls one "name=seconds" directive out of a
// Cache-Control value.
func cacheDirectiveSeconds(cacheControl, name string) (int, bool) {
	for _, part := range strings.Split(cacheControl, ",") {
		part = strings.TrimSpace(part)

		// Match the whole directive name: "max-age" must not match inside
		// "s-maxage", which is a different (and higher-priority) directive.
		rest, found := strings.CutPrefix(part, name+"=")
		if !found {
			continue
		}

		if v, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil && v >= 0 {
			return v, true
		}
	}

	return 0, false
}

// humanDuration renders a second count the way a person would say it.
func humanDuration(seconds int) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%.1fh", float64(seconds)/3600)
	default:
		return fmt.Sprintf("%.1f days", float64(seconds)/86400)
	}
}
