package main

import (
	"io"
	"os"
	"strings"
)

// colour is the set of highlights a screen line can carry. Every view
// renders through the same structure so the plain path and the colour
// paths differ only in these functions, never in the rendering itself.
type colour struct {
	green  func(string) string // a new row
	yellow func(string) string // a changed row
	red    func(string) string // a removed row
	dim    func(string) string // context: unchanged rows, captions, narration
	code   func(string) string // source calls: the journal's API, set apart from prose

	// on answers whether this is a real-terminal path at all; the plain
	// path shares neither bytes nor habits with the coloured ones, so a
	// caller with no row in hand can still ask.
	on bool
}

// noColours is the plain path: identity functions, zero escape bytes.
func noColours() colour {
	id := func(s string) string { return s }
	return colour{green: id, yellow: id, red: id, dim: id, code: id}
}

// The palette, fixed RGB on a dark room's screen: GitHub's dark-theme hues,
// which hold contrast at a distance. The terminal's own theme has no say on
// this path — a theme remapping ANSI-16 was what muted the first attempts.
const (
	rgbGreen  = "\x1b[38;2;63;185;80m"   // #3fb950
	rgbYellow = "\x1b[38;2;227;179;65m"  // #e3b341
	rgbRed    = "\x1b[38;2;248;81;73m"   // #f85149
	rgbGrey   = "\x1b[38;2;139;148;158m" // #8b949e
	rgbCode   = "\x1b[38;2;121;192;255m" // #79c0ff
	reset     = "\x1b[0m"
)

// The 256-colour middle tier, for terminals without truecolor: fixed
// indices chosen against the same palette.
const (
	c256Green  = "\x1b[38;5;114m"
	c256Yellow = "\x1b[38;5;221m"
	c256Red    = "\x1b[38;5;203m"
	c256Grey   = "\x1b[38;5;245m"
	c256Code   = "\x1b[38;5;117m"
)

// The ANSI-16 fallback, last resort: bright variants, because a themed
// terminal maps the 16 at will and the brights at least start vivid.
const (
	a16Green  = "\x1b[92m"
	a16Yellow = "\x1b[93m"
	a16Red    = "\x1b[91m"
	a16Grey   = "\x1b[90m"
	a16Code   = "\x1b[96m"
)

// coloursFor picks the path for a screen: colour only on a real terminal
// that NO_COLOR does not decline, and the strongest palette the terminal
// admits — truecolor when COLORTERM claims it, 256-colour next, the
// bright sixteens last. The gate is the writer, not the caller: a
// redirected or piped screen can never see escape bytes.
func coloursFor(w io.Writer) colour {
	f, ok := w.(*os.File)
	if !ok {
		return noColours()
	}
	if os.Getenv("NO_COLOR") != "" {
		return noColours()
	}
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return noColours()
	}
	mk := func(g, y, r, d, c string) colour {
		return colour{
			on:     true,
			green:  func(s string) string { return g + s + reset },
			yellow: func(s string) string { return y + s + reset },
			red:    func(s string) string { return r + s + reset },
			dim:    func(s string) string { return d + s + reset },
			code:   func(s string) string { return c + s + reset },
		}
	}
	colorterm := os.Getenv("COLORTERM")
	term := os.Getenv("TERM")
	switch {
	case strings.Contains(colorterm, "truecolor") || strings.Contains(colorterm, "24bit"):
		return mk(rgbGreen, rgbYellow, rgbRed, rgbGrey, rgbCode)
	case strings.Contains(term, "256color"):
		return mk(c256Green, c256Yellow, c256Red, c256Grey, c256Code)
	default:
		return mk(a16Green, a16Yellow, a16Red, a16Grey, a16Code)
	}
}
