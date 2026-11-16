package main

import (
	"io"
	"os"
)

// colour is the set of highlights a screen line can carry. Every view
// renders through the same structure so the plain path and the colour path
// differ only in these functions, never in the rendering itself.
type colour struct {
	green  func(string) string // a new row
	yellow func(string) string // a changed row
	red    func(string) string // a removed row
	dim    func(string) string // context: unchanged rows, captions

	// on answers whether this is the real-terminal path at all; the plain
	// path shares neither bytes nor habits with the coloured one, so a
	// caller with no row in hand can still ask.
	on bool
}

// noColours is the plain path: identity functions, zero escape bytes.
func noColours() colour {
	id := func(s string) string { return s }
	return colour{green: id, yellow: id, red: id, dim: id}
}

// coloursFor picks the path for a screen: colour only on a real terminal
// that NO_COLOR does not decline. The gate is the writer, not the caller:
// a redirected or piped screen can never see escape bytes.
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
	return colour{
		on: true,
		// The bright palette: the classic 30s render dark on modern
		// terminal themes, and a dim room projects them darker still.
		// Narration takes bright black — reads as calm grey where true
		// dim borders invisible on half the themes.
		green:  func(s string) string { return "\x1b[92m" + s + "\x1b[0m" },
		yellow: func(s string) string { return "\x1b[93m" + s + "\x1b[0m" },
		red:    func(s string) string { return "\x1b[91m" + s + "\x1b[0m" },
		dim:    func(s string) string { return "\x1b[90m" + s + "\x1b[0m" },
	}
}
