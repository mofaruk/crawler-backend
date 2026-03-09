package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/webkonsulenterne/crawler-backend/internal/config"
	"github.com/webkonsulenterne/crawler-backend/internal/crawler"
	"github.com/webkonsulenterne/crawler-backend/internal/metrics"
	"github.com/webkonsulenterne/crawler-backend/internal/queue"
	"github.com/webkonsulenterne/crawler-backend/internal/ratelimiter"
	"github.com/webkonsulenterne/crawler-backend/internal/repository"
	"github.com/webkonsulenterne/crawler-backend/internal/worker"
)

func main() {
	// Load config
	cfg := config.Load()
	cfg.ServiceRole = "worker"

	// Setup logging
	setupLogging(cfg.LogLevel)

	log.Info().
		Str("service", cfg.ServiceName).
		Str("role", cfg.ServiceRole).
		Int("concurrency", cfg.WorkerConcurrency).
		Msg("starting worker service")

	// Connect to MongoDB
	repo, err := repository.NewMongoRepository(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to MongoDB")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		repo.Close(ctx)
	}()
	log.Info().Msg("connected to MongoDB")

	// Connect to Redis
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
		PoolSize: cfg.WorkerConcurrency + 50, // ensure enough connections for all workers
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatal().Err(err).Msg("failed to connect to Redis")
	}
	defer rdb.Close()
	log.Info().Msg("connected to Redis")

	// Initialize components
	q := queue.NewDistributedQueue(rdb)
	sm := queue.NewJobStateManager(rdb)
	rl := ratelimiter.NewDistributedRateLimiter(rdb)
	fetcher := crawler.NewHTTPFetcher(cfg)

	// Create worker pool
	pool := worker.NewPool(cfg, q, sm, rl, fetcher, repo)

	// --- Metrics Server ---
	metricsSrv := &http.Server{
		Addr:    ":" + cfg.MetricsPort,
		Handler: metrics.Handler(),
	}
	go func() {
		log.Info().Str("port", cfg.MetricsPort).Msg("starting metrics server")
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("metrics server error")
		}
	}()

	// Start worker pool
	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)

	// --- Graceful Shutdown ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Info().Str("signal", sig.String()).Msg("shutting down worker")

	cancel()
	pool.Stop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("metrics server shutdown error")
	}

	log.Info().Msg("worker service stopped")
}

func setupLogging(level string) {
	zerolog.TimeFieldFormat = time.RFC3339Nano

	switch level {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	log.Logger = zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", "crawler-worker").
		Logger()
}
