package contenttest

import (
	"context"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Op names a Content method a fault is set on.
type Op string

const (
	OpDraw            Op = "Draw"
	OpWrite           Op = "Write"
	OpCommit          Op = "Commit"
	OpCommitExistence Op = "CommitExistence"
	OpRead            Op = "Read"
	OpTruncate        Op = "Truncate"
	OpDeallocate      Op = "Deallocate"
	OpDelete          Op = "Delete"
	OpClone           Op = "Clone"
	OpCopy            Op = "Copy"
	OpOverlay         Op = "Overlay"
	OpAllocation      Op = "Allocation"
)

// Call is one call as a fault sees it: its method, the share and file it
// names, a clone's or copy's destination, and the range it names: a Write's
// or Read's offset and length, a Commit's or Deallocate's extent, a
// Truncate's size as Offset, an Allocation's offset.
type Call struct {
	Op     Op
	Share  entity.ShareID
	File   entity.FileID
	Extent entity.Extent
}

// Fault runs as a call arrives. A non-nil error is the call's answer; nil
// passes the call on.
type Fault func(ctx context.Context, c Call) error

// FaultContent is the Content Faults returns.
type FaultContent struct {
	inner Content

	mu      sync.Mutex
	before  map[Op]Fault
	after   map[Op]func(ctx context.Context, c Call)
	pending []<-chan error // retry channels the next Deletes answer, in order
}

var _ Content = (*FaultContent)(nil)

// Faults is Content that passes every call to inner, arguments and results
// unchanged, but for the faults a test sets: On runs one as a call arrives,
// to fail it, hold it or act at that moment, After runs one once inner has
// answered. A fault stays set until replaced or cleared, so it fires on
// every call of its method. LossSeq, which can fail no way, always passes
// through. Its methods are safe for concurrent use.
//
// Every Content method is written out here and none is embedded, so a
// method added to Content fails to compile until it is wrapped.
func Faults(inner Content) *FaultContent {
	return &FaultContent{inner: inner, before: map[Op]Fault{}, after: map[Op]func(context.Context, Call){}}
}

// On sets fault to run as each call of op arrives, replacing the one set
// before; nil clears it. An error it answers is the call's answer, the call
// never reaching inner, except a Read's: inner reads first, so a read that
// fails answers the bytes it read, as the engine's failing part-way does.
func (f *FaultContent) On(op Op, fault Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fault == nil {
		delete(f.before, op)
		return
	}
	f.before[op] = fault
}

// After sets fn to run once inner has answered a call of op, before the call
// returns, replacing the one set before; nil clears it.
func (f *FaultContent) After(op Op, fn func(ctx context.Context, c Call)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fn == nil {
		delete(f.after, op)
		return
	}
	f.after[op] = fn
}

// Pending makes the next Delete that no fault set by On answers leave its
// release pending on retry, as the engine's does while a clone holds the
// file: it answers retry without reaching inner, which then holds the file
// still. Each Pending is answered once, the oldest first.
func (f *FaultContent) Pending(retry <-chan error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, retry)
}

// at runs the fault set on c's method, if any, and answers its error.
func (f *FaultContent) at(ctx context.Context, c Call) error {
	f.mu.Lock()
	fault := f.before[c.Op]
	f.mu.Unlock()
	if fault == nil {
		return nil
	}
	return fault(ctx, c)
}

// done runs what After set on c's method, if any.
func (f *FaultContent) done(ctx context.Context, c Call) {
	f.mu.Lock()
	fn := f.after[c.Op]
	f.mu.Unlock()
	if fn != nil {
		fn(ctx, c)
	}
}

func (f *FaultContent) Draw(ctx context.Context, s entity.ShareID, fl entity.FileID) (entity.JournalVersion, error) {
	c := Call{Op: OpDraw, Share: s, File: fl}
	if err := f.at(ctx, c); err != nil {
		return entity.JournalVersion{}, err
	}
	defer f.done(ctx, c)
	return f.inner.Draw(ctx, s, fl)
}

func (f *FaultContent) Write(ctx context.Context, s entity.ShareID, fl entity.FileID, off int64, p []byte, o engine.WriteOpts) (vfs.Verifier, error) {
	c := Call{Op: OpWrite, Share: s, File: fl, Extent: entity.Extent{Offset: off, Length: int64(len(p))}}
	if err := f.at(ctx, c); err != nil {
		return vfs.Verifier{}, err
	}
	defer f.done(ctx, c)
	return f.inner.Write(ctx, s, fl, off, p, o)
}

