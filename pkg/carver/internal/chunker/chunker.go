// Package chunker finds where the next chunk of a buffer ends under a
// namespace's settings, by the gear fingerprint RFC 2 §3.2 derives. It answers
// that and nothing else: it never hashes, holds a file offset, reads from
// anything, allocates or keeps anything between calls.
package chunker

import "github.com/cubbit/dittofs/pkg/carver/internal/params"

// window is how many bytes a boundary decision looks back (RFC 2 §3.4): after
// 64 shifts an older byte has left the 64-bit fingerprint.
const window = 64

// NextBoundary returns where the first chunk of buf ends under s: just after
// the first byte from index Min−1 whose fingerprint has its top Bits zero,
// else Max, else, when final is set and buf is shorter than Max, len(buf).
// buf holds at least s.Max() bytes unless final is set; it panics otherwise,
// and on the zero value. It never hashes, reads, allocates or keeps anything.
//
// The fingerprint is warmed over the 64 bytes ending at the first tested
// byte, never from the chunk's start: each decision depends on exactly the 64
// bytes ending at its position, so the two warm-ups give the same boundaries.
//
// decision: a buffer under Max with final unset panics rather than returning
// an error. There is no "not yet" answer (RFC 2 §1.2), and any position
// returned for such a buffer would be wrong, a short chunk or a boundary past
// the buffer. The one caller, the carver's Cut, never passes one, so reaching
// it is a bug in the carver, not in its input. The ceiling: a panic on the
// offload path stops the node, every share with it, where an error would fail
// one pass. The zero settings value panics too: no constructor returns it, and
// its bit counts of 0 would pass every byte and cut one-byte chunks. Make both
// an error return if the chunker gains a second caller, or if the carver's
// buffer-edge cases ever reach it.
func NextBoundary(s params.Settings, buf []byte, final bool) int {
	g := params.Gear(s)
	if g == nil {
		panic("chunker: NextBoundary on the zero settings value")
	}

	lo, target, hi := s.Min(), s.Target(), s.Max()
	if len(buf) < hi && !final {
		panic("chunker: NextBoundary on a buffer under Max that is not final")
	}

	end := min(len(buf), hi)
	if end < lo {
		return end
	}

	before, after := params.Bits(s)
	shiftBefore, shiftAfter := uint(64-before), uint(64-after)

	i := lo - 1
	fp := warm(g, (*[window]byte)(buf[lo-window:lo]))

	for {
		shift := shiftBefore
		if i >= target {
			shift = shiftAfter
		}

		if fp>>shift == 0 {
			return i + 1
		}

		i++
		if i == end {
			return end
		}

		fp = fp<<1 + g[buf[i]]
	}
}

// warm is the fingerprint over the 64 bytes ending at the first tested byte.
func warm(g *[256]uint64, w *[window]byte) uint64 {
	var fp uint64
	for _, b := range w {
		fp = fp<<1 + g[b]
	}

	return fp
}
