// Package linkcheck verifies that a site's outbound links still resolve.
//
// Deliberately separate from the cache crawl. These requests go to third
// parties rather than the customer's own origin, so they get their own
// concurrency budget, their own timeouts and their own schedule — a slow or
// hostile external host must never delay the cache report, which is the
// product's actual subject.
package linkcheck

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Result is the outcome of checking one destination.
type Result struct {
	URL          string
	StatusCode   int
	Error        string
	ResponseTime time.Duration
}

// Checker verifies destinations over HTTP.
type Checker struct {
	client    *http.Client
	userAgent string
}

// New builds a Checker.
//
// Redirects are followed: a link that redirects still gets the visitor
// somewhere, so only the final answer decides whether it is broken.
func New(userAgent string, timeout time.Duration) *Checker {
	return &Checker{
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
		userAgent: userAgent,
	}
}

// Check verifies one destination.
//
// HEAD first, because the body is irrelevant and not transferring it is the
// polite thing to do against someone else's server. Many hosts answer HEAD
// with 405 or 403 despite serving GET fine, so those fall back rather than
// being reported as broken — a false "broken link" is worse than a slightly
// more expensive check.
func (c *Checker) Check(ctx context.Context, rawURL string) Result {
	started := time.Now()

	status, err := c.request(ctx, http.MethodHead, rawURL)

	if err == nil && headRejected(status) {
		status, err = c.request(ctx, http.MethodGet, rawURL)
	}

	res := Result{URL: rawURL, StatusCode: status, ResponseTime: time.Since(started)}
	if err != nil {
		res.Error = describeError(err)
	}

	return res
}

func (c *Checker) request(ctx context.Context, method, rawURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return 0, err
	}

	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	return resp.StatusCode, nil
}

// BotBlocked reports whether a status is more likely to mean "we block
// automated requests" than "this page is gone".
//
// Social platforms and sites behind bot protection routinely answer any
// non-browser request with 400, 403 or 429 while serving humans perfectly
// well. Reporting those as broken links would fill a customer's report with
// false alarms about their own Facebook page, so they are flagged separately
// rather than counted as failures.
func BotBlocked(statusCode int) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusUnavailableForLegalReasons:
		return true
	}

	return false
}

// headRejected reports whether a status means "this server dislikes HEAD"
// rather than "this page is gone".
func headRejected(status int) bool {
	switch status {
	case http.StatusMethodNotAllowed, http.StatusNotImplemented,
		http.StatusForbidden, http.StatusBadRequest:
		return true
	}

	return false
}

// describeError turns a transport error into something a site owner can act
// on. The raw Go error names the dialer and the address, which reads as noise
// in a report.
func describeError(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "domain does not resolve"
	}

	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timeout") {
		return "timed out"
	}

	msg := err.Error()

	switch {
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"), strings.Contains(msg, "tls:"):
		return "certificate problem"
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "too many redirects"):
		return "redirect loop"
	}

	return "unreachable"
}

// CheckAll verifies destinations with bounded concurrency, returning results
// in the order the inputs were given.
//
// The cap is per call and modest on purpose: these are other people's servers,
// and a site with thousands of outbound links must not arrive at any of them
// as a burst.
func (c *Checker) CheckAll(ctx context.Context, urls []string, concurrency int) []Result {
	if concurrency < 1 {
		concurrency = 1
	}

	results := make([]Result, len(urls))
	sem := make(chan struct{}, concurrency)

	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)

		go func(i int, u string) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = Result{URL: u, Error: "cancelled"}
				return
			}

			results[i] = c.Check(ctx, u)
		}(i, u)
	}

	wg.Wait()

	return results
}
