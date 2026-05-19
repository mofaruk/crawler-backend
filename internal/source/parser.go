package source

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// URLParser fetches and parses URL sources (CSV or XML sitemaps).

type URLParser struct {
	client *http.Client
}

func NewURLParser() *URLParser {
	return &URLParser{
		client: &http.Client{
			Timeout: 5 * time.Minute, // large files may take time
		},
	}
}

// maxSitemapDepth caps how deep nested <sitemapindex> documents are followed.
// The protocol only allows one level of indexing; the extra headroom guards
// against non-conforming sites without risking unbounded recursion.
const maxSitemapDepth = 5

// get performs the HTTP GET with the site's User-Agent and returns a
// status-checked response. The caller must close resp.Body.
func (p *URLParser) get(ctx context.Context, target, userAgent string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching source: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("source returned status %d", resp.StatusCode)
	}
	return resp, nil
}

// ParseURLs fetches the URL source and returns the page URLs. userAgent is the
// site's configured User-Agent; sent so hosts that key on it behave
// consistently with the actual crawl. For xml sources, a <sitemapindex> is
// followed recursively into its child sitemaps.
func (p *URLParser) ParseURLs(ctx context.Context, sourceURL, sourceType, userAgent string, limit int) ([]string, error) {
	switch strings.ToLower(sourceType) {
	case "csv":
		resp, err := p.get(ctx, sourceURL, userAgent)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return p.parseCSV(resp.Body, limit)
	case "xml":
		return p.parseSitemap(ctx, sourceURL, userAgent, limit, 0, map[string]bool{})
	default:
		return nil, fmt.Errorf("unsupported source type: %s", sourceType)
	}
}

func (p *URLParser) parseCSV(reader io.Reader, limit int) ([]string, error) {
	r := csv.NewReader(bufio.NewReaderSize(reader, 64*1024))
	r.LazyQuotes = true
	r.TrimLeadingSpace = true

	var urls []string
	lineNum := 0

	for {
		if limit > 0 && len(urls) >= limit {
			break
		}

		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Warn().Err(err).Int("line", lineNum).Msg("skipping malformed CSV line")
			lineNum++
			continue
		}
		lineNum++

		if len(record) == 0 {
			continue
		}

		// First column contains the URL
		rawURL := strings.TrimSpace(record[0])
		if rawURL == "" || rawURL == "url" || rawURL == "URL" {
			continue // skip header
		}

		if isValidURL(rawURL) {
			urls = append(urls, rawURL)
		}
	}

	return urls, nil
}

// sitemapLoc captures the <loc> of both <url> (urlset) and <sitemap>
// (sitemapindex) entries — both wrap a single <loc>.
type sitemapLoc struct {
	Loc string `xml:"loc"`
}

// parseSitemap fetches one sitemap document and returns the page URLs it
// yields. A <urlset> contributes its <url><loc> entries directly; a
// <sitemapindex> has each referenced child sitemap fetched recursively. Safe
// against cycles (seen set), runaway nesting (maxSitemapDepth) and gzip-
// compressed sitemaps (.xml.gz, detected by magic bytes). A failing *child*
// is skipped; a failing *root* (depth 0) is fatal.
func (p *URLParser) parseSitemap(ctx context.Context, target, userAgent string, limit, depth int, seen map[string]bool) ([]string, error) {
	if depth > maxSitemapDepth || seen[target] {
		return nil, nil
	}
	seen[target] = true

	resp, err := p.get(ctx, target, userAgent)
	if err != nil {
		if depth == 0 {
			return nil, err
		}
		log.Warn().Err(err).Str("sitemap", target).Msg("skipping unreachable child sitemap")
		return nil, nil
	}
	defer resp.Body.Close()

	// Transparently decompress gzip sitemaps, detected by magic bytes rather
	// than relying on Content-Type / .gz extension (both are unreliable).
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	var reader io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, gzErr := gzip.NewReader(br)
		if gzErr != nil {
			if depth == 0 {
				return nil, fmt.Errorf("decompressing sitemap: %w", gzErr)
			}
			log.Warn().Err(gzErr).Str("sitemap", target).Msg("skipping unreadable gzip child sitemap")
			return nil, nil
		}
		defer gz.Close()
		reader = gz
	}

	var urls, children []string
	decoder := xml.NewDecoder(reader)
	for {
		if limit > 0 && len(urls) >= limit {
			break
		}
		token, err := decoder.Token()
		if err != nil { // io.EOF or malformed tail — stop with what we have
			break
		}
		se, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "url": // <urlset> entry → a page URL
			var u sitemapLoc
			if decoder.DecodeElement(&u, &se) == nil {
				if loc := strings.TrimSpace(u.Loc); loc != "" && isValidURL(loc) {
					urls = append(urls, loc)
				}
			}
		case "sitemap": // <sitemapindex> entry → a child sitemap
			var s sitemapLoc
			if decoder.DecodeElement(&s, &se) == nil {
				if loc := strings.TrimSpace(s.Loc); loc != "" && isValidURL(loc) {
					children = append(children, loc)
				}
			}
		}
	}

	// Follow child sitemaps until the limit is satisfied.
	for _, child := range children {
		if limit > 0 && len(urls) >= limit {
			break
		}
		remaining := limit
		if limit > 0 {
			remaining = limit - len(urls)
		}
		childURLs, _ := p.parseSitemap(ctx, child, userAgent, remaining, depth+1, seen)
		urls = append(urls, childURLs...)
	}

	if limit > 0 && len(urls) > limit {
		urls = urls[:limit]
	}
	return urls, nil
}

func isValidURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
