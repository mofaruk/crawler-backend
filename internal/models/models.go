package models

import (
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

// --- API Request/Response DTOs ---

type CreateSiteRequest struct {
	Name          string `json:"name" binding:"required"`
	BaseURL       string `json:"base_url" binding:"required,url"`
	URLLimit      int    `json:"url_limit" binding:"required,min=1"`
	URLSource     string `json:"url_source" binding:"omitempty,url"` // required unless url_source_type is "auto"
	URLSourceType string `json:"url_source_type" binding:"required,oneof=csv xml auto"`
	UserAgent     string `json:"user_agent"`
	ExtractData   string `json:"extract_data"` // comma-separated header names
}

type UpdateSiteRequest struct {
	Name          *string `json:"name"`
	BaseURL       *string `json:"base_url" binding:"omitempty,url"`
	URLLimit      *int    `json:"url_limit" binding:"omitempty,min=1"`
	URLSource     *string `json:"url_source" binding:"omitempty,url"`
	URLSourceType *string `json:"url_source_type" binding:"omitempty,oneof=csv xml auto"`
	UserAgent     *string `json:"user_agent"`
	ExtractData   *string `json:"extract_data"`
}

type StartCrawlingRequest struct {
	SiteID       string `json:"site_id" binding:"required"`
	Speed        int    `json:"speed"`
	ReloadSource bool   `json:"reload_source"`
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
