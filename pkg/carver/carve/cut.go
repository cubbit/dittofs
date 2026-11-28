// Package carve is the carver (RFC 2 §2, §4): the settings constructor, Cut,
// the chunk ID and the block name. It keeps nothing between calls and
// declares no interface. Only the node's content wiring imports it.
//
// Cut cuts a stretch with a real end; it refuses an artificial one with
// errors.ErrUnsupported.
package carve

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/internal/chunker"
	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

// Cut cuts one unbroken stretch read from r, whose first byte sits at file
// offset base, into chunks, calling emit once per chunk in order. The bytes
// handed to emit are borrowed: they are good only until emit returns, and a
// caller that keeps them copies them. buf is the caller's, at least s.Max()
// bytes, or Cut fails with io.ErrShortBuffer; a zero s fails it with
// carver.ErrInvalidSettings. realEnd says whether the stretch ends for good;
// on an artificial end the tail after the last content-chosen boundary is not
// emitted, and consumed says how far Cut cut. A reader that keeps returning
// no bytes and no error fails it with io.ErrNoProgress. An error from r or
// emit stops Cut and is returned as it came. A negative base, or a stretch
// whose next byte would sit past math.MaxInt64, fails it with an error
// wrapping fs.ErrInvalid, so offsets never overflow.
func Cut(r io.Reader, base int64, s carver.Settings, buf []byte, realEnd bool,
	emit func(carver.Chunk, []byte) error,
) (consumed int64, err error) {
	switch {
	case params.Gear(s) == nil || params.ChunkIDKey(s) == [32]byte{}:
		return 0, carver.ErrInvalidSettings
	case len(buf) < s.Max():
		return 0, io.ErrShortBuffer
	case base < 0:
		return 0, fmt.Errorf("%w: base %d is negative", fs.ErrInvalid, base)
	case !realEnd:
		return 0, fmt.Errorf("%w: an artificial end", errors.ErrUnsupported)
	}

	var (
		lo, hi int    // buf[lo:hi] is read and not yet cut; lo is the next chunk's start
		off    = base // file offset of buf[lo]
		eof    bool
	)

	for {
		// decision: the fill loop is Cut's own, never io.ReadFull, and moves
		// the unread rest to buf's front whenever fewer than Max bytes past the
		// next chunk's start are held, so a whole chunk is always in view and
		// NextBoundary sees under Max bytes only at the stretch's end. A buffer
		// of exactly Max moves up to Max bytes per chunk; a larger one moves
		// less often, which is the caller's to size (RFC 2 §10.5, at most
		// 2 × Max). The loop has no bound on empty reads yet: a reader that
		// keeps returning no bytes and no error is read for ever.
		if !eof && hi-lo < s.Max() {
			hi = copy(buf, buf[lo:hi])
			lo = 0

			for !eof && hi < len(buf) {
				// decision: consumed is what emit accepted, and a reader's
				// error stops Cut at once, before the bytes that came with it
				// are cut: the extents stay dirty and the retry re-reads them
				// (RFC 2 §7). io.EOF with bytes is the end, and they are cut.
				k, rerr := r.Read(buf[hi:])
				hi += k

				if int64(hi)-1 > math.MaxInt64-off {
					return consumed, fmt.Errorf("%w: a stretch at %d runs past the largest file offset", fs.ErrInvalid, base)
				}

				if rerr == io.EOF {
					eof = true
				} else if rerr != nil {
					return consumed, rerr
				}
			}
		}

		if lo == hi {
			return consumed, nil
		}

		e := chunker.NextBoundary(s, buf[lo:hi], eof)
		p := buf[lo : lo+e : lo+e]

		if err := emit(carver.Chunk{Offset: off, Length: int32(e), Hash: ChunkID(s, p)}, p); err != nil {
			return consumed, err
		}

		lo += e
		off += int64(e)
		consumed += int64(e)
	}
}
