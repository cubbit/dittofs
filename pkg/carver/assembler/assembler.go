// Package assembler is the block assembler (RFC 2 §5): the fold that packs one
// pass's chunks into block plans. It does no I/O, keeps nothing between passes
// and declares no interface. Only the node's content wiring imports it.
package assembler

import (
	"fmt"
	"time"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// decision: a block holds at most maxChunks carried chunks because that is the
// count cap N of block format version one (RFC 2 P4), which bounds the block
// header the remote tier's codec writes. It changes only with a block format
// version that raises it, beside that codec.
const maxChunks = 1024

// Fold packs one pass's chunks into block plans (RFC 2 §5). One Fold serves
// one pass and does no I/O; the plans it returns inherit the pass's namespace
// and chunk-ID key, which the pass, not the plan, records.
type Fold struct {
	cfg      carver.AssemblerConfig
	open     *carver.BlockPlan             // the pending plan, nil until a chunk opens one
	carried  int64                         // the open plan's carried bytes
	hashes   map[entity.ChunkHash]struct{} // the hashes the open plan carries
	finished bool
}

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
	return &Fold{cfg: c, hashes: map[entity.ChunkHash]struct{}{}}, nil
}

// Add folds in the next chunk, in file order, and returns the plans it closed
// and the chunk's disposition, which counts only once the plan holding it is
// committed. An all-zero chunk is a ZeroRef: it joins the open plan's Refs,
// opening a plan if none is open, and is carried by none. A chunk the open plan
// already carries is referenced again and carried once (RFC 2 P6). Neither adds
// carried bytes or a slot of MaxChunks.
//
// decision: Add takes no dedup lookup and returns no error, and Carried and
// ZeroRef are the only dispositions (RFC 2 §5.1): with no lookup nothing can
// fail and nothing is adopted. The lookup, the error and an adopted
// disposition arrive together with the dedup oracle, which changes this
// signature.
//
// Add panics after Finish: a fold serves one pass.
func (f *Fold) Add(c carver.ChunkInfo) ([]carver.BlockPlan, carver.Disposition) {
	if f.finished {
		panic("assembler: Add after Finish: a fold serves one pass")
	}
	var closed []carver.BlockPlan
	// decision: a full plan is closed only when the next carried chunk
	// arrives, never when it fills, so the zero refs after it join its commit:
	// RFC 2 P2's "nearest block" for zero refs between two blocks is the
	// earlier one, and the open plan is always the nearest, so no plan is
	// reopened and Add closes at most one. The next carried chunk closes it
	// even when it repeats one of the plan's chunks, so a repeat past the
	// block's end is carried again. The choice moves only which commit a zero
	// ref rides in, never a block's bytes, so it can change with no migration.
	if !c.Zero && f.open != nil && f.full() {
		closed = []carver.BlockPlan{*f.open}
		f.open, f.carried = nil, 0
		clear(f.hashes)
	}
	if f.open == nil {
		f.open = &carver.BlockPlan{}
	}
	f.open.Refs = append(f.open.Refs, c)
	if c.Zero {
		return closed, carver.ZeroRef
	}
	if _, repeat := f.hashes[c.Hash]; !repeat {
		f.hashes[c.Hash] = struct{}{}
		f.open.Carried = append(f.open.Carried, c)
		f.carried += int64(c.Length)
	}
	return closed, carver.Carried
}

// Finish closes the pass at now, which the caller supplies, and returns its
// last plan, nil if none is open. held is true when that plan is short and held
// back: the caller puts none of it, reports none of its extents and leaves them
// dirty for the next pass. A short plan is put once its oldest ref, zero refs
// counted, is at least MaxAge old at now; an Oldest after now reads as young.
// A plan with no carried chunk, the zero-ref commit, is never held. After
// Finish, Add and Finish panic.
func (f *Fold) Finish(now time.Time) (last *carver.BlockPlan, held bool) {
	if f.finished {
		panic("assembler: Finish after Finish: a fold serves one pass")
	}
	f.finished = true
	// decision: a plan with no carried chunk is never held, however young:
	// holding back exists to spare a short put (RFC 2 §5.4) and this plan has
	// no block to put, and the pass reports its all-zero extents in the same
	// pass (RFC 8 §6.3). Withdraw it if a zero-ref commit ever gains a put.
	if f.open == nil || len(f.open.Carried) == 0 || f.full() {
		return f.open, false
	}
	return f.open, now.Sub(oldest(f.open.Refs)) < f.cfg.MaxAge
}

// full reports whether the open plan has reached the block target in carried
// bytes or holds the count cap's chunks.
func (f *Fold) full() bool {
	return f.carried >= f.cfg.BlockTarget || len(f.open.Carried) >= f.cfg.MaxChunks
}

// oldest is the earliest Oldest among refs; a zero Oldest is older than any
// time, so its plan is always old enough to put.
func oldest(refs []carver.ChunkInfo) time.Time {
	o := refs[0].Oldest
	for _, r := range refs[1:] {
		if r.Oldest.Before(o) {
			o = r.Oldest
		}
	}
	return o
}
