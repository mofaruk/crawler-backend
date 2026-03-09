package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metrics for the crawler system.

var (
	// --- Crawl Operations ---

	URLsCrawledTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crawler_urls_crawled_total",
		Help: "Total number of URLs crawled",
	}, []string{"crawling_id", "status"})

	CrawlDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "crawler_url_fetch_duration_seconds",
		Help:    "HTTP fetch duration per URL",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"crawling_id"})

	CrawlErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crawler_errors_total",
		Help: "Total crawl errors by type",
	}, []string{"crawling_id", "error_type"})

	// --- Queue Metrics ---

	QueuePendingGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "crawler_queue_pending",
		Help: "Number of pending URLs in queue",
	}, []string{"crawling_id"})

	QueueProcessingGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "crawler_queue_processing",
		Help: "Number of URLs currently being processed",
	}, []string{"crawling_id"})

	QueueRetryGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "crawler_queue_retry",
		Help: "Number of URLs in retry queue",
	}, []string{"crawling_id"})

	QueueDeadGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "crawler_queue_dead",
		Help: "Number of URLs in dead letter queue",
	}, []string{"crawling_id"})

	// --- Worker Metrics ---

	WorkerActiveGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crawler_workers_active",
		Help: "Number of active worker goroutines",
	})

	WorkerIdleGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crawler_workers_idle",
		Help: "Number of idle worker goroutines",
	})

	// --- Rate Limiter Metrics ---

	RateLimitTokensAcquired = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crawler_rate_limit_tokens_acquired_total",
		Help: "Total tokens acquired from rate limiter",
	}, []string{"crawling_id"})

	RateLimitWaits = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crawler_rate_limit_waits_total",
		Help: "Number of times worker had to wait for tokens",
	}, []string{"crawling_id"})

	// --- HTTP Response Metrics ---

	HTTPStatusCodes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crawler_http_status_codes_total",
		Help: "HTTP response status codes",
	}, []string{"crawling_id", "code"})

	// --- Job Metrics ---

	ActiveCrawlingsGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crawler_active_crawlings",
		Help: "Number of currently active crawling jobs",
	})

	CrawlProgressGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "crawler_progress_percent",
		Help: "Crawl progress percentage",
	}, []string{"crawling_id"})
)

// Handler returns the Prometheus HTTP handler for scraping.
func Handler() http.Handler {
	return promhttp.Handler()
}
