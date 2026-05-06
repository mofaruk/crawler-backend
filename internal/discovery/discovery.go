// Package discovery walks a website starting from a seed URL, emitting every
// in-scope URL it encounters: pages, CSS, JS, images, fonts, media, etc.
//
// Scope is strict same-host (case-insensitive Host match against the seed).
// Only HTML responses are parsed for further links; static assets are emitted
// once and not recursed into. Each URL is emitted at most once per Discoverer.
package discovery

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html"
)

// Default knobs. Tuned for politeness on a single target host.
const (
	defaultConcurrency = 5
	defaultFetchTimeout = 30 * time.Second
	defaultMaxBodyBytes = 8 * 1024 * 1024 // 8 MB cap for HTML parsing
	taskQueueBuffer     = 1024
)

// EmitFn receives every newly-discovered URL (already deduplicated and in-scope).
// Return false to abort discovery (e.g., reached an external limit).
type EmitFn func(rawURL string) bool

type Discoverer struct {
	client      *http.Client
	userAgent   string
	concurrency int
	maxBody     int64
}

func New(userAgent string) *Discoverer {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  false,
	}
	client := &http.Client{
		Timeout:   defaultFetchTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	return &Discoverer{
		client:      client,
		userAgent:   userAgent,
		concurrency: defaultConcurrency,
		maxBody:     defaultMaxBodyBytes,
	}
}

// Discover walks the site starting from baseURL, restricted to the same host.
// emit is called for every unique in-scope URL until limit is reached or the
// crawl frontier is exhausted. limit <= 0 means no cap.
//
// Static assets (CSS/JS/images/etc.) are emitted but not fetched; only HTML
// responses contribute new URLs to the frontier.
func (d *Discoverer) Discover(ctx context.Context, baseURL string, limit int, emit EmitFn) error {
	base, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return fmt.Errorf("base URL must be http or https, got %q", base.Scheme)
	}
	if base.Host == "" {
		return fmt.Errorf("base URL must include a host")
	}
	base.Fragment = ""
	seedHost := strings.ToLower(base.Host)

	state := &discoverState{
		ctx:       ctx,
		seedHost:  seedHost,
		seen:      make(map[string]struct{}, 1024),
		tasks:     make(chan *url.URL, taskQueueBuffer),
		limit:     limit,
		emit:      emit,
		fetcher:   d,
	}

	// Launch HTML-fetch workers. Each waits on tasks; new tasks come from
	// addURL via state.dispatchHTML.
	for i := 0; i < d.concurrency; i++ {
		state.workers.Add(1)
		go state.workerLoop()
	}

	// Seed: emit and recurse.
	state.addURL(base, true)

	// Drain coordinator: when no tasks are in flight and the channel is empty,
	// close it so workers exit. We use the wg pattern: each enqueued task
	// increments wg; workerLoop decrements after handling. When wg hits zero
	// no more tasks can appear (only fetched HTML produces new ones, and all
	// fetches have completed).
	go func() {
		state.inflight.Wait()
		close(state.tasks)
	}()

	state.workers.Wait()
	return nil
}

type discoverState struct {
	ctx      context.Context
	seedHost string

	seenMu sync.Mutex
	seen   map[string]struct{}

	tasks    chan *url.URL
	inflight sync.WaitGroup // counts HTML pages queued but not yet fetched
	workers  sync.WaitGroup

	emittedCount atomic.Int64
	limit        int
	stopped      atomic.Bool

	emit    EmitFn
	fetcher *Discoverer
}

func (s *discoverState) workerLoop() {
	defer s.workers.Done()
	for u := range s.tasks {
		if !s.stopped.Load() && s.ctx.Err() == nil {
			s.fetcher.fetchAndExtract(s.ctx, u, func(found *url.URL) {
				s.addURL(found, true)
			})
		}
		s.inflight.Done()
	}
}

// addURL deduplicates, scope-checks, and emits a URL. If recurse is true and
// the URL appears to point at an HTML resource, it is queued for fetching.
func (s *discoverState) addURL(u *url.URL, recurse bool) {
	if s.stopped.Load() || s.ctx.Err() != nil {
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return
	}
	if !strings.EqualFold(u.Host, s.seedHost) {
		return
	}
	u.Fragment = ""
	key := u.String()

	s.seenMu.Lock()
	if _, dup := s.seen[key]; dup {
		s.seenMu.Unlock()
		return
	}
	s.seen[key] = struct{}{}
	s.seenMu.Unlock()

	n := s.emittedCount.Add(1)
	if s.limit > 0 && n > int64(s.limit) {
		s.stopped.Store(true)
		return
	}
	if !s.emit(key) {
		s.stopped.Store(true)
		return
	}

	if recurse && looksLikeHTML(u) {
		s.dispatchHTML(u)
	}
}

// dispatchHTML enqueues a URL for HTML parsing. Falls back to a goroutine if
// the bounded channel is full (avoids producer/consumer deadlock when workers
// themselves are the producers).
func (s *discoverState) dispatchHTML(u *url.URL) {
	s.inflight.Add(1)
	select {
	case s.tasks <- u:
	default:
		// Channel full and we may be a worker ourselves; spawn a launcher.
		go func() {
			select {
			case s.tasks <- u:
			case <-s.ctx.Done():
				s.inflight.Done()
			}
		}()
	}
}

