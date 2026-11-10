// Package htmlx holds the small HTML helpers shared by fetching and exporting.
package htmlx

import (
	"strings"

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
