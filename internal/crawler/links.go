package crawler

import (
	"sort"
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

		if isShareLink(target) {
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

// shareHosts are the endpoints social share buttons point at.
//
// Keyed by registrable domain and matched as a suffix, so a subdomain shim
// like l.facebook.com is covered by the facebook.com entry.
var shareHosts = map[string]struct{}{
	"addtoany.com":    {},
	"addthis.com":     {},
	"sharethis.com":   {},
	"pinterest.com":   {},
	"facebook.com":    {},
	"twitter.com":     {},
	"x.com":           {},
	"linkedin.com":    {},
	"reddit.com":      {},
	"tumblr.com":      {},
	"whatsapp.com":    {},
	"telegram.me":     {},
	"t.me":            {},
	"pocket.co":       {},
	"getpocket.com":   {},
	"digg.com":        {},
	"vk.com":          {},
	"buffer.com":      {},
	"flipboard.com":   {},
	"mix.com":         {},
	"xing.com":        {},
	"line.me":         {},
	"skype.com":       {},
	"messenger.com":   {},
	"myspace.com":     {},
	"livejournal.com": {},
	"blogger.com":     {},
	"evernote.com":    {},
	"trello.com":      {},
	"yummly.com":      {},
}

// sharePaths are the path prefixes a share endpoint uses, checked so an
// ordinary editorial link to one of these sites is still collected.
var sharePaths = []string{
	"/add_to/",
	"/share",
	"/sharer",
	"/intent/",
	"/submit",
	"/pin/create/",
	"/shareArticle",
	"/widgets/",
	"/dialog/",
}

// isShareLink reports whether a URL is a social share button rather than a
// link someone wrote.
//
// A share button is not an outbound link in any sense the customer cares
// about: it points at a widget endpoint that only means anything with a live
// visitor's click, and checking it says nothing about the page. Facebook and
// AddToAny answer 400 to a bare request, so every one of them landed in the
// report as a link that refused the check — nlphuset.dk had 172 AddToAny
// entries against 22 real ones, which buried the links that mattered.
//
// Matched on host *and* path: a page linking to its own Facebook profile or
// citing a tweet is an ordinary outbound link and is still collected.
func isShareLink(target *url.URL) bool {
	if !isShareHost(target.Host) {
		return false
	}

	// A share endpoint carries the page it shares as a parameter. This alone
	// identifies most of them, including forms not listed in sharePaths.
	for _, param := range []string{"linkurl", "url", "u", "text", "link"} {
		if target.Query().Get(param) != "" {
			return true
		}
	}

	path := strings.ToLower(target.Path)
	for _, prefix := range sharePaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}

	return false
}

// isShareHost reports whether a host belongs to one of the share services.
//
// Suffix matching rather than an exact lookup: Facebook routes its share
// buttons through l.facebook.com and its dialogs through
// web.facebook.com, and registrableHost only strips a "www." prefix.
func isShareHost(host string) bool {
	h := registrableHost(host)

	if _, ok := shareHosts[h]; ok {
		return true
	}

	for domain := range shareHosts {
		if strings.HasSuffix(h, "."+domain) {
			return true
		}
	}

	return false
}

// ShareHosts exposes the share-service domains for callers that must express
// the same rule elsewhere, such as a database query removing rows recorded
// before the filter existed.
func ShareHosts() []string {
	hosts := make([]string, 0, len(shareHosts))
	for host := range shareHosts {
		hosts = append(hosts, host)
	}

	sort.Strings(hosts)

	return hosts
}

// ShareMarks exposes the substrings that identify a share endpoint within one
// of those hosts, so a query can tell a share button from an ordinary link.
func ShareMarks() []string {
	marks := append([]string{}, sharePaths...)
	for _, param := range []string{"linkurl=", "url=", "u=", "text=", "link="} {
		marks = append(marks, "?"+param, "&"+param)
	}

	sort.Strings(marks)

	return marks
}
