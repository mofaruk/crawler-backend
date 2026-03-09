package source

import (
	"bufio"
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

// ParseURLs fetches the source file and streams URLs through the channel.
// This avoids loading the entire file into memory.
func (p *URLParser) ParseURLs(ctx context.Context, sourceURL, sourceType string, limit int) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching source: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("source returned status %d", resp.StatusCode)
	}

	switch strings.ToLower(sourceType) {
	case "csv":
		return p.parseCSV(resp.Body, limit)
	case "xml":
		return p.parseXML(resp.Body, limit)
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

// XML sitemap structures
type sitemapURLSet struct {
	URLs []sitemapURL `xml:"url"`
}

type sitemapURL struct {
	Loc string `xml:"loc"`
}

type sitemapIndex struct {
	Sitemaps []sitemapLoc `xml:"sitemap"`
}

type sitemapLoc struct {
	Loc string `xml:"loc"`
}

func (p *URLParser) parseXML(reader io.Reader, limit int) ([]string, error) {
	var urls []string
	decoder := xml.NewDecoder(bufio.NewReaderSize(reader, 64*1024))

	for {
		if limit > 0 && len(urls) >= limit {
			break
		}

		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		switch se := token.(type) {
		case xml.StartElement:
			if se.Name.Local == "url" {
				var u sitemapURL
				if err := decoder.DecodeElement(&u, &se); err == nil {
					loc := strings.TrimSpace(u.Loc)
					if loc != "" && isValidURL(loc) {
						urls = append(urls, loc)
					}
				}
			}
		}
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
