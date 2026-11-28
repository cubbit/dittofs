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
// an artificial end is refused with errors.ErrUnsupported. consumed is how
// far, from base, the chunks emit accepted reach. An error from r or emit
// stops Cut and is returned as it came. A negative base, or a stretch whose
// next byte would sit past math.MaxInt64, fails it with an error wrapping
// fs.ErrInvalid, so offsets never overflow.
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
		lo, hi int // buf[lo:hi] is read and not yet cut; lo is the next chunk's start
		eof    bool
	)

	for {
		// decision: the fill loop is Cut's own, never io.ReadFull, which keeps
		// calling a reader that returns no bytes and no error, so Cut can count
		// such reads. It moves the unread rest to buf's front whenever fewer
		// than Max bytes past the next chunk's start are held, so a whole chunk
		// is always in view and NextBoundary sees under Max bytes only at the
		// stretch's end. A buffer of exactly Max moves up to Max bytes per
		// chunk; a larger one moves less often, which is the caller's to size
		// (RFC 2 §10.5, at most 2 × Max). The loop has no bound on empty reads
		// yet: a reader that keeps returning no bytes and no error is read for
		// ever.
		if !eof && hi-lo < s.Max() {
			hi = copy(buf, buf[lo:hi])
			lo = 0

			for !eof && hi < len(buf) {
				// decision: a reader's error stops Cut at once, before the
				// bytes that came with it are cut: the extents stay dirty and
				// the retry re-reads them (RFC 2 §7). io.EOF with bytes is the
				// end, and they are cut. Cut the bytes first if a caller ever
				// keeps the chunks of a failed pass rather than re-reading it.
				k, rerr := r.Read(buf[hi:])
				if k < 0 || k > len(buf)-hi { // the io.Reader contract rules both out
					return consumed, fmt.Errorf("carve: reader returned %d bytes into a %d-byte slice", k, len(buf)-hi)
				}
				hi += k

				if int64(hi) > math.MaxInt64-(base+consumed) {
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

		if err := emit(carver.Chunk{Offset: base + consumed, Length: int32(e), Hash: ChunkID(s, p)}, p); err != nil {
			return consumed, err
		}

		lo += e
		consumed += int64(e)
	}
}
