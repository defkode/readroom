package htmlx

import (
	"bytes"
	stdhtml "html"
	"strconv"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/net/html"
)

// ---- the clean copy of an article ---------------------------------------------
//
// What a feed or a page sends is rarely just the article: there is a byline with
// the author's biography, a line asking to subscribe, links to other stories,
// markup that means nothing. A clean copy is made in two steps that never let a
// model write the article's words:
//
//   1. Blocks cuts the HTML into a flat list of blocks — heading, paragraph, list
//      item, quotation, code listing, picture, table — each reduced to the few
//      inline marks worth keeping (link, bold, italic, code, image).
//   2. Something outside — a model, through the API — says for each block
//      whether it stays and what it is. Assemble puts the ones that stay back
//      together as plain HTML, which is stored as Markdown.
//
// So the worst a bad judgment can do is drop a block or mislabel one; it cannot
// change a sentence.

// Block kinds.
const (
	BlockHeading = "heading"
	BlockText    = "text"
	BlockItem    = "item" // one item of a list
	BlockCode    = "code"
	BlockImage   = "image"
	BlockTable   = "table"
	BlockRule    = "rule"
)

// Block is one piece of an article's structure.
type Block struct {
	Kind    string
	Level   int    // heading: 2 to 6
	Quote   bool   // text or item standing inside a quotation
	Ordered bool   // item: of a numbered list
	Depth   int    // item: 0 at the top, 1 for a list inside an item, …
	HTML    string // what it holds: inline HTML; for code its text; for an image the <img>; for a table the <table>
	Caption string // image: its caption, as inline HTML
}

// Label is how a block is shown to whoever judges it: "h2", "text", "quote",
// "item", "code", "image", "table", "rule".
func (b Block) Label() string {
	switch {
	case b.Kind == BlockHeading:
		return "h" + strconv.Itoa(b.Level)
	case b.Kind == BlockText && b.Quote:
		return "quote"
	}
	return b.Kind
}

// Text is the block as plain text, for whoever judges it.
func (b Block) Text() string {
	switch b.Kind {
	case BlockCode:
		return strings.Join(strings.Fields(b.HTML), " ")
	case BlockImage:
		nodes, _ := ParseFragment(b.HTML)
		alt := ""
		Walk(nodes, func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "img" {
				alt = Attr(n, "alt")
			}
		})
		return strings.TrimSpace(alt + " " + Flat(b.Caption))
	case BlockTable:
		// Cells would otherwise run together.
		return Flat(strings.NewReplacer("</td>", " </td>", "</th>", " </th>").Replace(b.HTML))
	}
	return Flat(b.HTML)
}

// The roles a block can be given. RoleKeep leaves it as it is; the others that
// name a kind apply to text only — a picture or a table can be kept or dropped.
const (
	RoleKeep  = "keep"
	RoleDrop  = "drop"
	RoleH2    = "h2"
	RoleH3    = "h3"
	RoleText  = "text"
	RoleQuote = "quote"
)

// ValidRole reports whether r is one of the roles.
func ValidRole(r string) bool {
	switch r {
	case "", RoleKeep, RoleDrop, RoleH2, RoleH3, RoleText, RoleQuote:
		return true
	}
	return false
}

// inlineKept maps the inline elements that survive to what they become.
var inlineKept = map[string]string{
	"a": "a", "strong": "strong", "b": "strong", "em": "em", "i": "em", "code": "code", "br": "br",
	"img": "img", "sub": "sub", "sup": "sup", "del": "del", "s": "del", "strike": "del",
}

// inline writes n's subtree as inline HTML made only of inlineKept; every other
// element gives way to its contents.
func inline(b *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(stdhtml.EscapeString(n.Data))
		return
	case html.ElementNode:
	default:
		return
	}
	if pieceSkip[n.Data] {
		return
	}
	tag, kept := inlineKept[n.Data]
	var inner strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		inline(&inner, c)
	}
	switch {
	case !kept:
		b.WriteString(inner.String())
	case tag == "br":
		b.WriteString("<br>")
	case tag == "img":
		if src := Attr(n, "src"); src != "" {
			b.WriteString(`<img src="` + stdhtml.EscapeString(src) + `" alt="` + stdhtml.EscapeString(strings.TrimSpace(Attr(n, "alt"))) + `">`)
		}
	case strings.TrimSpace(inner.String()) == "":
		b.WriteString(inner.String()) // an empty link or emphasis is only noise
	case tag == "a":
		if href := Attr(n, "href"); href != "" {
			b.WriteString(`<a href="` + stdhtml.EscapeString(href) + `">` + inner.String() + `</a>`)
		} else {
			b.WriteString(inner.String())
		}
	default:
		b.WriteString("<" + tag + ">" + inner.String() + "</" + tag + ">")
	}
}

