// Package assembler is the block assembler (RFC 2 §5): the fold that packs one
// pass's chunks into block plans. It does no I/O, keeps nothing between passes
// and declares no interface. Only the node's content wiring imports it.
//
// Add and Finish are stubs behind their final signatures, returning their zero
// values.
package assembler

import (
	"fmt"
	"time"

	"github.com/cubbit/dittofs/pkg/carver"
)

// decision: a block holds at most maxChunks carried chunks because that is the
// count cap N of block format version one (RFC 2 P4), which bounds the block
// header the remote tier's codec writes. It changes only with a block format
// version that raises it, beside that codec.
const maxChunks = 1024

// Fold packs one pass's chunks into block plans (RFC 2 §5). One Fold serves
// one pass and does no I/O; the plans it returns inherit the pass's namespace
// and chunk-ID key, which the pass, not the plan, records.
type Fold struct{}

// New starts a pass's fold. It refuses, with carver.ErrInvalidSettings naming
// the field, a BlockTarget outside carver.MinBlockTarget to
// carver.MaxBlockTarget, a MaxChunks under 1 or above the block format's count
// cap, a KeyBudget under 1, a negative BlockKeys, FileKeys or MaxAge. A refused
// setting is never clamped or replaced by a default, and a refused New returns
// a nil Fold.
//
// decision: the BlockTarget range's upper bound is derived from P4 and the
// default chunking Target: 64 MiB is 1,024 chunks of its 64 KiB Min. The 4 MiB
// default and the 1 MiB floor are proposals, not measurements: the block-size
// benchmark (RFC 4 Appendix B) confirms the default per service, and lowers the
// floor if it finds a supported service whose put cost per byte is flat below
// 1 MiB.
//
// decision: New does not refuse a KeyBudget too small to hold the per-block
// and per-file keys and one chunk's: that is RFC 6 §5.2's floor on K, which is
// derived from the backend, never configured, and is the caller's to check
// at start, before it builds any fold.
func New(c carver.AssemblerConfig) (*Fold, error) {
	switch {
	case c.BlockTarget < carver.MinBlockTarget || c.BlockTarget > carver.MaxBlockTarget:
		return nil, fmt.Errorf("%w: BlockTarget %d is outside %d to %d bytes", carver.ErrInvalidSettings, c.BlockTarget, carver.MinBlockTarget, carver.MaxBlockTarget)
	case c.MaxChunks < 1 || c.MaxChunks > maxChunks:
		return nil, fmt.Errorf("%w: MaxChunks %d is outside 1 to %d", carver.ErrInvalidSettings, c.MaxChunks, maxChunks)
	case c.KeyBudget < 1:
		return nil, fmt.Errorf("%w: KeyBudget %d is under 1", carver.ErrInvalidSettings, c.KeyBudget)
	case c.BlockKeys < 0:
		return nil, fmt.Errorf("%w: BlockKeys %d is negative", carver.ErrInvalidSettings, c.BlockKeys)
	case c.FileKeys < 0:
		return nil, fmt.Errorf("%w: FileKeys %d is negative", carver.ErrInvalidSettings, c.FileKeys)
	case c.MaxAge < 0:
		return nil, fmt.Errorf("%w: MaxAge %v is negative", carver.ErrInvalidSettings, c.MaxAge)
	}
	return &Fold{}, nil
}

// Add folds in the next chunk, in file order, and returns the plans it closed
// and the chunk's disposition, which counts only once the plan holding it is
// committed.
//
// decision: Add takes no dedup lookup and returns no error, and Carried and
// ZeroRef are the only dispositions (RFC 2 §5.1): with no lookup nothing can
// fail and nothing is adopted. The lookup, the error and an adopted
// disposition arrive together with the dedup oracle, which changes this
// signature.
func (f *Fold) Add(c carver.ChunkInfo) ([]carver.BlockPlan, carver.Disposition) {
	return nil, 0
}

// Finish closes the pass at now, which the caller supplies, and returns its
// last plan, nil if none is open. held is true when that plan is short and held
// back: the caller puts none of it, reports none of its extents and leaves them
// dirty for the next pass.
func (f *Fold) Finish(now time.Time) (last *carver.BlockPlan, held bool) {
	return nil, false
}
