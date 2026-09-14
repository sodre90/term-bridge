package tmuxhost

import (
	"fmt"
	"strconv"
	"strings"
)

// style is one cell's attributes as the app's Style reads them; colours are
// "#rrggbb" or "" for the terminal default. The zero value is the default
// style, which the grid always lists as id 0 with no colours, so the app
// paints its own canvas behind unstyled cells.
type style struct {
	fg, bg                                          string
	bold, faint, italic, underline, inverse, strike bool
}

// applySGR advances s by one SGR sequence's parameters. Parameters arrive
// as tmux writes them, ';'-separated with the occasional ':' sub-parameter
// form for 38/48/58 and underline styles.
func applySGR(s style, params string) style {
	if params == "" {
		return style{}
	}
	p := splitParams(params)
	for i := 0; i < len(p); i++ {
		switch n := p[i].n; {
		case n == 0:
			s = style{}
		case n == 1:
			s.bold = true
		case n == 2:
			s.faint = true
		case n == 3:
			s.italic = true
		case n == 4, n == 21:
			// 4:0 is "underline off" in the sub-parameter form.
			s.underline = !(p[i].sub && p[i].subValue == 0)
		case n == 7:
			s.inverse = true
		case n == 9:
			s.strike = true
		case n == 22:
			s.bold, s.faint = false, false
		case n == 23:
			s.italic = false
		case n == 24:
			s.underline = false
		case n == 27:
			s.inverse = false
		case n == 29:
			s.strike = false
		case n >= 30 && n <= 37:
			s.fg = palette[n-30]
		case n == 38, n == 48, n == 58:
			colour, used := extendedColour(p[i:])
			i += used
			switch n {
			case 38:
				s.fg = colour
			case 48:
				s.bg = colour
			}
		case n == 39:
			s.fg = ""
		case n >= 40 && n <= 47:
			s.bg = palette[n-40]
		case n == 49:
			s.bg = ""
		case n >= 90 && n <= 97:
			s.fg = palette[n-90+8]
		case n >= 100 && n <= 107:
			s.bg = palette[n-100+8]
		}
	}
	return s
}

type sgrParam struct {
	n        int
	sub      bool // came with a ':' sub-parameter, e.g. 4:3
	subValue int
	subs     []int // every sub-parameter after n, for 38:2:… forms
}

func splitParams(params string) []sgrParam {
	var out []sgrParam
	for _, field := range strings.Split(params, ";") {
		parts := strings.Split(field, ":")
		n, _ := strconv.Atoi(parts[0])
		p := sgrParam{n: n}
		for _, sp := range parts[1:] {
			v, _ := strconv.Atoi(sp)
			p.subs = append(p.subs, v)
		}
		if len(p.subs) > 0 {
			p.sub, p.subValue = true, p.subs[0]
		}
		out = append(out, p)
	}
	return out
}

// extendedColour reads a 38/48/58 colour spec starting at p[0] and returns
// the colour plus how many further ';'-parameters it consumed. Both
// spellings are accepted: "38;5;n" / "38;2;r;g;b" and "38:5:n" /
// "38:2::r:g:b" (the latter with an optional colour-space id).
func extendedColour(p []sgrParam) (string, int) {
	if p[0].sub {
		subs := p[0].subs
		switch {
		case len(subs) >= 2 && subs[0] == 5:
			return palette256(subs[1]), 0
		case len(subs) >= 4 && subs[0] == 2:
			rgb := subs[len(subs)-3:]
			return rgbHex(rgb[0], rgb[1], rgb[2]), 0
		}
		return "", 0
	}
	switch {
	case len(p) >= 3 && p[1].n == 5:
		return palette256(p[2].n), 2
	case len(p) >= 5 && p[1].n == 2:
		return rgbHex(p[2].n, p[3].n, p[4].n), 4
	}
	return "", 0
}

func rgbHex(r, g, b int) string {
	return fmt.Sprintf("#%02x%02x%02x", clamp8(r), clamp8(g), clamp8(b))
}

func clamp8(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

// palette is the 16 ANSI colours, in Catppuccin Mocha so they sit on the
// app's canvas (which is that theme's base) the way a themed terminal would
// show them; tmux hands over indices, not colours, so any palette is a
// choice the bridge has to make.
var palette = [16]string{
	"#45475a", "#f38ba8", "#a6e3a1", "#f9e2af", "#89b4fa", "#f5c2e7", "#94e2d5", "#bac2de",
	"#585b70", "#f38ba8", "#a6e3a1", "#f9e2af", "#89b4fa", "#f5c2e7", "#94e2d5", "#a6adc8",
}

// palette256 is xterm's 256-colour table: the 16 above, a 6x6x6 cube, then
// a 24-step grey ramp.
func palette256(n int) string {
	switch {
	case n < 0:
		return ""
	case n < 16:
		return palette[n]
	case n < 232:
		n -= 16
		steps := [6]int{0, 95, 135, 175, 215, 255}
		return rgbHex(steps[n/36], steps[n/6%6], steps[n%6])
	case n < 256:
		v := 8 + (n-232)*10
		return rgbHex(v, v, v)
	}
	return ""
}
