package contenttest

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Clock is the time Memory stamps its operations and versions with.
type Clock interface{ Now() time.Time }

// Memory is Content held in memory: every write is durable once staged, so a
// Commit only answers, and nothing is lost but by Lose. A file's operations
// stay pending, in its Overlay and holding their writes' Dones, until
// CommitExistence or Delete commits them. A share answers only once bound,
// and a file only through the share it was first named with.
//
// ponytail: one lock serves every share and file, and a file's bytes are held
// whole, a sparse file costing its size. Split the lock per file, or keep
// extents, if a test's load or sizes ever show in its run time.
type Memory struct {
	clock  Clock
	mu     sync.Mutex
	shares map[entity.ShareID]*journal
	files  map[entity.FileID]*file
	owners map[entity.FileID]entity.ShareID // the share each file was first named with
}

var _ Content = (*Memory)(nil)

// journal is a bound share's: the last version it issued and its loss
// generation, which its verifier carries.
type journal struct {
	last, gen uint64
}

// file is one file's content: its bytes, which of them are data rather than
// hole, its pending operations in version order, and its loss sequence.
type file struct {
	b    []byte
	data []bool
	ops  []op
	loss uint64
}

// op is one pending operation as Overlay replays it.
type op struct {
	truncate bool
	size     int64 // a truncate's size; else the end of the bytes it changed, 0 for none
	at       time.Time
	mtime    bool // it moves the modification time
	version  uint64
	done     func()
}

// New is a Memory with no share bound, reading clock.
func New(clock Clock) *Memory {
	return &Memory{clock: clock, shares: map[entity.ShareID]*journal{}, files: map[entity.FileID]*file{}, owners: map[entity.FileID]entity.ShareID{}}
}

// Bind makes s served: before it, every call naming s answers a Delay with
// DelayFrozen, as the engine answers a share with no binding.
func (m *Memory) Bind(s entity.ShareID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shares[s] == nil {
		m.shares[s] = &journal{}
	}
}

// Lose is a loss of file f of share s, as a failed sync window reports one:
// it raises the share's loss generation, changing its verifier, and f's loss
// sequence.
//
// ponytail: a loss drops no byte, as every write here is durable once staged;
// drop the file's pending writes, calling their Dones, once a test needs to
// read what a loss took.
func (m *Memory) Lose(s entity.ShareID, f entity.FileID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.shares[s]; j != nil {
		j.gen++
		m.file(f).loss++
	}
}

// bound is s's journal for a call naming f: the Delay of a share with no
// binding, or ErrCorrupt for a file first named through another share, as
// the engine answers a file another share's journal holds. The caller holds
// mu.
//
// decision: any call naming f through a bound share makes it that share's,
// where the engine's journal claims a file only at a write or removal. A
// test naming one file through two shares is wrong whichever call does it;
// claim at a write or removal only if a test ever needs a draw or read
// through one share before another writes.
func (m *Memory) bound(s entity.ShareID, f entity.FileID) (*journal, error) {
	j := m.shares[s]
	if j == nil {
		return nil, &vfs.Delay{Cause: vfs.DelayFrozen}
	}
	if was, ok := m.owners[f]; ok && was != s {
		return nil, fmt.Errorf("contenttest: file %v is share %v's, named through %v: %w", f, was, s, vfs.ErrCorrupt)
	}
	m.owners[f] = s
	return j, nil
}

// file is f's content, created empty. The caller holds mu.
func (m *Memory) file(f entity.FileID) *file {
	fl := m.files[f]
	if fl == nil {
		fl = &file{}
		m.files[f] = fl
	}
	return fl
}

// draw is j's next version: above every one it issued, and at least the
// clock's nanoseconds, as the store stamps its own records by time. The
// caller holds mu.
func (m *Memory) draw(j *journal) uint64 {
	j.last = max(j.last+1, uint64(max(m.clock.Now().UnixNano(), 0)))
	return j.last
}

// stage appends o to f's pending operations at j's next version. The caller
// holds mu.
func (m *Memory) stage(j *journal, fl *file, o op) {
	o.at, o.version = m.clock.Now(), m.draw(j)
	fl.ops = append(fl.ops, o)
}

func verifier(j *journal) vfs.Verifier {
	var v vfs.Verifier
	binary.BigEndian.PutUint64(v[:], j.gen+1) // never the zero verifier, which a reply holding none carries
	return v
}

