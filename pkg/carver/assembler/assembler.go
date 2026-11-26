// Package assembler is the block assembler (RFC 2 §5): the fold that packs one
// pass's chunks into block plans. It does no I/O, keeps nothing between passes
// and declares no interface. Only the node's content wiring imports it.
//
// Every body is a stub behind its final signature: a function with an error
// returns errors.ErrUnsupported, one without returns its zero value.
package assembler

import (
	"errors"
	"time"

	"github.com/cubbit/dittofs/pkg/carver"
)

// Fold packs one pass's chunks into block plans (RFC 2 §5). One Fold serves
// one pass and does no I/O; the plans it returns inherit the pass's namespace
// and chunk-ID key, which the pass, not the plan, records.
type Fold struct{}

// New starts a pass's fold. It refuses a BlockTarget outside
// carver.MinBlockTarget to carver.MaxBlockTarget with
// carver.ErrInvalidSettings, never clamping it.
func New(c carver.AssemblerConfig) (*Fold, error) {
	return nil, errors.ErrUnsupported
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
