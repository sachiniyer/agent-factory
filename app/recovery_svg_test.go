package app

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/ui/theme"
)

// recoverySVG faithfully converts the app's ANSI cell grid into a standalone
// still. Colours are read from SGR output, with generated tokens for defaults.
func recoverySVG(frame, mode string, cols, rows int) string {
	colors := theme.Colors()
	fg, bg := colors["ink"].Light, colors["surface"].Light
	if mode == "dark" {
		fg, bg = colors["ink"].Dark, colors["surface"].Dark
	}
	baseFG, baseBG := fg, bg
	bold := false
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d"><rect width="100%%" height="100%%" fill="%s"/><g font-family="DejaVu Sans Mono, monospace" font-size="14" xml:space="preserve">`, cols*9, rows*20, cols*9, rows*20, bg)
	x, y := 0, 0
	for len(frame) > 0 {
		if strings.HasPrefix(frame, "\x1b[") {
			end := strings.IndexByte(frame, 'm')
			if end < 0 {
				break
			}
			fields := strings.Split(frame[2:end], ";")
			codes := make([]int, len(fields))
			for i, s := range fields {
				codes[i], _ = strconv.Atoi(s)
			}
			for i := 0; i < len(codes); i++ {
				c := codes[i]
				switch {
				case c == 0:
					fg, bg, bold = baseFG, baseBG, false
				case c == 1:
					bold = true
				case c == 22:
					bold = false
				case c == 39:
					fg = baseFG
				case c == 49:
					bg = baseBG
				case c == 38 || c == 48:
					value := ""
					if i+4 < len(codes) && codes[i+1] == 2 {
						value = fmt.Sprintf("#%02x%02x%02x", codes[i+2], codes[i+3], codes[i+4])
						i += 4
					} else if i+2 < len(codes) && codes[i+1] == 5 {
						value = termenv.ConvertToRGB(termenv.ANSI256Color(codes[i+2])).Hex()
						i += 2
					}
					if value != "" {
						if c == 38 {
							fg = value
						} else {
							bg = value
						}
					}
				case c >= 30 && c <= 37:
					fg = termenv.ConvertToRGB(termenv.ANSIColor(c - 30)).Hex()
				case c >= 90 && c <= 97:
					fg = termenv.ConvertToRGB(termenv.ANSIColor(c - 90 + 8)).Hex()
				case c >= 40 && c <= 47:
					bg = termenv.ConvertToRGB(termenv.ANSIColor(c - 40)).Hex()
				}
			}
			frame = frame[end+1:]
			continue
		}
		r, n := utf8.DecodeRuneInString(frame)
		frame = frame[n:]
		if r == '\n' {
			x = 0
			y++
			continue
		}
		if r == '\r' {
			x = 0
			continue
		}
		w := runewidth.RuneWidth(r)
		if x < cols && y < rows {
			fmt.Fprintf(&out, `<rect x="%d" y="%d" width="%d" height="20" fill="%s"/>`, x*9, y*20, w*9, bg)
			if r != ' ' {
				weight := "normal"
				if bold {
					weight = "bold"
				}
				fmt.Fprintf(&out, `<text x="%d" y="%d" fill="%s" font-weight="%s">%s</text>`, x*9, y*20+15, fg, weight, html.EscapeString(string(r)))
			}
		}
		x += w
	}
	out.WriteString("</g></svg>\n")
	return out.String()
}
