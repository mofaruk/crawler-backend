package crawler

import (
	"io"
	"strings"

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
	"404", "not found", "page not found", "page doesn't exist",
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
			if inBody && text != "" {
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
			if string(nameBytes) == "title" {
				inTitle = false
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
	return false
}
