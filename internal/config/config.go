package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Service identity
	ServiceName string
	ServiceRole string // "api" or "worker"

	// HTTP server
	APIPort string

	// MongoDB
	MongoURI    string
	MongoDB     string
	MongoPoolSz int

	// Redis
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// Worker settings
	WorkerConcurrency int
	WorkerPollInterval time.Duration
	WorkerBatchSize   int

	// Crawler settings
	CrawlerTimeout    time.Duration
	CrawlerMaxRetries int
	DefaultUserAgent  string

	// Rate limiter
	RateLimitWindow time.Duration

	// Metrics
	MetricsPort string

	// Webhook
	WebhookTimeout   time.Duration
	WebhookMaxRetries int

	// Security
	//
	// APIKey gates every route except /health. Empty disables the check —
	// intended only for local development; the service logs a loud warning
	// at startup when it is unset.
	APIKey string
	// AllowedOrigins is a comma-separated CORS allowlist. Empty means no
	// cross-origin browser access, which is correct for a server-to-server
	// API; the dashboard calls it from PHP, not from the browser.
	AllowedOrigins []string
	// AllowPrivateTargets disables the SSRF guard. Local development only.
	AllowPrivateTargets bool

	// LinkCheckConcurrency bounds how many outbound links are verified at
	// once. These are third parties' servers, not the customer's.
	LinkCheckConcurrency int

	// Logging
	LogLevel string
}

// defaultUserAgent impersonates a current Chrome build. CDNs and WAFs often
// serve different cache behaviour — or block outright — for unrecognised bot
// agents, which would make the extracted cache headers misrepresent what a
// real visitor receives. Override per-site, or via DEFAULT_USER_AGENT.
// envBool reads a boolean the way deploy files actually write them. An exact
// "true" match meant TRUE, True, 1 and yes all silently left the SSRF guard on
// — safe, but confusing to debug when the bypass is genuinely wanted.
//
// Anything unrecognised keeps the default, so a typo never turns protection
// off by accident.
func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return def
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return def
	}
}

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"

func Load() *Config {
	return &Config{
		ServiceName: envStr("SERVICE_NAME", "crawler-backend"),
		ServiceRole: envStr("SERVICE_ROLE", "api"),

		APIPort: envStr("API_PORT", "8080"),

		MongoURI:    envStr("MONGO_URI", "mongodb://mongodb:27017"),
		MongoDB:     envStr("MONGO_DB", "crawler"),
		MongoPoolSz: envInt("MONGO_POOL_SIZE", 100),

		RedisAddr:     envStr("REDIS_ADDR", "redis:6379"),
		RedisPassword: envStr("REDIS_PASSWORD", ""),
		RedisDB:       envInt("REDIS_DB", 0),

		WorkerConcurrency:  envInt("WORKER_CONCURRENCY", 200),
		WorkerPollInterval: envDuration("WORKER_POLL_INTERVAL", 100*time.Millisecond),
		WorkerBatchSize:    envInt("WORKER_BATCH_SIZE", 50),

		CrawlerTimeout:    envDuration("CRAWLER_TIMEOUT", 30*time.Second),
		CrawlerMaxRetries: envInt("CRAWLER_MAX_RETRIES", 3),
		DefaultUserAgent:  envStr("DEFAULT_USER_AGENT", defaultUserAgent),

		RateLimitWindow: envDuration("RATE_LIMIT_WINDOW", 1*time.Second),

		MetricsPort: envStr("METRICS_PORT", "9090"),

		WebhookTimeout:    envDuration("WEBHOOK_TIMEOUT", 10*time.Second),
		WebhookMaxRetries: envInt("WEBHOOK_MAX_RETRIES", 3),

		// Modest on purpose: these requests go to third parties, so arriving
		// as a burst is both rude and a good way to get blocked.
		LinkCheckConcurrency: envInt("LINK_CHECK_CONCURRENCY", 5),

		APIKey:              envStr("API_KEY", ""),
		AllowedOrigins:      splitAndTrim(envStr("ALLOWED_ORIGINS", "")),
		AllowPrivateTargets: envBool("ALLOW_PRIVATE_TARGETS", false),

		LogLevel: strings.ToLower(envStr("LOG_LEVEL", "info")),
	}
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// splitAndTrim parses a comma-separated env var into a slice, dropping blanks.
func splitAndTrim(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
