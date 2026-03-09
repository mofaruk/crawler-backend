package crawler

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/webkonsulenterne/crawler-backend/internal/config"
	"github.com/webkonsulenterne/crawler-backend/internal/models"
)

// HTTPFetcher performs HTTP requests with connection pooling,
// per-domain concurrency limits, and politeness enforcement.

type HTTPFetcher struct {
	client        *http.Client
	domainLimiter *DomainLimiter
	cfg           *config.Config
}

func NewHTTPFetcher(cfg *config.Config) *HTTPFetcher {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   50,
		MaxConnsPerHost:       50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:  10 * time.Second,
		ResponseHeaderTimeout: cfg.CrawlerTimeout,
		TLSClientConfig:      &tls.Config{InsecureSkipVerify: false},
	}

	return &HTTPFetcher{
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.CrawlerTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects (5)")
				}
				return nil
			},
		},
		domainLimiter: NewDomainLimiter(10, 100*time.Millisecond), // max 10 concurrent per domain
		cfg:           cfg,
	}
}

// FetchResult contains the results of fetching a URL.
type FetchResult struct {
	URL          string
	StatusCode   int
	Headers      map[string]string
	ContentType  string
	ResponseTime time.Duration
	Error        error
}

// Fetch crawls a single URL and extracts the requested headers.
func (f *HTTPFetcher) Fetch(ctx context.Context, task *models.CrawlTask) *FetchResult {
	result := &FetchResult{URL: task.URL}
	start := time.Now()

	// Per-domain concurrency limit
	domain := extractDomain(task.URL)
	f.domainLimiter.Acquire(domain)
	defer f.domainLimiter.Release(domain)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, task.URL, nil)
	if err != nil {
		result.Error = err
		return result
	}

	// Set user agent
	ua := task.UserAgent
	if ua == "" {
		ua = f.cfg.DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := f.client.Do(req)
	if err != nil {
		result.Error = err
		result.ResponseTime = time.Since(start)
		return result
	}
	defer resp.Body.Close()

	// Drain body to allow connection reuse (but limit read)
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) // 1MB max

	result.StatusCode = resp.StatusCode
	result.ContentType = resp.Header.Get("Content-Type")
	result.ResponseTime = time.Since(start)

	// Extract requested headers
	result.Headers = make(map[string]string)
	for _, header := range task.ExtractData {
		header = strings.TrimSpace(header)
		if header == "" {
			continue
		}
		value := resp.Header.Get(header)
		if value != "" {
			result.Headers[header] = value
		}
	}

	return result
}

func extractDomain(rawURL string) string {
	// Fast domain extraction without full URL parsing
	idx := strings.Index(rawURL, "://")
	if idx == -1 {
		return rawURL
	}
	rest := rawURL[idx+3:]
	if slashIdx := strings.Index(rest, "/"); slashIdx != -1 {
		rest = rest[:slashIdx]
	}
	// Remove port
	if colonIdx := strings.LastIndex(rest, ":"); colonIdx != -1 {
		rest = rest[:colonIdx]
	}
	return rest
}

// --- Per-Domain Concurrency Limiter ---

type DomainLimiter struct {
	mu       sync.Mutex
	limiters map[string]chan struct{}
	maxConc  int
	delay    time.Duration
}

func NewDomainLimiter(maxConcurrency int, minDelay time.Duration) *DomainLimiter {
	return &DomainLimiter{
		limiters: make(map[string]chan struct{}),
		maxConc:  maxConcurrency,
		delay:    minDelay,
	}
}

func (dl *DomainLimiter) Acquire(domain string) {
	dl.mu.Lock()
	ch, ok := dl.limiters[domain]
	if !ok {
		ch = make(chan struct{}, dl.maxConc)
		dl.limiters[domain] = ch
	}
	dl.mu.Unlock()

	ch <- struct{}{} // blocks if at max concurrency

	if dl.delay > 0 {
		time.Sleep(dl.delay)
	}
}

func (dl *DomainLimiter) Release(domain string) {
	dl.mu.Lock()
	ch, ok := dl.limiters[domain]
	dl.mu.Unlock()

	if ok {
		<-ch
	}
}

// Close cleans up domain limiters (for graceful shutdown).
func (f *HTTPFetcher) Close() {
	f.client.CloseIdleConnections()
	log.Info().Msg("HTTP fetcher closed")
}