func inlineOf(nodes []*html.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		inline(&b, n)
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	// A line break at either end separates nothing.
	for {
		trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(out, "<br>"), "<br>"))
		if trimmed == out {
			return out
		}
		out = trimmed
	}
}

func children(n *html.Node) []*html.Node {
	var kids []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		kids = append(kids, c)
	}
	return kids
}

// firstImage finds the first <img> with a source under n.
func firstImage(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.Data == "img" && Attr(n, "src") != "" {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if img := firstImage(c); img != nil {
			return img
		}
	}
	return nil
}

type blockCutter struct {
	out     []Block
	quote   bool
	inList  bool
	ordered bool
	depth   int
}

// run turns a run of inline nodes into a block: text, or the pictures it holds
// when it has no words.
func (c *blockCutter) run(nodes []*html.Node) {
	words := false
	for _, n := range nodes {
		words = words || hasLetters(n)
	}
	if !words {
		for _, n := range nodes {
			Walk([]*html.Node{n}, func(n *html.Node) {
				if n.Type == html.ElementNode && n.Data == "img" && Attr(n, "src") != "" {
					c.out = append(c.out, Block{Kind: BlockImage, HTML: inlineOf([]*html.Node{n})})
				}
			})
		}
		return
	}
	b := Block{Kind: BlockText, Quote: c.quote, HTML: inlineOf(nodes)}
	if c.inList {
		b.Kind, b.Ordered, b.Depth = BlockItem, c.ordered, c.depth
	}
	c.out = append(c.out, b)
}

func (c *blockCutter) walk(parent *html.Node) {
	var run []*html.Node
	flush := func() {
		if len(run) > 0 {
			c.run(run)
			run = nil
		}
	}
	for n := parent.FirstChild; n != nil; n = n.NextSibling {
		if n.Type == html.TextNode || (n.Type == html.ElementNode && !hasBlock(n)) {
			run = append(run, n)
			continue
		}
		flush()
		if n.Type != html.ElementNode {
			continue
		}
		switch n.Data {
		case "pre":
			if code := strings.Trim(text1(n), "\n"); strings.TrimSpace(code) != "" {
				c.out = append(c.out, Block{Kind: BlockCode, HTML: code})
			}
		case "h1", "h2", "h3", "h4", "h5", "h6":
			if h := inlineOf(children(n)); Flat(h) != "" {
				// The article's own title is the page's first heading, so nothing in the text outranks h2.
				level := max(2, int(n.Data[1]-'0'))
				c.out = append(c.out, Block{Kind: BlockHeading, Level: level, HTML: h})
			}
		case "hr":
			c.out = append(c.out, Block{Kind: BlockRule})
		case "table":
			if !dataTable(n) {
				c.walk(n) // a table used to lay the page out: what counts is in its cells
				continue
			}
			if Flat(Render([]*html.Node{n})) != "" {
				c.out = append(c.out, Block{Kind: BlockTable, HTML: Sanitize(Render([]*html.Node{n}))})
			}
		case "figure":
			img := firstImage(n)
			if img == nil {
				c.walk(n) // a figure around a quotation or a table
				continue
			}
			b := Block{Kind: BlockImage, HTML: inlineOf([]*html.Node{img})}
			Walk([]*html.Node{n}, func(f *html.Node) {
				if f.Type == html.ElementNode && f.Data == "figcaption" && b.Caption == "" {
					b.Caption = inlineOf(children(f))
				}
			})
			c.out = append(c.out, b)
		case "blockquote":
			was := c.quote
			c.quote = true
			c.walk(n)
			c.quote = was
		case "ul", "ol":
			was := *c
			if c.inList {
				c.depth++
			}
			c.inList, c.ordered = true, n.Data == "ol"
			c.walk(n)
			c.inList, c.ordered, c.depth = was.inList, was.ordered, was.depth
		default:
			if !pieceSkip[n.Data] {
				c.walk(n)
			}
		}
	}
	flush()
}

// dataTable tells a table of data from one that only arranges the page, as
// newsletters written for e-mail are built: tables within tables, most of one
// cell. A table of data has no table inside it and at least two rows, each of
// the same two or more cells.
func dataTable(table *html.Node) bool {
	var rows []int
	nested := false
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.Data {
			case "table":
				nested = true
			case "tr":
				cells := 0
				for d := c.FirstChild; d != nil; d = d.NextSibling {
					if d.Type == html.ElementNode && (d.Data == "td" || d.Data == "th") {
						cells++
						walk(d) // only to find a table inside
					}
				}
				rows = append(rows, cells)
			default:
				walk(c)
			}
		}
	}
	walk(table)
	if nested || len(rows) < 2 || rows[0] < 2 {
		return false
	}
	for _, cells := range rows {
		if cells != rows[0] {
			return false
		}
	}
	return true
}

// text1 is the text under n, exactly as written.
func text1(n *html.Node) string {
	var b strings.Builder
	Walk([]*html.Node{n}, func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
	})
	return b.String()
}

