package source

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// candidatePaths are the sitemap locations worth trying, most likely first.
//
// The ordering matters: WordPress is the common case for this product's
// customers, and since 5.5 core serves /wp-sitemap.xml. Yoast and RankMath
// (the two dominant SEO plugins) both serve /sitemap_index.xml, which usually
// also exists as /sitemap.xml via a redirect — trying the index first avoids
// following that hop.
var candidatePaths = []string{
	"/sitemap_index.xml", // Yoast, RankMath
	"/wp-sitemap.xml",    // WordPress core 5.5+
	"/sitemap.xml",       // the generic default
	"/sitemap-index.xml",
	"/sitemap/sitemap.xml",
	"/sitemap1.xml",
	"/post-sitemap.xml", // Yoast without an index
	"/page-sitemap.xml",
}

// SitemapCandidate is one location that was tried.
type SitemapCandidate struct {
	URL      string `json:"url"`
	Found    bool   `json:"found"`
	Status   int    `json:"status,omitempty"`
	URLCount int    `json:"url_count,omitempty"`
	Kind     string `json:"kind,omitempty"` // "urlset" | "sitemapindex"
	Source   string `json:"source"`         // "robots.txt" | "common path"
}

// FindSitemaps locates a site's sitemap without the user having to know where
// it lives.
//
// robots.txt is checked first because it is authoritative — a site that
// declares its sitemap there is telling the truth about a non-standard
// location. Only if that yields nothing do we probe the common paths.
//
// Every candidate is fetched and parsed rather than merely HEAD-checked: many
// hosts answer 200 with an HTML error page for a missing .xml, and a
// soft-404 would otherwise be reported as a working sitemap.
func (p *URLParser) FindSitemaps(ctx context.Context, baseURL, userAgent string) ([]SitemapCandidate, error) {
	if err := ValidateTargetURL(baseURL, true); err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	root := u.Scheme + "://" + u.Host

	var out []SitemapCandidate
	seen := map[string]bool{}

	// 1. Whatever robots.txt declares.
	for _, declared := range p.sitemapsFromRobots(ctx, root, userAgent) {
		if seen[declared] {
			continue
		}
		seen[declared] = true
		out = append(out, p.probeSitemap(ctx, declared, userAgent, "robots.txt"))
	}

	// A working sitemap from robots.txt is authoritative; stop there rather
	// than listing every other path that happens to exist.
	for _, c := range out {
		if c.Found && c.URLCount > 0 {
			return out, nil
		}
	}

	// 2. Common locations.
	for _, path := range candidatePaths {
		candidate := root + path
		if seen[candidate] {
			continue
		}
		seen[candidate] = true

		c := p.probeSitemap(ctx, candidate, userAgent, "common path")
		out = append(out, c)

		if c.Found && c.URLCount > 0 {
			break
		}
	}

	return out, nil
}

// sitemapsFromRobots reads the Sitemap: directives out of robots.txt.
func (p *URLParser) sitemapsFromRobots(ctx context.Context, root, userAgent string) []string {
	resp, _, err := p.get(ctx, root+"/robots.txt", userAgent)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var found []string
	scanner := bufio.NewScanner(resp.Body)
	// robots.txt is small; cap the read so a hostile file cannot exhaust memory.
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(strings.ToLower(line), "sitemap:") {
			continue
		}
		if v := strings.TrimSpace(line[len("sitemap:"):]); isValidURL(v) {
			found = append(found, v)
		}
	}

	return found
}

// probeSitemap fetches one candidate and reports what it actually contains.
func (p *URLParser) probeSitemap(ctx context.Context, target, userAgent, source string) SitemapCandidate {
	c := SitemapCandidate{URL: target, Source: source}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, status, err := p.get(ctx, target, userAgent)
	c.Status = status
	if err != nil {
		return c
	}
	defer resp.Body.Close()

	// Parse rather than trust the status: hosts routinely serve an HTML error
	// page with 200 for a missing .xml.
	stats := &ParseStats{SourceType: "xml"}
	urls, err := p.parseSitemapDocument(resp, 5, stats)
	if err != nil {
		return c
	}

	c.Kind = stats.XMLFormat
	c.URLCount = len(urls)
	if stats.XMLChildSitemaps > 0 {
		c.URLCount = stats.XMLChildSitemaps
		c.Kind = "sitemapindex"
	}
	c.Found = c.URLCount > 0 && (c.Kind == "urlset" || c.Kind == "sitemapindex")

	return c
}

// parseSitemapDocument reads one already-fetched sitemap without following
// its children — a probe only needs to know the document is real and has
// entries, not to walk the whole tree.
func (p *URLParser) parseSitemapDocument(resp *http.Response, limit int, stats *ParseStats) ([]string, error) {
	return p.parseSitemapReader(resp.Body, limit, stats)
}
