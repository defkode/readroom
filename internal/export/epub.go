// Package export turns entries into EPUB and Markdown.
package export

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"image/png"
	"io"
	"regexp"
	"strings"
	"time"

	"golang.org/x/image/webp"
	"golang.org/x/net/html"

	"readroom/internal/htmlx"
	"readroom/internal/store"
)

// AssetLoader fetches a preserved image by its SHA-256.
type AssetLoader func(ctx context.Context, sha string) (*store.Asset, error)

// Image types every EPUB reader must support. WebP is converted to PNG.
var imgExt = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/svg+xml": ".svg",
}

var extMIME = map[string]string{
	".jpg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".svg": "image/svg+xml",
}

// portable returns image bytes in a format e-readers understand.
func portable(a *store.Asset) (data []byte, ext string, ok bool) {
	if a.MIME == "image/webp" {
		img, err := webp.Decode(bytes.NewReader(a.Data))
		if err != nil {
			return nil, "", false
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", false
		}
		return buf.Bytes(), ".png", true
	}
	ext, ok = imgExt[a.MIME]
	return a.Data, ext, ok
}

const epubCSS = `body{font-family:serif;line-height:1.5;margin:0 4%}
h1{font-size:1.5em;line-height:1.2;margin:1em 0 .3em}
.meta{font-size:.85em;color:#555;margin:0 0 1.2em}
.summary{border-left:3px solid #999;margin:0 0 1.5em;padding:.2em 0 .2em 1em;font-style:italic}
img{max-width:100%;height:auto}
pre{white-space:pre-wrap;font-size:.85em}
blockquote{margin:1em 0 1em 1em;padding-left:1em;border-left:2px solid #bbb}`

// EPUB writes an EPUB 3 book with one chapter per entry. Preserved images are
// embedded; images that were never downloaded are dropped, as e-readers are offline.
func EPUB(ctx context.Context, w io.Writer, title string, entries []*store.Entry, load AssetLoader) error {
	zw := zip.NewWriter(w)
	// The mimetype entry must come first and be stored uncompressed.
	mt, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		return err
	}
	io.WriteString(mt, "application/epub+zip")

	add := func(name, body string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, body)
		return err
	}
	if err := add("META-INF/container.xml", `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
<rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`); err != nil {
		return err
	}
	if err := add("OEBPS/style.css", epubCSS); err != nil {
		return err
	}

	images := map[string]string{} // sha -> file name inside OEBPS/img
	var manifest, spine, nav strings.Builder
	for i, e := range entries {
		name := fmt.Sprintf("ch%04d.xhtml", i+1)
		var body strings.Builder
		nodes, err := htmlx.ParseFragment(e.Body())
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if err := writeXHTML(ctx, &body, n, images, load, zw); err != nil {
				return err
			}
		}
		heading := e.Title
		if heading == "" {
			heading = "Untitled"
		}
		meta := e.FeedTitle
		if e.Author != "" {
			meta += " · " + e.Author
		}
		if t, err := time.Parse(store.TimeFormat, e.PublishedAt); err == nil {
			meta += " · " + t.Format("2 Jan 2006")
		}
		var ch strings.Builder
		ch.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml"><head><meta charset="utf-8"/><title>` + xmlEsc(heading) +
			`</title><link rel="stylesheet" type="text/css" href="style.css"/></head><body>` + "\n")
		ch.WriteString(`<h1>` + xmlEsc(heading) + `</h1>` + "\n" + `<p class="meta">` + xmlEsc(meta) + `</p>` + "\n")
		if e.Summary != "" {
			ch.WriteString(`<div class="summary">`)
			for _, para := range strings.Split(e.Summary, "\n") {
				if para = strings.TrimSpace(para); para != "" {
					ch.WriteString(`<p>` + xmlEsc(para) + `</p>`)
				}
			}
			ch.WriteString("</div>\n")
		}
		ch.WriteString(body.String())
		if e.URL != "" {
			ch.WriteString("\n" + `<p class="meta">Source: <a href="` + xmlEsc(e.URL) + `">` + xmlEsc(e.URL) + `</a></p>`)
		}
		ch.WriteString("\n</body></html>")
		if err := add("OEBPS/"+name, ch.String()); err != nil {
			return err
		}
		fmt.Fprintf(&manifest, `<item id="ch%d" href="%s" media-type="application/xhtml+xml"/>`+"\n", i+1, name)
		fmt.Fprintf(&spine, `<itemref idref="ch%d"/>`+"\n", i+1)
		fmt.Fprintf(&nav, `<li><a href="%s">%s</a></li>`+"\n", name, xmlEsc(heading))
	}
	n := 0
	for _, file := range images {
		n++
		fmt.Fprintf(&manifest, `<item id="img%d" href="img/%s" media-type="%s"/>`+"\n", n, file, extMIME[file[strings.LastIndex(file, "."):]])
	}

	if err := add("OEBPS/nav.xhtml", `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops"><head><meta charset="utf-8"/><title>Contents</title></head>
<body><nav epub:type="toc"><h1>Contents</h1><ol>
`+nav.String()+`</ol></nav></body></html>`); err != nil {
		return err
	}
	id := fmt.Sprintf("%x", sha1.Sum([]byte(title+spine.String()+time.Now().String())))
	if err := add("OEBPS/content.opf", `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="uid">
<metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
<dc:identifier id="uid">urn:readroom:`+id+`</dc:identifier>
<dc:title>`+xmlEsc(title)+`</dc:title>
<dc:language>en</dc:language>
<dc:creator>readroom</dc:creator>
<meta property="dcterms:modified">`+time.Now().UTC().Format(store.TimeFormat)+`</meta>
</metadata>
<manifest>
<item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
<item id="css" href="style.css" media-type="text/css"/>
`+manifest.String()+`</manifest>
<spine>
`+spine.String()+`</spine>
</package>`); err != nil {
		return err
	}
	return zw.Close()
}

