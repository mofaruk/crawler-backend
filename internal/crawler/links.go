package crawler

import (
	"net/url"
	"strings"
)

// OutboundLink is one external destination found on a page.
type OutboundLink struct {
	// URL is the absolute, normalised destination.
	URL string
	// FoundOn is the page that linked to it.
	FoundOn string
}

// ExternalLinks resolves raw hrefs against the page they appeared on and keeps
// only the ones pointing off-site.
//
// Internal links are excluded because the crawl already visits them: reporting
// them here would duplicate findings the cache crawl produces anyway, and
// double the checking work.
//
// Fragments are dropped and the result deduplicated per page, so a nav bar
// linking the same partner site eight times yields one destination.
func ExternalLinks(pageURL string, hrefs []string) []OutboundLink {
	page, err := url.Parse(pageURL)
	if err != nil || page.Host == "" {
		return nil
	}

	pageHost := registrableHost(page.Host)

	seen := make(map[string]struct{}, len(hrefs))
	var out []OutboundLink

	for _, raw := range hrefs {
		target := resolveLink(page, raw)
		if target == nil {
			continue
		}

		if registrableHost(target.Host) == pageHost {
			continue
		}

		key := target.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		out = append(out, OutboundLink{URL: key, FoundOn: pageURL})
	}

	return out
}

// resolveLink turns one href into an absolute http(s) URL, or nil if it is not
// something that can be fetched.
func resolveLink(page *url.URL, raw string) *url.URL {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	// mailto:, tel:, javascript: and friends are not fetchable, and a bare
	// fragment is a link to the same page.
	if strings.HasPrefix(raw, "#") {
		return nil
	}

	ref, err := url.Parse(raw)
	if err != nil {
		return nil
	}

	target := page.ResolveReference(ref)
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil
	}
	if target.Host == "" {
		return nil
	}

	// The fragment is a position within the page, not a separate destination:
	// keeping it would check the same URL once per anchor used.
	target.Fragment = ""

	return target
}

// registrableHost strips a leading "www." and the port so that
// "www.example.dk", "example.dk" and "example.dk:443" compare as one site.
//
// Deliberately not a public-suffix lookup: treating "shop.example.dk" as
// external to "example.dk" is the useful behaviour here, since a broken link
// to a subdomain is still worth reporting.
func registrableHost(host string) string {
	h := strings.ToLower(host)

	if i := strings.LastIndex(h, ":"); i != -1 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}

	return strings.TrimPrefix(h, "www.")
}
