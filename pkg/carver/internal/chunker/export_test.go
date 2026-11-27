package chunker

import (
	"fmt"
	"io"

	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

// Ends cuts r under s as the carver's Cut will, streaming it through a window
// of buf bytes, at least s.Max(): it asks NextBoundary for each boundary from
// the last, with final set once r is done, and returns every chunk's end as an
// offset into r. It never holds more of r than the window.
func Ends(s params.Settings, r io.Reader, buf int) []int {
	if buf < s.Max() {
		panic("Ends: a window under Max")
	}

	var (
		ends []int
		win  = make([]byte, buf)
		n    int // bytes held in win
		off  int // offset in r of win[0]
		eof  bool
	)

	for {
		for !eof && n < buf {
			k, err := r.Read(win[n:])
			n += k

			if err == io.EOF {
				eof = true
			} else if err != nil {
				panic(err)
			}
		}

		if n == 0 {
			return ends
		}

		e := NextBoundary(s, win[:n], eof)
		if e <= 0 || e > n {
			panic(fmt.Sprintf("Ends: NextBoundary returned %d over %d bytes held, final %v", e, n, eof))
		}

		off += e
		ends = append(ends, off)
		n = copy(win, win[e:n])
	}
}
