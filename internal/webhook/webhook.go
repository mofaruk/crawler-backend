package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/webkonsulenterne/crawler-backend/internal/config"
)

// Webhook dispatcher sends crawl progress events to configured endpoints.

type EventType string

const (
	EventCrawlStarted   EventType = "crawl.started"
	EventCrawlPaused    EventType = "crawl.paused"
	EventCrawlResumed   EventType = "crawl.resumed"
	EventCrawlStopped   EventType = "crawl.stopped"
	EventCrawlCompleted EventType = "crawl.completed"
	EventCrawlFailed    EventType = "crawl.failed"
	EventCrawlProgress  EventType = "crawl.progress"
)

type WebhookPayload struct {
	Event     EventType              `json:"event"`
	Timestamp time.Time              `json:"timestamp"`
	Data      map[string]interface{} `json:"data"`
}

type Dispatcher struct {
	client     *http.Client
	maxRetries int
}

func NewDispatcher(cfg *config.Config) *Dispatcher {
	return &Dispatcher{
		client: &http.Client{
			Timeout: cfg.WebhookTimeout,
		},
		maxRetries: cfg.WebhookMaxRetries,
	}
}

// Send dispatches a webhook event. Retries on failure with exponential backoff.
func (d *Dispatcher) Send(ctx context.Context, webhookURL, secret string, payload WebhookPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling webhook payload: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= d.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("creating webhook request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Webhook-Event", string(payload.Event))
		req.Header.Set("X-Webhook-Timestamp", payload.Timestamp.Format(time.RFC3339))

		// HMAC signature for verification
		if secret != "" {
			sig := computeHMAC(body, secret)
			req.Header.Set("X-Webhook-Signature", sig)
		}

		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			log.Warn().Err(err).Int("attempt", attempt+1).Str("url", webhookURL).Msg("webhook delivery failed")
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		lastErr = fmt.Errorf("webhook returned status %d", resp.StatusCode)
		log.Warn().Int("status", resp.StatusCode).Int("attempt", attempt+1).Msg("webhook non-2xx response")
	}

	return fmt.Errorf("webhook delivery failed after %d attempts: %w", d.maxRetries+1, lastErr)
}

func computeHMAC(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
