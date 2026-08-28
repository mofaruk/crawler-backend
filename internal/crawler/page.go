package crawler

import (
	"io"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// PageSignals are the on-page facts worth reporting, extracted from HTML the
// fetcher already downloads.
//
// The body was being read and discarded purely to free the connection for
// reuse. Parsing it on the way past costs one pass over bytes already in
// memory and no extra requests, which is what makes broad page-quality
// detection affordable here.
type PageSignals struct {
	Title           string `bson:"title,omitempty" json:"title,omitempty"`
	TitleLength     int    `bson:"title_length,omitempty" json:"title_length,omitempty"`
	MetaDescription string `bson:"meta_description,omitempty" json:"meta_description,omitempty"`
	MetaDescLength  int    `bson:"meta_desc_length,omitempty" json:"meta_desc_length,omitempty"`
	Canonical       string `bson:"canonical,omitempty" json:"canonical,omitempty"`
	NoIndex         bool   `bson:"noindex,omitempty" json:"noindex,omitempty"`
	H1Count         int    `bson:"h1_count,omitempty" json:"h1_count,omitempty"`
	WordCount       int    `bson:"word_count,omitempty" json:"word_count,omitempty"`
	ImagesMissingAlt int   `bson:"images_missing_alt,omitempty" json:"images_missing_alt,omitempty"`
	InsecureRefs    int    `bson:"insecure_refs,omitempty" json:"insecure_refs,omitempty"`
	// SoftNotFound marks a page that returned 200 but reads as an error page.
	// These are worse than a real 404: search engines index them and users
	// get no signal that the link is dead.
	SoftNotFound bool `bson:"soft_not_found,omitempty" json:"soft_not_found,omitempty"`
}

// softNotFoundPhrases are matched against the page title only. Matching body
// text produced false positives on pages that merely *discuss* 404s, and the
// title is where a real error page announces itself.
var softNotFoundPhrases = []string{
	"not found", "page not found", "page doesn't exist",
	"page does not exist", "ikke fundet", "siden findes ikke",
}

// ExtractPageSignals parses an HTML document. It never returns an error: a
// malformed page still yields whatever was read before the break, which is
// more useful than discarding the lot.
func ExtractPageSignals(r io.Reader, pageIsHTTPS bool) PageSignals {
	var s PageSignals
	z := html.NewTokenizer(r)

	var (
		inTitle bool
		inBody  bool
		// Script and style bodies are text tokens too. Counting them lets an
		// inline analytics blob or a JSON-LD block push an otherwise empty page
		// past the thin-content threshold, so the page never gets reported.
		inScript bool
		words   int
	)

	for {
		switch z.Next() {
		case html.ErrorToken:
			s.WordCount = words
			s.TitleLength = len([]rune(s.Title))
			s.MetaDescLength = len([]rune(s.MetaDescription))
			s.SoftNotFound = looksLikeNotFound(s.Title)
			return s

		case html.TextToken:
			text := strings.TrimSpace(string(z.Text()))
			if inTitle && text != "" {
				s.Title = strings.Join(strings.Fields(text), " ")
			}
			if inBody && !inScript && text != "" {
				words += len(strings.Fields(text))
			}

		case html.StartTagToken, html.SelfClosingTagToken:
			nameBytes, hasAttr := z.TagName()
			tag := string(nameBytes)

			switch tag {
			case "title":
				inTitle = true
			case "body":
				inBody = true
			case "script", "style", "noscript", "template":
				inScript = true
			case "h1":
				s.H1Count++
			}

			if !hasAttr {
				continue
			}

			attrs := map[string]string{}
			for {
				k, v, more := z.TagAttr()
				attrs[strings.ToLower(string(k))] = string(v)
				if !more {
					break
				}
			}

			switch tag {
			case "meta":
				switch strings.ToLower(attrs["name"]) {
				case "description":
					s.MetaDescription = strings.TrimSpace(attrs["content"])
				case "robots":
					if strings.Contains(strings.ToLower(attrs["content"]), "noindex") {
						s.NoIndex = true
					}
				}
			case "link":
				if strings.EqualFold(attrs["rel"], "canonical") {
					s.Canonical = strings.TrimSpace(attrs["href"])
				}
			case "img":
				if strings.TrimSpace(attrs["alt"]) == "" {
					s.ImagesMissingAlt++
				}
				// An https page loading http assets is mixed content: browsers
				// block it, so the page renders broken for real visitors.
				if pageIsHTTPS && strings.HasPrefix(strings.ToLower(attrs["src"]), "http://") {
					s.InsecureRefs++
				}
			case "script", "iframe":
				if pageIsHTTPS && strings.HasPrefix(strings.ToLower(attrs["src"]), "http://") {
					s.InsecureRefs++
				}
			}

		case html.EndTagToken:
			nameBytes, _ := z.TagName()
			switch string(nameBytes) {
			case "title":
				inTitle = false
			case "script", "style", "noscript", "template":
				inScript = false
			}
		}
	}
}

func looksLikeNotFound(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		return false
	}
	for _, p := range softNotFoundPhrases {
		if strings.Contains(t, p) {
			return true
		}
	}

	// A bare "404" is only taken as an error page when the title is short and
	// mostly consists of it. This is a critical-severity finding, and titles
	// like "Filter model 404" or "Ventilation 4045 spare part" are product
	// names, not errors — a real error page announces itself in a few words
	// ("404", "Error 404", "404 | Site").
	words := strings.FieldsFunc(t, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})

	if len(words) > 3 {
		return false
	}

	for _, w := range words {
		if w == "404" {
			return true
		}
	}

	return false
}
