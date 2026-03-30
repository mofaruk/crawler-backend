package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/webkonsulenterne/crawler-backend/internal/api"
	"github.com/webkonsulenterne/crawler-backend/internal/config"
	"github.com/webkonsulenterne/crawler-backend/internal/dedup"
	"github.com/webkonsulenterne/crawler-backend/internal/metrics"
	"github.com/webkonsulenterne/crawler-backend/internal/middleware"
	"github.com/webkonsulenterne/crawler-backend/internal/queue"
	"github.com/webkonsulenterne/crawler-backend/internal/ratelimiter"
	"github.com/webkonsulenterne/crawler-backend/internal/repository"
)

func main() {
	// Load config
	cfg := config.Load()
	cfg.ServiceRole = "api"

	// Setup logging
	setupLogging(cfg.LogLevel)

	log.Info().
		Str("service", cfg.ServiceName).
		Str("role", cfg.ServiceRole).
		Str("port", cfg.APIPort).
		Msg("starting API service")

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
		PoolSize: 50,
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
	dd := dedup.NewDeduplicator(rdb)

	// Create handler
	handler := api.NewHandler(cfg, repo, q, sm, rl, dd)

	// Setup Gin router
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(middleware.Recovery())
	router.Use(middleware.RequestLogger())
	router.Use(middleware.CORS())

	// --- Routes ---
	router.GET("/health", handler.Health)

	// Sites
	router.POST("/sites", handler.CreateSite)
	router.GET("/sites", handler.ListSites)
	router.GET("/sites/:id", handler.GetSite)
	router.PUT("/sites/:id", handler.UpdateSite)
	router.DELETE("/sites/:id", handler.DeleteSite)

	// Crawlings
	router.POST("/crawlings/start", handler.StartCrawling)
	router.GET("/crawlings", handler.ListCrawlings)
	router.GET("/crawlings/:id", handler.GetCrawling)
	router.POST("/crawlings/:id/pause", handler.PauseCrawling)
	router.POST("/crawlings/:id/resume", handler.ResumeCrawling)
	router.POST("/crawlings/:id/stop", handler.StopCrawling)
	router.GET("/crawlings/:id/progress", handler.GetCrawlingProgress)
	router.GET("/crawlings/:id/failures", handler.GetCrawlingFailures)
	router.GET("/crawlings/:id/results", handler.GetCrawlingResults)
	router.GET("/crawlings/:id/results/analytics", handler.GetHeaderAnalytics)

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

	// --- API Server ---
	srv := &http.Server{
		Addr:         ":" + cfg.APIPort,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Info().Str("port", cfg.APIPort).Msg("starting API server")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("API server error")
		}
	}()

	// --- Graceful Shutdown ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Info().Str("signal", sig.String()).Msg("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("API server shutdown error")
	}
	if err := metricsSrv.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("metrics server shutdown error")
	}

	log.Info().Msg("API service stopped")
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
		Str("service", "crawler-api").
		Logger()
}