func (f *FaultContent) Commit(ctx context.Context, s entity.ShareID, fl entity.FileID, off, n int64) (vfs.Verifier, uint64, error) {
	c := Call{Op: OpCommit, Share: s, File: fl, Extent: entity.Extent{Offset: off, Length: n}}
	if err := f.at(ctx, c); err != nil {
		return vfs.Verifier{}, 0, err
	}
	defer f.done(ctx, c)
	return f.inner.Commit(ctx, s, fl, off, n)
}

func (f *FaultContent) CommitExistence(ctx context.Context, s entity.ShareID, fl entity.FileID) error {
	c := Call{Op: OpCommitExistence, Share: s, File: fl}
	if err := f.at(ctx, c); err != nil {
		return err
	}
	defer f.done(ctx, c)
	return f.inner.CommitExistence(ctx, s, fl)
}

func (f *FaultContent) LossSeq(s entity.ShareID, fl entity.FileID) uint64 {
	return f.inner.LossSeq(s, fl)
}

func (f *FaultContent) Read(ctx context.Context, s entity.ShareID, fl entity.FileID, off int64, dst []byte) (int, error) {
	c := Call{Op: OpRead, Share: s, File: fl, Extent: entity.Extent{Offset: off, Length: int64(len(dst))}}
	defer f.done(ctx, c)
	n, err := f.inner.Read(ctx, s, fl, off, dst)
	if ferr := f.at(ctx, c); ferr != nil {
		return n, ferr
	}
	return n, err
}

func (f *FaultContent) Truncate(ctx context.Context, s entity.ShareID, fl entity.FileID, size int64) error {
	c := Call{Op: OpTruncate, Share: s, File: fl, Extent: entity.Extent{Offset: size}}
	if err := f.at(ctx, c); err != nil {
		return err
	}
	defer f.done(ctx, c)
	return f.inner.Truncate(ctx, s, fl, size)
}

func (f *FaultContent) Deallocate(ctx context.Context, s entity.ShareID, fl entity.FileID, off, n int64) error {
	c := Call{Op: OpDeallocate, Share: s, File: fl, Extent: entity.Extent{Offset: off, Length: n}}
	if err := f.at(ctx, c); err != nil {
		return err
	}
	defer f.done(ctx, c)
	return f.inner.Deallocate(ctx, s, fl, off, n)
}

func (f *FaultContent) Delete(ctx context.Context, s entity.ShareID, fl entity.FileID) (<-chan error, error) {
	c := Call{Op: OpDelete, Share: s, File: fl}
	if err := f.at(ctx, c); err != nil {
		return nil, err
	}
	defer f.done(ctx, c)
	f.mu.Lock()
	var retry <-chan error
	if len(f.pending) > 0 {
		retry, f.pending = f.pending[0], f.pending[1:]
	}
	f.mu.Unlock()
	if retry != nil {
		return retry, nil
	}
	return f.inner.Delete(ctx, s, fl)
}

func (f *FaultContent) Clone(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, x vfs.Extent) error {
	c := Call{Op: OpClone, Share: dstShare, File: dst, Extent: entity.Extent{Offset: x.DstOff, Length: x.Len}}
	if err := f.at(ctx, c); err != nil {
		return err
	}
	defer f.done(ctx, c)
	return f.inner.Clone(ctx, srcShare, src, dstShare, dst, x)
}

func (f *FaultContent) Copy(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, xs []vfs.Extent) (int64, error) {
	c := Call{Op: OpCopy, Share: dstShare, File: dst}
	if err := f.at(ctx, c); err != nil {
		return 0, err
	}
	defer f.done(ctx, c)
	return f.inner.Copy(ctx, srcShare, src, dstShare, dst, xs)
}

func (f *FaultContent) Overlay(ctx context.Context, s entity.ShareID, fl entity.FileID) (engine.Overlay, bool, error) {
	c := Call{Op: OpOverlay, Share: s, File: fl}
	if err := f.at(ctx, c); err != nil {
		return engine.Overlay{}, false, err
	}
	defer f.done(ctx, c)
	return f.inner.Overlay(ctx, s, fl)
}

func (f *FaultContent) Allocation(ctx context.Context, s entity.ShareID, fl entity.FileID, off int64) (entity.Span, error) {
	c := Call{Op: OpAllocation, Share: s, File: fl, Extent: entity.Extent{Offset: off}}
	if err := f.at(ctx, c); err != nil {
		return entity.Span{}, err
	}
	defer f.done(ctx, c)
	return f.inner.Allocation(ctx, s, fl, off)
}