// outside reports whether [off, off+n) is no range of a file.
func outside(off, n int64) bool { return off < 0 || n < 0 || n > math.MaxInt64-off }

// grow extends fl with hole to at least n bytes, appended as zeros over any
// capacity a truncate left, never re-exposing what it cut.
func (fl *file) grow(n int64) {
	if d := n - int64(len(fl.b)); d > 0 {
		fl.b = append(fl.b, make([]byte, d)...)
		fl.data = append(fl.data, make([]bool, d)...)
	}
}

func (m *Memory) Draw(ctx context.Context, s entity.ShareID, f entity.FileID) (entity.JournalVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.bound(s, f)
	if err != nil {
		return entity.JournalVersion{}, err
	}
	return entity.VersionOf(m.draw(j)), nil
}

func (m *Memory) Write(ctx context.Context, s entity.ShareID, f entity.FileID, off int64, p []byte, o engine.WriteOpts) (vfs.Verifier, error) {
	if outside(off, int64(len(p))) {
		return vfs.Verifier{}, vfs.ErrInvalid
	}
	m.mu.Lock()
	j, err := m.bound(s, f)
	if err != nil {
		m.mu.Unlock()
		return vfs.Verifier{}, err
	}
	v := verifier(j)
	if len(p) == 0 { // stages nothing, so nothing commits it
		m.mu.Unlock()
		if o.Done != nil {
			o.Done()
		}
		return v, nil
	}
	fl := m.file(f)
	end := off + int64(len(p))
	fl.grow(end)
	copy(fl.b[off:], p)
	for i := off; i < end; i++ {
		fl.data[i] = true
	}
	m.stage(j, fl, op{size: end, mtime: o.Suspend&entity.TimeModify == 0, done: o.Done})
	m.mu.Unlock()
	return v, nil
}

func (m *Memory) Commit(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) (vfs.Verifier, uint64, error) {
	if outside(off, n) {
		return vfs.Verifier{}, 0, vfs.ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.bound(s, f)
	if err != nil {
		return vfs.Verifier{}, 0, err
	}
	return verifier(j), m.file(f).loss, nil
}

// CommitExistence commits f's pending operations, calling their writes' Dones.
//
// ponytail: the committed size and times are kept nowhere but in the bytes,
// as no File record is written: past this call the Overlay answers nothing
// pending, and a filesystem over Memory reads the size from its own store.
// Commit them through the store's Existence once a filesystem test over
// Memory reads attributes after an existence commit.
func (m *Memory) CommitExistence(ctx context.Context, s entity.ShareID, f entity.FileID) error {
	m.mu.Lock()
	if _, err := m.bound(s, f); err != nil {
		m.mu.Unlock()
		return err
	}
	fl := m.file(f)
	ops := fl.ops
	fl.ops = nil
	m.mu.Unlock()
	finish(ops)
	return nil
}

// finish calls the Done of each of ops' writes.
func finish(ops []op) {
	for _, o := range ops {
		if o.done != nil {
			o.done()
		}
	}
}

func (m *Memory) LossSeq(s entity.ShareID, f entity.FileID) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.bound(s, f); err != nil {
		return 0
	}
	return m.file(f).loss
}

