package crawler

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// variantSuffix matches the "-800x600" WordPress appends when it generates a
// resized copy of an upload, up to the file extension.
//
// Go's regexp has no lookahead, so the extension is captured as group 3 and
// put back when the suffix is stripped.
var variantSuffix = regexp.MustCompile(`-(\d{2,4})x(\d{2,4})(\.[A-Za-z0-9]+)$`)

// imageExtensions are the formats worth warming as images. AVIF and WebP are
// included because a modern browser is served those in preference to the JPEG,
// so warming only the JPEG warms a file most visitors never request.
var imageExtensions = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true,
	"webp": true, "avif": true, "svg": true, "bmp": true, "ico": true,
}

// FilterAssets narrows a page's asset references according to the site's mode.
//
// The input is every URL the page loads; on a real customer site that is
// thirteen thousand across three hundred pages, most of them the same image at
// six or eight sizes. Warming all of it is 39x the cost of crawling the pages
// alone, so each mode trades coverage for requests against the origin.
func FilterAssets(urls []string, mode string) []string {
	// Normalised here rather than trusting the caller: an "ALL" reaching this
	// unchanged would fall through to the default and quietly stop warming
	// stylesheets for a site that explicitly asked for everything.
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "none":
		return nil
	case "all":
		return urls
	case "images":
		return filterImages(urls)
	default: // top_variants
		return topVariants(filterImages(urls))
	}
}

func filterImages(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if imageExtensions[extensionOf(u)] {
			out = append(out, u)
		}
	}

	return out
}

// topVariants keeps each image at its two largest sizes.
//
// WordPress generates six to eight sizes of every upload; a visitor loads one,
// occasionally two. Keeping the largest pair covers a desktop and a mobile
// viewport, and drops the thumbnails that are rarely requested. An image with
// no size suffix is the original and is always kept.
func topVariants(urls []string) []string {
	type variant struct {
		url  string
		area int
	}

	groups := map[string][]variant{}
	var out []string

	for _, u := range urls {
		m := variantSuffix.FindStringSubmatch(u)
		if m == nil {
			// No size suffix: an original, an SVG, an icon. Always warmed —
			// there is no larger version to prefer.
			out = append(out, u)
			continue
		}

		w, _ := strconv.Atoi(m[1])
		h, _ := strconv.Atoi(m[2])

		// Group by the URL with the size removed, so every size of one upload
		// competes against its own siblings rather than another image's. The
		// extension is group 3 and has to survive the strip.
		key := variantSuffix.ReplaceAllString(u, "$3")
		groups[key] = append(groups[key], variant{url: u, area: w * h})
	}

	for _, vs := range groups {
		sort.Slice(vs, func(i, j int) bool {
			if vs[i].area != vs[j].area {
				return vs[i].area > vs[j].area
			}
			// Deterministic when two sizes have the same area, so a crawl
			// warms the same files every round.
			return vs[i].url < vs[j].url
		})

		for i, v := range vs {
			if i >= 2 {
				break
			}
			out = append(out, v.url)
		}
	}

	return out
}

// extensionOf returns a URL's lowercased file extension, ignoring any query
// string — a CDN that appends ?v=123 must not defeat the type check.
func extensionOf(rawURL string) string {
	path := rawURL
	if i := strings.IndexAny(path, "?#"); i != -1 {
		path = path[:i]
	}
	if i := strings.LastIndex(path, "/"); i != -1 {
		path = path[i+1:]
	}

	i := strings.LastIndex(path, ".")
	if i == -1 {
		return ""
	}

	return strings.ToLower(path[i+1:])
}

// IsAssetURL reports whether a URL points at something served as a static
// file — an image, stylesheet, script, font or media file — rather than a page.
//
// Used to decide which rate budget a request is charged to: a page costs the
// origin a PHP request and database queries, an asset usually does not.
func IsAssetURL(rawURL string) bool {
	ext := extensionOf(rawURL)
	if ext == "" {
		return false
	}

	if imageExtensions[ext] {
		return true
	}

	switch ext {
	case "css", "js", "mjs", "map",
		"woff", "woff2", "ttf", "otf", "eot",
		"mp4", "webm", "ogg", "mp3", "wav", "avi", "mov",
		"pdf", "zip", "gz", "ico":
		return true
	}

	return false
}
