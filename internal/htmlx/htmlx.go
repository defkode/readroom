// Package htmlx holds the small HTML helpers shared by fetching and exporting.
package htmlx

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var policy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("loading").OnElements("img")
	return p
}()

// Sanitize strips scripts, styles and other unsafe markup from untrusted HTML.
func Sanitize(s string) string { return policy.Sanitize(s) }

// ParseFragment parses an HTML fragment as if it were the contents of <body>.
func ParseFragment(s string) ([]*html.Node, error) {
	body := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	return html.ParseFragment(strings.NewReader(s), body)
}

// Render serialises nodes back to an HTML string.
func Render(nodes []*html.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		html.Render(&b, n)
	}
	return b.String()
}

// Walk calls fn for every node in the trees, parents before children.
func Walk(nodes []*html.Node, fn func(*html.Node)) {
	for _, n := range nodes {
		fn(n)
		var kids []*html.Node
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			kids = append(kids, c)
		}
		Walk(kids, fn)
	}
}

func Attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func SetAttr(n *html.Node, key, val string) {
	for i, a := range n.Attr {
		if a.Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func DelAttr(n *html.Node, key string) {
	out := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Key != key {
			out = append(out, a)
		}
	}
	n.Attr = out
}

// Flat is Text on a single line, for excerpts and length checks.
func Flat(s string) string { return strings.Join(strings.Fields(Text(s)), " ") }

// Lead returns the opening of text, at most max runes, cut at the end of a
// sentence when one falls in the second half, otherwise at a word with an ellipsis.
func Lead(text string, max int) string {
	r := []rune(text)
	if len(r) <= max {
		return text
	}
	cut := -1
	for i := max - 1; i >= max/2; i-- {
		if (r[i] == '.' || r[i] == '!' || r[i] == '?') && i+1 < len(r) && r[i+1] == ' ' {
			cut = i + 1
			break
		}
	}
	if cut > 0 {
		return string(r[:cut])
	}
	for i := max - 1; i >= max/2; i-- {
		if r[i] == ' ' {
			return strings.TrimRight(string(r[:i]), " ,;:–-") + "…"
		}
	}
	return string(r[:max]) + "…"
}

// norm reduces text to lower-case letters and digits, for loose comparison.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// dropPrefix removes a leading repetition of title from p, ignoring case and
// punctuation, and reports whether it did.
func dropPrefix(p, title string) (string, bool) {
	want := utf8.RuneCountInString(norm(title))
	if want == 0 || !strings.HasPrefix(norm(p), norm(title)) {
		return p, false
	}
	seen := 0
	for i, r := range p {
		if seen == want {
			return strings.TrimLeft(p[i:], " \t:;,.|·—–-"), true
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			seen++
		}
	}
	return "", true
}

// endsSentence reports whether p reads like prose rather than a label.
func endsSentence(p string) bool {
	p = strings.TrimRight(p, " \"'”’)»")
	for _, end := range []string{".", "!", "?", "…", ":"} {
		if strings.HasSuffix(p, end) {
			return true
		}
	}
	return false
}

// Excerpt builds a teaser of at most max runes from an article body: the first
// real paragraph, skipping a repeated title and the short label-like lines that
// often open an article (site name, byline, date). For an entry that is only an
// image it falls back to the image's caption.
func Excerpt(content, title string, max int) string {
	var kept []string
	length, opening := 0, true
	for i, p := range strings.Split(Text(content), "\n") {
		if opening && i < 8 {
			rest, wasTitle := dropPrefix(p, title)
			if wasTitle {
				p = rest
			}
			if p == "" || (utf8.RuneCountInString(p) < 140 && !endsSentence(p)) {
				continue
			}
		}
		opening = false
		kept = append(kept, p)
		if length += utf8.RuneCountInString(p); length >= max {
			break
		}
	}
	if len(kept) > 0 {
		return Lead(strings.Join(kept, " "), max)
	}
	// Nothing read like prose: use whatever text there is, unless it only repeats the title.
	if flat := Flat(content); flat != "" {
		if rest, _ := dropPrefix(flat, title); rest != "" {
			return Lead(rest, max)
		}
		return ""
	}
	_, caption := FirstImage(content)
	return Lead(caption, max)
}

// FirstImage returns the source and caption (title, else alt text) of the first
// image in an HTML fragment.
func FirstImage(content string) (src, caption string) {
	nodes, err := ParseFragment(content)
	if err != nil {
		return "", ""
	}
	found := false
	Walk(nodes, func(n *html.Node) {
		if found || n.Type != html.ElementNode || n.Data != "img" || Attr(n, "src") == "" {
			return
		}
		found = true
		src = Attr(n, "src")
		if caption = strings.TrimSpace(Attr(n, "title")); caption == "" {
			caption = strings.TrimSpace(Attr(n, "alt"))
		}
	})
	return src, strings.Join(strings.Fields(caption), " ")
}

var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "blockquote": true, "pre": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "section": true, "article": true,
}

// Text reduces HTML to readable plain text, one block per line.
func Text(s string) string {
	nodes, err := ParseFragment(s)
	if err != nil {
		return s
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockTags[n.Data] {
			b.WriteString("\n")
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	var lines []string
	for _, l := range strings.Split(b.String(), "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}
