package source

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeSiteURL turns whatever a customer typed into a URL the crawler can
// use.
//
// Customers type a domain, not a URL — "billigfilter.dk", sometimes with a
// stray "www.", a trailing slash, or a pasted path. Requiring them to write
// "https://" is a pointless obstacle, so the scheme is filled in when it is
// missing.
//
// https is assumed rather than http: every site this product is sold to
// serves https, and guessing http would mean crawling a redirect chain on
// every single URL.
func NormalizeSiteURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("enter a domain, for example billigfilter.dk")
	}

	// A bare domain has no "//" — but neither does "http:/example.com", so
	// only add the scheme when there is no scheme-looking prefix at all.
	if !strings.Contains(s, "://") {
		// Reject an explicit non-web scheme rather than silently prefixing it
		// into something meaningless like "https://mailto:x@y".
		if i := strings.Index(s, ":"); i > 0 && !isPort(s[i+1:]) {
			return "", fmt.Errorf("%q is not a web address", raw)
		}
		s = "https://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid domain", raw)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("only http and https addresses can be crawled")
	}

	host := strings.ToLower(strings.TrimSpace(u.Host))
	if host == "" {
		return "", fmt.Errorf("enter a domain, for example billigfilter.dk")
	}

	// A hostname with no dot is either a typo or an internal name; neither is
	// a site we can crawl from the public internet.
	hostOnly := host
	if h, _, ok := strings.Cut(host, ":"); ok {
		hostOnly = h
	}
	if !strings.Contains(hostOnly, ".") {
		return "", fmt.Errorf("%q does not look like a domain — did you mean %s.dk?", raw, hostOnly)
	}
	if strings.HasPrefix(hostOnly, ".") || strings.HasSuffix(hostOnly, ".") {
		return "", fmt.Errorf("%q is not a valid domain", raw)
	}

	u.Host = host
	u.Fragment = ""

	// Keep a real path ("/shop"), drop a bare "/" so the stored value is the
	// clean origin.
	if u.Path == "/" {
		u.Path = ""
	}

	return u.String(), nil
}

// isPort reports whether s starts with digits, i.e. the colon was a port
// separator ("example.dk:8080") rather than a scheme marker ("mailto:x").
func isPort(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return r == '/' // "example.dk:8080/path"
		}
	}
	return true
}
