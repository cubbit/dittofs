package durability

import (
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// roState is one immutable snapshot of the read-only state. A change builds a
// new value and swaps it in, so a reader takes no lock a write needs.
type roState struct {
	core.ReadOnlyState

	// refusal is what Admit returns: core.ErrJournalReadOnly wrapping Err. It
	// is built once, on entry, so a refused write allocates nothing.
	refusal error

	// raise raises the loss generation once for every record the entry
	// fails, on whichever stream's worker fails one first. It is made on
	// entry and shared by every later snapshot of the state.
	raise *sync.Once

	// woke wakes every Sync and WaitDurable waiter once, on the first worker
	// pass after entry, so a wait on a record failed before the entry, which
	// no pass changes, is answered with the refusal.
	woke *sync.Once
}

// Enter makes the journal read-only because of cause, and wakes every
// stream's worker to fail what is unsynced (failReadOnly). It is idempotent:
// the first cause is kept and a later call changes nothing. It takes no lock
// and is safe from any goroutine.
func (d *Durability) Enter(cause error) {
	for {
		cur := d.ro.Load()
		if cur != nil && cur.ReadOnly {
			return
		}
		next := &roState{refusal: fmt.Errorf("%w: %w", core.ErrJournalReadOnly, cause), raise: new(sync.Once), woke: new(sync.Once)}
		if cur != nil {
			next.ReadOnlyState = cur.ReadOnlyState
		}
		next.ReadOnly, next.Err = true, cause
		if d.ro.CompareAndSwap(cur, next) {
			break
		}
	}
	d.startProbe()
	// Loaded after the swap, while Start stores the workers before it reads
	// the state, so a worker started meanwhile is woken by one of the two.
	if ws := d.workers.Load(); ws != nil {
		for _, st := range *ws {
			st.wake()
		}
	}
}

// raiseOnce raises the loss generation for the read-only entry, once however
// many streams and passes fail records under it. A concurrent caller waits
// for the raise to land, so no stream publishes a loss or answers a Sync
// before the generation is up.
func (d *Durability) raiseOnce() {
	if st := d.ro.Load(); st != nil && st.raise != nil {
		st.raise.Do(func() { d.RaiseWindow() })
	}
}

// wakeOnce wakes every waiter once for the read-only entry.
func (d *Durability) wakeOnce() {
	if st := d.ro.Load(); st != nil && st.woke != nil {
		st.woke.Do(d.notifyAll)
	}
}

// Admit reports whether the journal takes a write: nil, or an error matching
// core.ErrJournalReadOnly that also wraps the cause the journal went
// read-only with. d.Admit is also the func() error the pool, the idle seal
// and the retirer are handed to consult before they write. It reads one
// atomic pointer, so it is safe from any goroutine and under any lock.
func (d *Durability) Admit() error {
	if st := d.ro.Load(); st != nil && st.ReadOnly {
		return st.refusal
	}
	return nil
}

// isReadOnly reports whether the journal is read-only: Admit refuses, which it
// does for that state alone. It is the test the steps that run differently in
// a read-only journal consult (a read served from a copy, a held record's
// claim and re-append, their retry tick), rather than refuse, which is the
// append's own check and can be made to answer for another reason. It reads
// one atomic pointer, so it is safe from any goroutine and under any lock.
func (d *Durability) isReadOnly() bool { return d.Admit() != nil }

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

// Observe returns fs wrapped so that the first EROFS from any call it makes,
// or from any call on a file it returns, makes the journal read-only through
// Enter. A read-only filesystem can answer EROFS to any write path (a segment
// write, a sync, a rename, a directory sync), so the call sites cannot each
// be checked; the one wrapper, put under the journal's storage once when it is
// built, sees them all. It never alters a result: every value and error,
// EROFS included, is returned exactly as the wrapped call gave it, and an
// error that is not EROFS (EIO, ENOSPC) leaves the journal as it was.
func (d *Durability) Observe(fs storage.FS) storage.FS { return &observedFS{d: d, fs: fs} }

// observe enters read-only when err is an EROFS and returns err unchanged.
func (d *Durability) observe(err error) error {
	if err != nil && errors.Is(err, syscall.EROFS) {
		d.Enter(err)
	}
	return err
}

type observedFS struct {
	d  *Durability
	fs storage.FS
}

func (o *observedFS) wrap(f storage.File, err error) (storage.File, error) {
	if f != nil {
		f = &observedFile{d: o.d, f: f}
	}
	return f, o.d.observe(err)
}

func (o *observedFS) Create(name string) (storage.File, error) { return o.wrap(o.fs.Create(name)) }
func (o *observedFS) Open(name string) (storage.File, error)   { return o.wrap(o.fs.Open(name)) }
func (o *observedFS) Rename(oldName, newName string) error {
	return o.d.observe(o.fs.Rename(oldName, newName))
}
func (o *observedFS) Remove(name string) error { return o.d.observe(o.fs.Remove(name)) }
func (o *observedFS) List() ([]string, error) {
	names, err := o.fs.List()
	return names, o.d.observe(err)
}
func (o *observedFS) SyncDir() error { return o.d.observe(o.fs.SyncDir()) }

type observedFile struct {
	d *Durability
	f storage.File
}

func (o *observedFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := o.f.ReadAt(p, off)
	return n, o.d.observe(err)
}

func (o *observedFile) ReadAtDirect(p []byte, off int64) (int, error) {
	n, err := o.f.ReadAtDirect(p, off)
	return n, o.d.observe(err)
}

func (o *observedFile) WriteAt(p []byte, off int64) (int, error) {
	n, err := o.f.WriteAt(p, off)
	return n, o.d.observe(err)
}

func (o *observedFile) Allocate(off, n int64) error { return o.d.observe(o.f.Allocate(off, n)) }
func (o *observedFile) Sync() error                 { return o.d.observe(o.f.Sync()) }
func (o *observedFile) Truncate(size int64) error   { return o.d.observe(o.f.Truncate(size)) }
func (o *observedFile) Size() (int64, error) {
	n, err := o.f.Size()
	return n, o.d.observe(err)
}
func (o *observedFile) Close() error { return o.d.observe(o.f.Close()) }
func (o *observedFile) Name() string { return o.f.Name() }

var (
	_ storage.FS   = (*observedFS)(nil)
	_ storage.File = (*observedFile)(nil)
)
