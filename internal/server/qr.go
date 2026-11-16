package server

import (
	"fmt"
	"html/template"
	"strings"

	"rsc.io/qr"
)

// qrSVG draws a QR code for a link as an inline SVG, for the printed article:
// paper has no link to tap, and the page may load nothing from another host.
// It returns nothing for a link too long to encode.
func qrSVG(link string) template.HTML {
	code, err := qr.Encode(link, qr.M)
	if err != nil {
		return ""
	}
	const quiet = 4 // the blank margin a scanner needs, in modules
	var path strings.Builder
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if !code.Black(x, y) {
				continue
			}
			run := 1 // a row of dark modules is one rectangle
			for x+run < code.Size && code.Black(x+run, y) {
				run++
			}
			fmt.Fprintf(&path, "M%d %dh%dv1h-%dz", x+quiet, y+quiet, run, run)
			x += run - 1
		}
	}
	side := code.Size + 2*quiet
	return template.HTML(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %[1]d %[1]d" shape-rendering="crispEdges" role="img" aria-label="QR code of the original's address">`+
			`<rect width="%[1]d" height="%[1]d" fill="#fff"/><path fill="#000" d="%[2]s"/></svg>`, side, path.String()))
}