// looksLikeHTML uses a fast extension check to decide whether a URL is worth
// fetching for further link extraction. The fetcher itself also gates on
// Content-Type, so false positives just cost one HEAD-equivalent GET.
func looksLikeHTML(u *url.URL) bool {
	path := strings.ToLower(u.Path)
	// Trim trailing slash so "/page/" becomes "/page".
	path = strings.TrimRight(path, "/")
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return true // no extension — likely a directory or pretty-URL page
	}
	// If there's a slash after the dot, the dot was in a directory name.
	if strings.Contains(path[idx:], "/") {
		return true
	}
	ext := path[idx:]
	if _, isAsset := nonHTMLExts[ext]; isAsset {
		return false
	}
	// Common page extensions (or unknown) → treat as HTML.
	return true
}

// File extensions we are confident never serve HTML and never need recursion.
var nonHTMLExts = map[string]struct{}{
	".css": {}, ".js": {}, ".mjs": {}, ".map": {},
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".webp": {}, ".bmp": {}, ".svg": {}, ".ico": {}, ".avif": {}, ".heic": {},
	".woff": {}, ".woff2": {}, ".ttf": {}, ".otf": {}, ".eot": {},
	".mp3": {}, ".mp4": {}, ".webm": {}, ".ogg": {}, ".wav": {}, ".m4a": {}, ".m4v": {}, ".mov": {},
	".pdf": {}, ".zip": {}, ".gz": {}, ".tar": {}, ".rar": {}, ".7z": {},
	".doc": {}, ".docx": {}, ".xls": {}, ".xlsx": {}, ".ppt": {}, ".pptx": {},
	".json": {}, ".xml": {}, ".rss": {}, ".atom": {}, ".txt": {}, ".csv": {},
}

// fetchAndExtract fetches u, and if the response is HTML, parses it and calls
// onURL for each link/asset reference (already resolved against the page URL).
func (d *Discoverer) fetchAndExtract(ctx context.Context, u *url.URL, onURL func(*url.URL)) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return
	}
	if d.userAgent != "" {
		req.Header.Set("User-Agent", d.userAgent)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := d.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	if !isHTMLContentType(resp.Header.Get("Content-Type")) {
		return
	}

	// The page URL we resolve against may differ from u after redirects.
	pageURL := u
	if resp.Request != nil && resp.Request.URL != nil {
		pageURL = resp.Request.URL
	}

	body := io.LimitReader(resp.Body, d.maxBody)
	extractLinks(body, pageURL, onURL)
}

func isHTMLContentType(ct string) bool {
	ct = strings.ToLower(ct)
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	return ct == "text/html" || ct == "application/xhtml+xml"
}

// extractLinks streams through an HTML document, resolving every URL-bearing
// attribute against pageURL and forwarding results to onURL.
func extractLinks(r io.Reader, pageURL *url.URL, onURL func(*url.URL)) {
	z := html.NewTokenizer(r)
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}

		nameBytes, hasAttr := z.TagName()
		if !hasAttr {
			continue
		}
		tag := string(nameBytes)

		// Walk attributes, capturing the ones we care about for this tag.
		var hrefVal, srcVal, srcsetVal, dataVal, posterVal string
		for {
			k, v, more := z.TagAttr()
			switch strings.ToLower(string(k)) {
			case "href":
				hrefVal = string(v)
			case "src":
				srcVal = string(v)
			case "srcset", "imagesrcset":
				srcsetVal = string(v)
			case "data":
				dataVal = string(v)
			case "poster":
				posterVal = string(v)
			}
			if !more {
				break
			}
		}

		switch tag {
		case "a", "area", "link":
			emitCandidate(pageURL, hrefVal, onURL)
		case "script", "iframe", "embed", "audio", "video", "img", "track", "frame":
			emitCandidate(pageURL, srcVal, onURL)
			if posterVal != "" {
				emitCandidate(pageURL, posterVal, onURL)
			}
			if srcsetVal != "" {
				for _, c := range parseSrcset(srcsetVal) {
					emitCandidate(pageURL, c, onURL)
				}
			}
		case "source":
			emitCandidate(pageURL, srcVal, onURL)
			if srcsetVal != "" {
				for _, c := range parseSrcset(srcsetVal) {
					emitCandidate(pageURL, c, onURL)
				}
			}
		case "object":
			emitCandidate(pageURL, dataVal, onURL)
		}
	}
}

func emitCandidate(pageURL *url.URL, raw string, onURL func(*url.URL)) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return
	}
	// Skip non-http(s) schemes early: data:, javascript:, mailto:, tel:, etc.
	if i := strings.IndexByte(raw, ':'); i > 0 {
		// Heuristic: a colon before any "/" is a scheme.
		slash := strings.IndexByte(raw, '/')
		if slash < 0 || i < slash {
			scheme := strings.ToLower(raw[:i])
			if scheme != "http" && scheme != "https" {
				return
			}
		}
	}

	ref, err := url.Parse(raw)
	if err != nil {
		return
	}
	resolved := pageURL.ResolveReference(ref)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return
	}
	onURL(resolved)
}

// parseSrcset extracts the URL portion of each comma-separated entry in a
// srcset attribute. Each entry is "url descriptor"; descriptor is optional.
func parseSrcset(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Take everything up to the first whitespace.
		if i := strings.IndexAny(part, " \t"); i > 0 {
			part = part[:i]
		}
		out = append(out, part)
	}
	return out
}
