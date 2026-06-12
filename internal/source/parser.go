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

// ParseStats records per-stage counters from a single ParseURLs invocation.
// It is always populated (even on success) so the caller can decide whether
// to surface it. When the URL list comes back empty, Diagnosis() turns these
// counters into a human-readable reason suitable for an end-user error log.
type ParseStats struct {
	SourceURL     string `json:"source_url"`
	SourceType    string `json:"source_type"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	ContentBytes  int64  `json:"content_bytes"` // bytes read from the root document

	// CSV-specific counters.
	CSVRowsScanned int `json:"csv_rows_scanned,omitempty"`
	CSVMalformed   int `json:"csv_malformed,omitempty"`
	CSVHeaderRows  int `json:"csv_header_rows,omitempty"`
	CSVEmptyRows   int `json:"csv_empty_rows,omitempty"`

	// XML-specific counters.
	XMLFormat          string `json:"xml_format,omitempty"`        // "urlset", "sitemapindex", "mixed", "unknown"
	XMLDocumentsRead   int    `json:"xml_documents_read,omitempty"` // root + child sitemaps successfully fetched
	XMLDocumentsFailed int    `json:"xml_documents_failed,omitempty"`
	XMLChildSitemaps   int    `json:"xml_child_sitemaps,omitempty"` // <sitemap><loc> entries observed
	XMLDepthReached    int    `json:"xml_depth_reached,omitempty"`
	XMLGzipDecoded     bool   `json:"xml_gzip_decoded,omitempty"`
	XMLLocEntries      int    `json:"xml_loc_entries,omitempty"` // total <url><loc> observed across all docs

	// Common — apply to both CSV first-column URLs and XML <loc>s.
	URLsAccepted        int `json:"urls_accepted"`
	URLsRejectedEmpty   int `json:"urls_rejected_empty,omitempty"`
	URLsRejectedInvalid int `json:"urls_rejected_invalid,omitempty"` // failed isValidURL (non-http/https or unparseable)

	LimitReached bool `json:"limit_reached,omitempty"`
}

// Diagnosis returns a short, end-user-facing reason for an empty result. It is
// only meaningful when len(urls)==0; callers should append it to the dashboard
// error message so the user can see *why* the source produced no URLs.
func (s *ParseStats) Diagnosis() string {
	if s == nil {
		return "no parse statistics available"
	}

	if s.HTTPStatus != 0 && s.HTTPStatus != http.StatusOK {
		return fmt.Sprintf("source returned HTTP %d", s.HTTPStatus)
	}
	if s.ContentBytes == 0 {
		return "source returned 0 bytes"
	}

	switch strings.ToLower(s.SourceType) {
	case "csv":
		switch {
		case s.CSVRowsScanned == 0:
			return fmt.Sprintf("CSV had %d bytes but produced 0 parsable rows", s.ContentBytes)
		case s.URLsRejectedInvalid > 0 && s.URLsAccepted == 0:
			return fmt.Sprintf(
				"CSV: scanned %d rows (%d malformed, %d empty, %d header), %d rows had values in column 1 but none were valid http(s) URLs",
				s.CSVRowsScanned, s.CSVMalformed, s.CSVEmptyRows, s.CSVHeaderRows, s.URLsRejectedInvalid,
			)
		case s.CSVMalformed > 0 && s.CSVMalformed == s.CSVRowsScanned:
			return fmt.Sprintf("CSV: all %d rows malformed (parser errors)", s.CSVRowsScanned)
		default:
			return fmt.Sprintf(
				"CSV: scanned %d rows (%d malformed, %d empty, %d header) and column 1 yielded no usable URLs",
				s.CSVRowsScanned, s.CSVMalformed, s.CSVEmptyRows, s.CSVHeaderRows,
			)
		}

	case "xml":
		switch {
		case s.XMLFormat == "" || s.XMLFormat == "unknown":
			return fmt.Sprintf(
				"XML: parsed %d byte(s) but found no <urlset> or <sitemapindex> elements (check it is a valid sitemap, not HTML or a different format)",
				s.ContentBytes,
			)
		case s.XMLFormat == "sitemapindex" && s.XMLChildSitemaps == 0:
			return "XML: <sitemapindex> contained no <sitemap><loc> entries"
		case s.XMLFormat == "sitemapindex" && s.XMLLocEntries == 0:
			return fmt.Sprintf(
				"XML: <sitemapindex> referenced %d child sitemap(s) but none produced any <url><loc> entries (%d failed to fetch/parse, depth reached %d)",
				s.XMLChildSitemaps, s.XMLDocumentsFailed, s.XMLDepthReached,
			)
		case s.XMLLocEntries == 0:
			return fmt.Sprintf("XML <%s>: contained 0 <url><loc> entries", s.XMLFormat)
		case s.URLsRejectedInvalid > 0 && s.URLsAccepted == 0:
			return fmt.Sprintf(
				"XML <%s>: scanned %d <loc> entries (%d empty, %d invalid http(s) URLs); none accepted",
				s.XMLFormat, s.XMLLocEntries, s.URLsRejectedEmpty, s.URLsRejectedInvalid,
			)
		default:
			return fmt.Sprintf(
				"XML <%s>: scanned %d <loc> entries (%d empty, %d invalid); 0 accepted",
				s.XMLFormat, s.XMLLocEntries, s.URLsRejectedEmpty, s.URLsRejectedInvalid,
			)
		}
	}
	return fmt.Sprintf("unsupported source type %q produced no URLs", s.SourceType)
}

// countingReader is a thin io.Reader wrapper that records bytes read so the
// stats reflect what the parser actually consumed from the source.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// get performs the HTTP GET with the site's User-Agent and returns the
// status-checked response plus the upstream-reported status code (set even on
// non-2xx so the caller can record it in stats).
func (p *URLParser) get(ctx context.Context, target, userAgent string) (*http.Response, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetching source: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		status := resp.StatusCode
		resp.Body.Close()
		return nil, status, fmt.Errorf("source returned status %d", status)
	}
	return resp, resp.StatusCode, nil
}

// ParseURLs fetches the URL source and returns the page URLs along with a
// ParseStats record describing what happened. userAgent is the site's
// configured User-Agent; sent so hosts that key on it behave consistently
// with the actual crawl. For xml sources, a <sitemapindex> is followed
// recursively into its child sitemaps.
//
// The stats value is always non-nil and is intended to be logged (and, when
// urls is empty, used to enrich the user-facing error via Diagnosis()).
func (p *URLParser) ParseURLs(ctx context.Context, sourceURL, sourceType, userAgent string, limit int) ([]string, *ParseStats, error) {
	stats := &ParseStats{
		SourceURL:  sourceURL,
		SourceType: strings.ToLower(sourceType),
	}

	switch stats.SourceType {
	case "csv":
		resp, status, err := p.get(ctx, sourceURL, userAgent)
		stats.HTTPStatus = status
		if err != nil {
			return nil, stats, err
		}
		defer resp.Body.Close()
		cr := &countingReader{r: resp.Body}
		urls, perr := p.parseCSV(cr, limit, stats)
		stats.ContentBytes = cr.n
		return urls, stats, perr

	case "xml":
		urls, perr := p.parseSitemap(ctx, sourceURL, userAgent, limit, 0, map[string]bool{}, stats)
		return urls, stats, perr

	default:
		return nil, stats, fmt.Errorf("unsupported source type: %s", sourceType)
	}
}

func (p *URLParser) parseCSV(reader io.Reader, limit int, stats *ParseStats) ([]string, error) {
	r := csv.NewReader(bufio.NewReaderSize(reader, 64*1024))
	r.LazyQuotes = true
	r.TrimLeadingSpace = true

	var urls []string

	for {
		if limit > 0 && len(urls) >= limit {
			stats.LimitReached = true
			break
		}

		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			stats.CSVMalformed++
			log.Warn().Err(err).Int("row", stats.CSVRowsScanned).Msg("skipping malformed CSV line")
			continue
		}
		stats.CSVRowsScanned++

		if len(record) == 0 {
			stats.CSVEmptyRows++
			continue
		}

		// First column contains the URL
		rawURL := strings.TrimSpace(record[0])
		if rawURL == "" {
			stats.CSVEmptyRows++
			continue
		}
		if rawURL == "url" || rawURL == "URL" {
			stats.CSVHeaderRows++
			continue
		}

		if isValidURL(rawURL) {
			stats.URLsAccepted++
			urls = append(urls, rawURL)
		} else {
			stats.URLsRejectedInvalid++
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
func (p *URLParser) parseSitemap(ctx context.Context, target, userAgent string, limit, depth int, seen map[string]bool, stats *ParseStats) ([]string, error) {
	if depth > maxSitemapDepth || seen[target] {
		return nil, nil
	}
	seen[target] = true
	if depth > stats.XMLDepthReached {
		stats.XMLDepthReached = depth
	}

	resp, status, err := p.get(ctx, target, userAgent)
	if depth == 0 {
		stats.HTTPStatus = status
	}
	if err != nil {
		stats.XMLDocumentsFailed++
		if depth == 0 {
			return nil, err
		}
		log.Warn().Err(err).Str("sitemap", target).Int("depth", depth).Msg("skipping unreachable child sitemap")
		return nil, nil
	}
	defer resp.Body.Close()

	// Transparently decompress gzip sitemaps, detected by magic bytes rather
	// than relying on Content-Type / .gz extension (both are unreliable).
	cr := &countingReader{r: resp.Body}
	br := bufio.NewReaderSize(cr, 64*1024)
	var reader io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, gzErr := gzip.NewReader(br)
		if gzErr != nil {
			stats.XMLDocumentsFailed++
			if depth == 0 {
				stats.ContentBytes = cr.n
				return nil, fmt.Errorf("decompressing sitemap: %w", gzErr)
			}
			log.Warn().Err(gzErr).Str("sitemap", target).Msg("skipping unreadable gzip child sitemap")
			return nil, nil
		}
		defer gz.Close()
		reader = gz
		stats.XMLGzipDecoded = true
	}

	stats.XMLDocumentsRead++

	var urls, children []string
	docFormat := "" // "urlset" / "sitemapindex" / mixed if both seen
	decoder := xml.NewDecoder(reader)
	for {
		if limit > 0 && len(urls) >= limit {
			stats.LimitReached = true
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
		case "urlset":
			if docFormat == "" {
				docFormat = "urlset"
			} else if docFormat != "urlset" {
				docFormat = "mixed"
			}
		case "sitemapindex":
			if docFormat == "" {
				docFormat = "sitemapindex"
			} else if docFormat != "sitemapindex" {
				docFormat = "mixed"
			}
		case "url": // <urlset> entry → a page URL
			stats.XMLLocEntries++
			var u sitemapLoc
			if decoder.DecodeElement(&u, &se) == nil {
				loc := strings.TrimSpace(u.Loc)
				switch {
				case loc == "":
					stats.URLsRejectedEmpty++
				case isValidURL(loc):
					stats.URLsAccepted++
					urls = append(urls, loc)
				default:
					stats.URLsRejectedInvalid++
				}
			}
		case "sitemap": // <sitemapindex> entry → a child sitemap
			stats.XMLChildSitemaps++
			var s sitemapLoc
			if decoder.DecodeElement(&s, &se) == nil {
				loc := strings.TrimSpace(s.Loc)
				switch {
				case loc == "":
					stats.URLsRejectedEmpty++
				case isValidURL(loc):
					children = append(children, loc)
				default:
					stats.URLsRejectedInvalid++
				}
			}
		}
	}

	if depth == 0 {
		stats.ContentBytes = cr.n
		stats.XMLFormat = docFormat
		if stats.XMLFormat == "" {
			stats.XMLFormat = "unknown"
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
		childURLs, _ := p.parseSitemap(ctx, child, userAgent, remaining, depth+1, seen, stats)
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
