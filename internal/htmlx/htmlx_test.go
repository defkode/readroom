package htmlx

import (
	"strings"
	"testing"
)

func TestExcerpt(t *testing.T) {
	cases := []struct{ name, title, content, want string }{
		{"plain first paragraph", "Hello",
			`<p>This is the opening sentence of the article.</p><p>Second paragraph.</p>`,
			"This is the opening sentence of the article. Second paragraph."},
		{"title repeated as its own paragraph", "Introducing Our Smallest Model: Ember",
			`<p><strong><a href="#">Introducing Our Smallest Model: Ember</a></strong></p>Meta just launched their entry in the small-model category.`,
			"Meta just launched their entry in the small-model category."},
		{"title repeated inline", "Yonkers Rises",
			`<p>Yonkers Rises — a long read about a city and how it changed over a decade.</p>`,
			"a long read about a city and how it changed over a decade."},
		{"site name, title, byline and date before the text", "A new Go API for JSON",
			`<h1>The Go Blog</h1><h1>A new Go API for JSON</h1><p>Michael Pratt, Joe Tsai, and Damien Neil<br>4 November 2026</p><h2>Introduction</h2><p>JSON is a simple data interchange format.</p>`,
			"JSON is a simple data interchange format."},
		{"image only: caption from the title attribute", "Dragonfly",
			`<img src="https://example.com/c.png" title="The hover text is the joke." alt="Dragonfly">`,
			"The hover text is the joke."},
		{"image only: falls back to alt", "Photo",
			`<img src="https://example.com/c.png" alt="A harbour at dusk">`, "A harbour at dusk"},
		{"only the title, nothing else", "Just a title", `<p>Just a title</p>`, ""},
		{"short note without punctuation is still shown", "Release", `<p>Version 2 is out</p>`, "Version 2 is out"},
		{"empty", "x", ``, ""},
	}
	for _, c := range cases {
		if got := Excerpt(c.content, c.title, 280); got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

func TestLeadCutsAtSentence(t *testing.T) {
	text := "First sentence is here. Second sentence follows it closely. Third one is the longest sentence of them all by far."
	if got := Lead(text, 70); got != "First sentence is here. Second sentence follows it closely." {
		t.Errorf("got %q", got)
	}
	if got := Lead("word "+text[:0]+"averyveryverylongunbrokenstringwithoutspaces and more words after that", 30); got == "" || len([]rune(got)) > 31 {
		t.Errorf("word cut: %q", got)
	}
}

func TestSpeech(t *testing.T) {
	got := Speech(`<h2>A heading</h2><p>First sentence. Second “one.”</p><pre>code()</pre>` +
		`<figure><img src="a.png"><figcaption>A chart</figcaption></figure><ul><li>An item</li><li>Done!</li></ul>` +
		`<table><tr><td>1</td></tr></table><p>The end</p>`)
	want := "A heading.\nFirst sentence. Second “one.”\nAn item.\nDone!\nThe end."
	if got != want {
		t.Errorf("Speech:\n got %q\nwant %q", got, want)
	}
}

func TestPieces(t *testing.T) {
	const article = `<h2>A title</h2>
<p>First <a href="/x">linked</a> paragraph.</p>
<figure><img src="/assets/abc" alt="x"><figcaption>The caption</figcaption></figure>
<pre><code>do not touch()</code></pre>
<ul><li>One <ul><li>Nested</li></ul></li><li><code>only_code</code></li></ul>
<div>Loose text<br>second line</div>
<blockquote><p>Quoted.</p></blockquote>`
	got := Pieces(article)
	want := []string{"A title", `First <a href="/x">linked</a> paragraph.`, "The caption", "One", "Nested",
		"Loose text<br/>second line", "Quoted."}
	if len(got) != len(want) {
		t.Fatalf("pieces = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("piece %d = %q, want %q", i, got[i], want[i])
		}
	}

	out, ok := ReplacePieces(article, []string{"Tytuł", `Pierwszy <a href="/x">podlinkowany</a> akapit.`, "Podpis",
		"Jeden", "Zagnieżdżony", "", "Cytat."})
	if !ok {
		t.Fatal("not replaced")
	}
	for _, s := range []string{`<h2>Tytuł</h2>`, `<p>Pierwszy <a href="/x">podlinkowany</a> akapit.</p>`,
		`<img src="/assets/abc" alt="x"/><figcaption>Podpis</figcaption>`, `<pre><code>do not touch()</code></pre>`,
		`<li>Jeden<ul><li>Zagnieżdżony</li></ul></li>`, `<li><code>only_code</code></li>`,
		`<div>Loose text<br/>second line</div>`, `<blockquote><p>Cytat.</p></blockquote>`} {
		if !strings.Contains(out, s) {
			t.Errorf("translated article lacks %s:\n%s", s, out)
		}
	}
	if _, ok := ReplacePieces(article, []string{"too", "few"}); ok {
		t.Error("accepted the wrong number of pieces")
	}
}

func TestCleanCopy(t *testing.T) {
	const article = `<div class="byline"><span>By <a href="/a">Ann Author</a></span></div>
<p><span style="color:red">Ann is a <b>staff writer</b> covering harbours.</span></p>
<h1>The <em>lamp</em> stays</h1>
<p>First <a href="https://example.com/x" class="link" data-x="1">linked</a> <font>paragraph</font>, with <i>stress</i>.</p>
<p><strong>What the board said</strong></p>
<p>&ldquo;We like the old lamp,&rdquo; the chair said.</p>
<figure><img src="/assets/abc" alt="The lamp" width="800"><figcaption>Photo: <a href="/p">Pat</a></figcaption></figure>
<ul><li>One<ul><li>Nested</li></ul></li><li>Two</li></ul>
<blockquote><p>Quoted line.</p></blockquote>
<pre><code>lamp := "old"
keep(lamp)</code></pre>
<p><a href="/subscribe">Subscribe to our newsletter</a></p>
<hr>
<p>Related: <a href="/other">Another story</a></p>`
	blocks := Blocks(article)
	var labels []string
	for _, b := range blocks {
		labels = append(labels, b.Label())
	}
	want := "text text h2 text text text image item item item quote code text rule text"
	if got := strings.Join(labels, " "); got != want {
		t.Fatalf("blocks = %s\nwant     %s", got, want)
	}
	if got := blocks[3].HTML; got != `First <a href="https://example.com/x">linked</a> paragraph, with <em>stress</em>.` {
		t.Errorf("inline marks: %s", got)
	}
	if got := blocks[6].Text(); got != "The lamp Photo: Pat" {
		t.Errorf("image as text: %q", got)
	}

	// What a reader of the blocks would say: the byline, the biography, the plea to
	// subscribe and the related link go; a bold line is a heading; a paragraph is a quotation.
	roles := []string{"drop", "drop", "drop", "", "h2", "quote", "keep", "", "", "", "", "", "drop", "", "drop"}
	html, ok := Assemble(blocks, roles)
	if !ok {
		t.Fatal("not assembled")
	}
	for _, s := range []string{
		`<p>First <a href="https://example.com/x">linked</a> paragraph, with <em>stress</em>.</p><h2>What the board said</h2>`,
		`<blockquote><p>“We like the old lamp,” the chair said.</p></blockquote>`,
		`<p><img src="/assets/abc" alt="The lamp"></p><p><em>Photo: <a href="/p">Pat</a></em></p>`,
		`<ul><li>One<ul><li>Nested</li></ul></li><li>Two</li></ul>`,
		`<blockquote><p>Quoted line.</p></blockquote><pre><code>lamp := &#34;old&#34;` + "\n" + `keep(lamp)</code></pre>`,
	} {
		if !strings.Contains(html, s) {
			t.Errorf("clean HTML lacks %s:\n%s", s, html)
		}
	}
	for _, s := range []string{"Ann", "Subscribe", "Related", "<hr>", "staff writer"} {
		if strings.Contains(html, s) {
			t.Errorf("clean HTML still has %q", s)
		}
	}

	// It is kept as Markdown, and shown from that.
	md, err := Markdown(html)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"First [linked](https://example.com/x) paragraph, with *stress*.", "## What the board said",
		"> “We like the old lamp,” the chair said.", "![The lamp](/assets/abc)", "- One\n", "  - Nested\n- Two", "```\nlamp := \"old\"\nkeep(lamp)\n```"} {
		if !strings.Contains(md, s) {
			t.Errorf("Markdown lacks %q:\n%s", s, md)
		}
	}
	back, err := FromMarkdown(md)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`<h2>What the board said</h2>`, `<a href="https://example.com/x" rel="nofollow">linked</a>`,
		`<img src="/assets/abc" alt="The lamp"`, "<blockquote>", "<li>Nested</li>", "keep(lamp)"} {
		if !strings.Contains(back, s) {
			t.Errorf("HTML from the Markdown lacks %s:\n%s", s, back)
		}
	}

	if _, ok := Assemble(blocks, roles[:3]); ok {
		t.Error("accepted too few roles")
	}
	if _, ok := Assemble(blocks[:1], []string{"h9"}); ok {
		t.Error("accepted an unknown role")
	}
}