func (m *Memory) Read(ctx context.Context, s entity.ShareID, f entity.FileID, off int64, dst []byte) (int, error) {
	if outside(off, int64(len(dst))) {
		return 0, vfs.ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.bound(s, f); err != nil {
		return 0, err
	}
	b := m.file(f).b
	if off >= int64(len(b)) {
		return 0, nil
	}
	return copy(dst, b[off:]), nil
}

func (m *Memory) Truncate(ctx context.Context, s entity.ShareID, f entity.FileID, size int64) error {
	if size < 0 {
		return vfs.ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.bound(s, f)
	if err != nil {
		return err
	}
	fl := m.file(f)
	if size < int64(len(fl.b)) {
		fl.b, fl.data = fl.b[:size], fl.data[:size]
	}
	fl.grow(size)
	m.stage(j, fl, op{truncate: true, size: size, mtime: true})
	return nil
}

func (m *Memory) Deallocate(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) error {
	if outside(off, n) {
		return vfs.ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.bound(s, f)
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	fl := m.file(f)
	from, to := min(off, int64(len(fl.b))), min(off+n, int64(len(fl.b)))
	clear(fl.b[from:to])
	clear(fl.data[from:to])
	m.stage(j, fl, op{})
	return nil
}

// Delete releases f within the call, its pending writes' Dones called, so it
// never answers a retry channel: Memory has no holder to abort it and no
// clone running past its call.
func (m *Memory) Delete(ctx context.Context, s entity.ShareID, f entity.FileID) (<-chan error, error) {
	m.mu.Lock()
	if _, err := m.bound(s, f); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	var ops []op
	if fl := m.files[f]; fl != nil {
		ops = fl.ops
	}
	delete(m.files, f)
	delete(m.owners, f)
	m.mu.Unlock()
	finish(ops)
	return nil, nil
}

func (m *Memory) Clone(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, x vfs.Extent) error {
	_, err := m.Copy(ctx, srcShare, src, dstShare, dst, []vfs.Extent{x})
	return err
}

// Copy copies xs in order, all at once: Memory has no chunk to fail part-way
// and no deadline to meet. An extent whose source ends past the source's size
// stops it, as the engine's does: ErrInvalid if it is the first, else the
// bytes of the extents before it.
func (m *Memory) Copy(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, xs []vfs.Extent) (int64, error) {
	for _, x := range xs {
		if invalid(x, src == dst) {
			return 0, vfs.ErrInvalid
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.bound(srcShare, src); err != nil {
		return 0, err
	}
	j, err := m.bound(dstShare, dst)
	if err != nil {
		return 0, err
	}
	from, to := m.file(src), m.file(dst)
	var n int64
	for _, x := range xs {
		if x.SrcOff+x.Len > int64(len(from.b)) {
			if n > 0 {
				return n, nil
			}
			return 0, vfs.ErrInvalid
		}
		to.grow(x.DstOff + x.Len)
		copy(to.b[x.DstOff:], from.b[x.SrcOff:x.SrcOff+x.Len])
		copy(to.data[x.DstOff:], from.data[x.SrcOff:x.SrcOff+x.Len])
		m.stage(j, to, op{size: x.DstOff + x.Len, mtime: true})
		n += x.Len
	}
	return n, nil
}

// invalid reports whether x is an extent no clone moves: empty, outside a
// file, or overlapping itself within one file.
func invalid(x vfs.Extent, oneFile bool) bool {
	return x.Len <= 0 || outside(x.SrcOff, x.Len) || outside(x.DstOff, x.Len) ||
		oneFile && x.SrcOff < x.DstOff+x.Len && x.DstOff < x.SrcOff+x.Len
}

// Overlay replays f's pending operations in version order: a truncate sets
// the size exactly, each later write or clone raises it to its end, the
// change time is the newest operation's and the modification time the newest
// of one that moved it.
func (m *Memory) Overlay(ctx context.Context, s entity.ShareID, f entity.FileID) (engine.Overlay, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.bound(s, f); err != nil {
		return engine.Overlay{}, false, err
	}
	var o engine.Overlay
	fl := m.files[f]
	if fl == nil || len(fl.ops) == 0 {
		return o, false, nil
	}
	for _, p := range fl.ops {
		if p.truncate {
			o.Size, o.SizeSet = p.size, true
		} else {
			o.Size = max(o.Size, p.size)
		}
		o.Ctime = p.at
		if p.mtime {
			o.Mtime = p.at
		}
		o.Version = entity.VersionOf(p.version)
	}
	return o, true, nil
}

// Allocation is the longest run from off that is all hole or all data, a
// written zero byte being data; past the end of file the span is empty.
func (m *Memory) Allocation(ctx context.Context, s entity.ShareID, f entity.FileID, off int64) (entity.Span, error) {
	if off < 0 {
		return entity.Span{}, vfs.ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.bound(s, f); err != nil {
		return entity.Span{}, err
	}
	data := m.file(f).data
	if off >= int64(len(data)) {
		return entity.Span{Extent: entity.Extent{Offset: off}, Class: entity.Hole}, nil
	}
	end := off
	for end < int64(len(data)) && data[end] == data[off] {
		end++
	}
	class := entity.Hole
	if data[off] {
		class = entity.Uncarved // data no chunk holds: Memory carves nothing
	}
	return entity.Span{Extent: entity.Extent{Offset: off, Length: end - off}, Class: class}, nil
}
