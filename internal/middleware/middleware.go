package middleware

import (
	"crypto/subtle"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// RequestLogger logs each HTTP request with structured fields.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		method := c.Request.Method

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		logger := log.With().
			Str("method", method).
			Str("path", path).
			Int("status", status).
			Dur("latency", latency).
			Str("ip", c.ClientIP()).
			Logger()

		switch {
		case status >= 500:
			logger.Error().Msg("server error")
		case status >= 400:
			logger.Warn().Msg("client error")
		default:
			logger.Info().Msg("request")
		}
	}
}

// Recovery recovers from panics and returns 500.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				log.Error().Interface("error", err).Str("path", c.Request.URL.Path).Msg("panic recovered")
				c.AbortWithStatusJSON(500, gin.H{"error": "internal server error"})
			}
		}()
		c.Next()
	}
}

// CORS restricts cross-origin access to an explicit allowlist.
//
// The previous "*" let any web page in a user's browser drive this API. The
// dashboard talks to it server-to-server from PHP, so the correct default is
// an empty allowlist — no browser origin at all. Set ALLOWED_ORIGINS only if
// something genuinely needs browser access.
func CORS(allowedOrigins []string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin != "" {
			if _, ok := allowed[origin]; ok {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
				c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
			}
		}

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// APIKeyAuth rejects any request without the shared secret.
//
// Every route except /health is gated. The key is compared with
// subtle.ConstantTimeCompare so a timing side-channel cannot reveal it.
//
// An empty configured key disables the check entirely: that is a deliberate
// local-development affordance, and cmd/api logs a startup warning so it
// cannot be enabled in production by accident.
func APIKeyAuth(expected string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if expected == "" {
			c.Next()
			return
		}

		provided := c.Request.Header.Get("X-API-Key")
		if provided == "" {
			// Also accept "Authorization: Bearer <key>".
			if h := c.Request.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				provided = strings.TrimPrefix(h, "Bearer ")
			}
		}

		if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			log.Warn().
				Str("path", c.Request.URL.Path).
				Str("ip", c.ClientIP()).
				Msg("rejected request with missing or invalid API key")
			c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized", "code": "UNAUTHORIZED"})
			return
		}

		c.Next()
	}
}