var (
	voidTags = map[string]bool{"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true,
		"img": true, "input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true}
	dropTags = map[string]bool{"script": true, "style": true, "iframe": true, "object": true, "embed": true,
		"form": true, "input": true, "button": true, "video": true, "audio": true, "source": true, "noscript": true,
		"svg": true, "math": true}
	xmlName   = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]*$`)
	assetPath = regexp.MustCompile(`^/assets/([0-9a-f]{64})$`)
)

// writeXHTML serialises an HTML node as well-formed XHTML, embedding preserved images.
func writeXHTML(ctx context.Context, b *strings.Builder, n *html.Node, images map[string]string, load AssetLoader, zw *zip.Writer) error {
	switch n.Type {
	case html.TextNode:
		b.WriteString(xmlEsc(n.Data))
		return nil
	case html.ElementNode:
	default:
		return nil
	}
	tag := strings.ToLower(n.Data)
	if dropTags[tag] || !xmlName.MatchString(tag) {
		return nil
	}
	var attrs strings.Builder
	seen := map[string]bool{}
	for _, a := range n.Attr {
		key := strings.ToLower(a.Key)
		if a.Namespace != "" || !xmlName.MatchString(key) || seen[key] || strings.HasPrefix(key, "on") ||
			strings.HasPrefix(key, "data-") || key == "srcset" || key == "sizes" || key == "loading" {
			continue
		}
		seen[key] = true
		val := a.Val
		if tag == "img" && key == "src" {
			m := assetPath.FindStringSubmatch(val)
			if m == nil {
				return nil // not stored locally: leave the image out
			}
			file, ok := images[m[1]]
			if !ok {
				asset, err := load(ctx, m[1])
				if err != nil {
					return nil
				}
				data, ext, known := portable(asset)
				if !known {
					return nil
				}
				file = m[1][:16] + ext
				f, err := zw.Create("OEBPS/img/" + file)
				if err != nil {
					return err
				}
				if _, err := f.Write(data); err != nil {
					return err
				}
				images[m[1]] = file
			}
			val = "img/" + file
		}
		attrs.WriteString(` ` + key + `="` + xmlEsc(val) + `"`)
	}
	if tag == "img" {
		if !seen["src"] {
			return nil
		}
		if !seen["alt"] {
			attrs.WriteString(` alt=""`)
		}
	}
	if voidTags[tag] {
		b.WriteString("<" + tag + attrs.String() + "/>")
		return nil
	}
	b.WriteString("<" + tag + attrs.String() + ">")
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if err := writeXHTML(ctx, b, c, images, load, zw); err != nil {
			return err
		}
	}
	b.WriteString("</" + tag + ">")
	return nil
}

// xmlEsc escapes text for XML and drops characters XML 1.0 forbids.
func xmlEsc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '"':
			b.WriteString("&quot;")
		case r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r != 0xFFFE && r != 0xFFFF && r != 0xFFFD):
			b.WriteRune(r)
		}
	}
	return b.String()
}
