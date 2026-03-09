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

	// Logging
	LogLevel string
}

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
		DefaultUserAgent:  envStr("DEFAULT_USER_AGENT", "WK-Crawler/1.0"),

		RateLimitWindow: envDuration("RATE_LIMIT_WINDOW", 1*time.Second),

		MetricsPort: envStr("METRICS_PORT", "9090"),

		WebhookTimeout:    envDuration("WEBHOOK_TIMEOUT", 10*time.Second),
		WebhookMaxRetries: envInt("WEBHOOK_MAX_RETRIES", 3),

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