// Blocks cuts an article's HTML into its blocks, in order.
func Blocks(content string) []Block {
	nodes, err := ParseFragment(content)
	if err != nil {
		return nil
	}
	root := &html.Node{Type: html.ElementNode, Data: "div"}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	c := &blockCutter{}
	c.walk(root)
	return c.out
}

// Assemble puts blocks back together as HTML, each in the role given for it, and
// reports false when roles are not one per block or one of them is unknown. The
// result uses nothing but headings, paragraphs, quotations, lists, code listings,
// pictures, tables and the inline marks Blocks keeps.
func Assemble(blocks []Block, roles []string) (string, bool) {
	if len(roles) != len(blocks) {
		return "", false
	}
	var kept []Block
	for i, b := range blocks {
		role := roles[i]
		if !ValidRole(role) {
			return "", false
		}
		if role == RoleDrop {
			continue
		}
		if b.Kind == BlockHeading || b.Kind == BlockText || b.Kind == BlockItem {
			switch role {
			case RoleH2, RoleH3:
				b = Block{Kind: BlockHeading, Level: int(role[1] - '0'), HTML: stripMarks(b.HTML)}
			case RoleText:
				b = Block{Kind: BlockText, HTML: b.HTML}
			case RoleQuote:
				b = Block{Kind: BlockText, Quote: true, HTML: b.HTML}
			}
		}
		kept = append(kept, b)
	}
	// A rule that no longer separates two things separates nothing.
	var out []Block
	for i, b := range kept {
		if b.Kind == BlockRule && (len(out) == 0 || i == len(kept)-1 || out[len(out)-1].Kind == BlockRule) {
			continue
		}
		out = append(out, b)
	}

	var w strings.Builder
	var lists []string // the lists open now, outermost first: "ul" or "ol"
	quote := false
	closeLists := func(to int) {
		for len(lists) > to {
			w.WriteString("</li></" + lists[len(lists)-1] + ">")
			lists = lists[:len(lists)-1]
		}
	}
	for _, b := range out {
		if b.Kind != BlockItem {
			closeLists(0)
		}
		if wants := b.Quote && (b.Kind == BlockText || b.Kind == BlockItem); wants != quote {
			closeLists(0)
			if quote = wants; quote {
				w.WriteString("<blockquote>")
			} else {
				w.WriteString("</blockquote>")
			}
		}
		switch b.Kind {
		case BlockHeading:
			h := "h" + strconv.Itoa(b.Level)
			w.WriteString("<" + h + ">" + b.HTML + "</" + h + ">")
		case BlockText:
			w.WriteString("<p>" + b.HTML + "</p>")
		case BlockItem:
			tag := "ul"
			if b.Ordered {
				tag = "ol"
			}
			depth := min(b.Depth, len(lists)) // an item cannot be deeper than one more than what is open
			switch {
			case depth == len(lists):
				w.WriteString("<" + tag + ">")
				lists = append(lists, tag)
			default:
				closeLists(depth + 1)
				if lists[depth] != tag {
					closeLists(depth)
					w.WriteString("<" + tag + ">")
					lists = append(lists, tag)
				} else {
					w.WriteString("</li>")
				}
			}
			w.WriteString("<li>" + b.HTML)
		case BlockCode:
			w.WriteString("<pre><code>" + stdhtml.EscapeString(b.HTML) + "</code></pre>")
		case BlockImage:
			w.WriteString("<p>" + b.HTML + "</p>")
			if b.Caption != "" {
				w.WriteString("<p><em>" + stripMarks(b.Caption) + "</em></p>")
			}
		case BlockTable:
			w.WriteString(b.HTML)
		case BlockRule:
			w.WriteString("<hr>")
		}
	}
	closeLists(0)
	if quote {
		w.WriteString("</blockquote>")
	}
	return w.String(), true
}

// stripMarks removes bold and italic from inline HTML: a heading or a caption is
// already set apart, and a paragraph that was all bold because it stood in for a
// heading should not stay bold as one.
func stripMarks(s string) string {
	return strings.NewReplacer("<strong>", "", "</strong>", "", "<em>", "", "</em>", "").Replace(s)
}

var toMarkdown = converter.NewConverter(converter.WithPlugins(
	base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), strikethrough.NewStrikethroughPlugin(), table.NewTablePlugin()))

// Markdown converts article HTML to Markdown.
func Markdown(content string) (string, error) {
	md, err := toMarkdown.ConvertString(content)
	return strings.TrimSpace(md) + "\n", err
}

var fromMarkdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

// FromMarkdown renders Markdown as HTML, sanitised like any other content.
func FromMarkdown(md string) (string, error) {
	var b bytes.Buffer
	if err := fromMarkdown.Convert([]byte(md), &b); err != nil {
		return "", err
	}
	return Sanitize(b.String()), nil
}
