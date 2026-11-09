package durability

import (
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// roState is one immutable snapshot of the read-only state. A change builds a
// new value and swaps it in, so a reader takes no lock a write needs.
type roState struct {
	core.ReadOnlyState

	// refusal is what Admit returns: core.ErrJournalReadOnly wrapping Err. It
	// is built once, on entry, so a refused write allocates nothing.
	refusal error
}

// Enter makes the journal read-only because of cause. It is idempotent: the
// first cause is kept and a later call changes nothing. It takes no lock and
// is safe from any goroutine.
func (d *Durability) Enter(cause error) {
	for {
		cur := d.ro.Load()
		if cur != nil && cur.ReadOnly {
			return
		}
		next := &roState{refusal: fmt.Errorf("%w: %w", core.ErrJournalReadOnly, cause)}
		if cur != nil {
			next.ReadOnlyState = cur.ReadOnlyState
		}
		next.ReadOnly, next.Err = true, cause
		if d.ro.CompareAndSwap(cur, next) {
			return
		}
	}
}

// Admit reports whether the journal takes a write: nil, or an error matching
// core.ErrJournalReadOnly that also wraps the cause the journal went
// read-only with.
func (d *Durability) Admit() error {
	if st := d.ro.Load(); st != nil && st.ReadOnly {
		return st.refusal
	}
	return nil
}

// ReadOnly is Admit as the func() error the pool, the idle seal and the
// retirer consult before they write.
func (d *Durability) ReadOnly() error { return d.Admit() }

// ReadOnlyState returns the read-only state. It reads one atomic pointer and
// never waits on a write path.
func (d *Durability) ReadOnlyState() core.ReadOnlyState {
	if st := d.ro.Load(); st != nil {
		return st.ReadOnlyState
	}
	return core.ReadOnlyState{}
}

// update swaps in a copy of the state that edit has changed, retrying when a
// concurrent change won the swap.
func (d *Durability) update(edit func(*roState)) {
	for {
		cur := d.ro.Load()
		next := &roState{}
		if cur != nil {
			*next = *cur
		}
		edit(next)
		if d.ro.CompareAndSwap(cur, next) {
			return
		}
	}
}

// setSpareMade records that a spare segment was made after the journal went
// read-only.
func (d *Durability) setSpareMade() { d.update(func(s *roState) { s.SpareMade = true }) }

// setCheck records the check a probe found failing, or clears it with nil.
func (d *Durability) setCheck(err error) { d.update(func(s *roState) { s.Check = err }) }
