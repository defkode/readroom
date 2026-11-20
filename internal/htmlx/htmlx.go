// Package htmlx holds the small HTML helpers shared by fetching and exporting.
package htmlx

import (
	"regexp"
	"slices"
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
func Text(s string) string { return text(s, nil) }

// notSpoken is what a listener gains nothing from hearing read out.
var notSpoken = map[string]bool{"pre": true, "table": true, "figure": true, "svg": true, "math": true}

// Speech reduces HTML to text for reading aloud, one block per line: like
// Text, but without code listings, tables and figures, and with every line
// closed by punctuation, so that a heading or a list item does not run into
// the sentence after it.
func Speech(s string) string {
	lines := strings.Split(text(s, notSpoken), "\n")
	for i, l := range lines {
		// A link's number in the list of links, or a footnote's: nothing to say aloud.
		l = strings.Join(strings.Fields(linkNumber.ReplaceAllString(l, "")), " ")
		l = strings.NewReplacer(" .", ".", " ,", ",", " ;", ";", " :", ":").Replace(l)
		lines[i] = l
		if l != "" && !strings.ContainsRune(".!?:;…", lastLetter(l)) {
			lines[i] = l + "."
		}
	}
	return strings.Join(lines, "\n")
}

// linksSection finds the list of links a clean copy ends with (see
// LinksHeading) among an article's top-level nodes: its heading and the list
// itself, or nil for both. Nothing after it is the article's text, so it is
// neither read aloud nor translated nor counted.
func linksSection(nodes []*html.Node) (heading, list *html.Node) {
	var last []*html.Node
	for _, n := range nodes {
		if n.Type == html.ElementNode {
			last = append(last, n)
		}
	}
	if len(last) < 2 {
		return nil, nil
	}
	heading, list = last[len(last)-2], last[len(last)-1]
	if heading.Data != "h2" || list.Data != "ol" || strings.TrimSpace(text1(heading)) != LinksHeading {
		return nil, nil
	}
	return heading, list
}

// WithoutLinks returns an article's HTML without the list of links a clean copy
// ends with: the article proper.
func WithoutLinks(content string) string {
	nodes, err := ParseFragment(content)
	if err != nil {
		return content
	}
	heading, list := linksSection(nodes)
	if heading == nil {
		return content
	}
	var kept []*html.Node
	for _, n := range nodes {
		if n != heading && n != list {
			kept = append(kept, n)
		}
	}
	return Render(kept)
}

var linkNumber = regexp.MustCompile(`\[\d{1,3}\]`)

// lastLetter is the last character of l, looking past closing quotes and brackets.
func lastLetter(l string) rune {
	r := []rune(strings.TrimRight(l, `"'”’)]»`))
	if len(r) == 0 {
		return 0
	}
	return r[len(r)-1]
}

func text(s string, skip map[string]bool) string {
	nodes, err := ParseFragment(s)
	if err != nil {
		return s
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skip[n.Data] {
			b.WriteString("\n")
			return
		}
		if skip != nil && n.Type == html.ElementNode && n.Data == "a" && isAddress(text1(n)) {
			return // an address read out letter by letter tells a listener nothing
		}
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
	heading, list := linksSection(nodes)
	for _, n := range nodes {
		if skip != nil && (n == heading || n == list) {
			continue // links are for clicking
		}
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

// ---- pieces for translation --------------------------------------------------
//
// An article is translated piece by piece: each piece is a run of text with its
// inline markup (links, emphasis), and the blocks around the pieces — paragraphs,
// lists, figures, code listings — stay exactly as they are. That keeps the
// structure and the images out of a translator's hands, and lets a long article
// be done a few pieces at a time.

// pieceBlock is what separates one piece from the next.
var pieceBlock = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "aside": true, "header": true, "footer": true,
	"main": true, "nav": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"ul": true, "ol": true, "li": true, "dl": true, "dt": true, "dd": true, "blockquote": true, "figure": true,
	"figcaption": true, "table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true, "td": true,
	"th": true, "caption": true, "hr": true, "details": true, "summary": true, "address": true, "form": true,
}

// pieceSkip is never translated, nor anything inside it.
var pieceSkip = map[string]bool{
	"pre": true, "script": true, "style": true, "svg": true, "math": true, "iframe": true, "video": true,
	"audio": true, "object": true, "template": true,
}

// pieceVerbatim is inline text that is not language: a piece made of nothing else is left alone.
var pieceVerbatim = map[string]bool{"code": true, "kbd": true, "samp": true, "var": true}

func hasBlock(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if pieceBlock[n.Data] || pieceSkip[n.Data] {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasBlock(c) {
			return true
		}
	}
	return false
}

func hasLetters(n *html.Node) bool {
	if n.Type == html.TextNode {
		return strings.IndexFunc(n.Data, unicode.IsLetter) >= 0
	}
	if n.Type == html.ElementNode && pieceVerbatim[n.Data] {
		return false
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasLetters(c) {
			return true
		}
	}
	return false
}

// pieceRuns collects, in document order, every run of sibling inline nodes under
// parent that holds something to translate.
func pieceRuns(parent *html.Node, out *[][]*html.Node, skip ...*html.Node) {
	var run []*html.Node
	flush := func() {
		for _, n := range run {
			if hasLetters(n) {
				*out = append(*out, run)
				break
			}
		}
		run = nil
	}
	for c := parent.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode, c.Type == html.ElementNode && !hasBlock(c):
			run = append(run, c)
		default:
			flush()
			if c.Type == html.ElementNode && !pieceSkip[c.Data] && !slices.Contains(skip, c) {
				pieceRuns(c, out)
			}
		}
	}
	flush()
}

func pieceTree(content string) (*html.Node, [][]*html.Node, error) {
	nodes, err := ParseFragment(content)
	if err != nil {
		return nil, nil, err
	}
	root := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	// The links a clean copy ends with stay as they are: a source is found by
	// the title it was published under.
	heading, list := linksSection(nodes)
	var runs [][]*html.Node
	pieceRuns(root, &runs, heading, list)
	return root, runs, nil
}

// Pieces returns what there is to translate in an HTML fragment, in order: each
// piece is a run of text as HTML, with its inline markup.
func Pieces(content string) []string {
	_, runs, err := pieceTree(content)
	if err != nil {
		return nil
	}
	out := make([]string, len(runs))
	for i, run := range runs {
		out[i] = strings.TrimSpace(Render(run))
	}
	return out
}

// ReplacePieces returns content with each piece replaced by its translation, and
// false when there are not exactly as many translations as Pieces(content). A
// blank translation leaves that piece as it was.
func ReplacePieces(content string, translated []string) (string, bool) {
	root, runs, err := pieceTree(content)
	if err != nil || len(runs) != len(translated) {
		return "", false
	}
	for i, run := range runs {
		if strings.TrimSpace(translated[i]) == "" {
			continue
		}
		fresh, err := ParseFragment(translated[i])
		if err != nil {
			continue
		}
		parent := run[0].Parent
		for _, n := range fresh {
			parent.InsertBefore(n, run[0])
		}
		for _, n := range run {
			parent.RemoveChild(n)
		}
	}
	var kids []*html.Node
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		kids = append(kids, c)
	}
	return Render(kids), true
}
