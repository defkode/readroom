package export

import (
	"fmt"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"

	"readroom/internal/store"
)

// Markdown renders one entry as a Markdown document with YAML front matter.
// baseURL (e.g. "http://nas:8090") makes preserved image links absolute.
func Markdown(e *store.Entry, baseURL string) (string, error) {
	body, err := htmltomarkdown.ConvertString(e.Body(), converter.WithDomain(baseURL))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %q\n", e.Title)
	fmt.Fprintf(&b, "feed: %q\n", e.FeedTitle)
	if e.Author != "" {
		fmt.Fprintf(&b, "author: %q\n", e.Author)
	}
	fmt.Fprintf(&b, "published: %s\n", e.PublishedAt)
	if e.URL != "" {
		fmt.Fprintf(&b, "url: %s\n", e.URL)
	}
	b.WriteString("---\n\n# " + e.Title + "\n\n")
	if e.Summary != "" {
		for _, line := range strings.Split(e.Summary, "\n") {
			b.WriteString("> " + line + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimSpace(body) + "\n")
	return b.String(), nil
}

// MarkdownAll joins several entries into one document.
func MarkdownAll(title string, entries []*store.Entry, baseURL string) (string, error) {
	var b strings.Builder
	for i, e := range entries {
		md, err := Markdown(e, baseURL)
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		b.WriteString(md)
	}
	return b.String(), nil
}
