package datapath

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// watchdog is how long a test waits on a call that must not block before it
// fails the test. It detects a hang; no test orders its steps by it.
const watchdog = 10 * time.Second

// within returns what ch delivers, failing the test when nothing arrives
// within the watchdog: what names the call that should have returned.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(watchdog):
		t.Fatalf("%s did not return: it waited on something it must not", what)
	}
	var zero T
	return zero
}

// releaser returns a function that closes ch once, and registers it to run
// when the test ends, so a test that fails while a hook waits on ch leaves no
// write or read parked for the core's cleanup to wait on.
func releaser(t *testing.T, ch chan struct{}) func() {
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return release
}

// readResult is everything one ReadAt returned.
type readResult struct {
	n       int
	missing []core.Extent
	held    []core.Held
	asOf    core.Version
	err     error
}

// readAsync runs a ReadAt of len(p) bytes at off of f on its own goroutine
// and delivers what it returned.
func readAsync(c *Core, s *Share, f core.FileID, off int64, p []byte) <-chan readResult {
	ch := make(chan readResult, 1)
	go func() {
		var r readResult
		r.n, r.missing, r.held, r.asOf, r.err = c.ReadAt(context.Background(), s, f, off, p)
		ch <- r
	}()
	return ch
}

// served checks that r served data whole from 0 into p at version v.
func served(t *testing.T, r readResult, p, data []byte, v core.Version) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("ReadAt: %v", r.err)
	}
	if r.n != len(data) || len(r.missing) != 0 || !bytes.Equal(p, data) {
		t.Fatalf("n = %d missing %v, bytes equal %v: the record was not served", r.n, r.missing, bytes.Equal(p, data))
	}
	if want := []core.Held{{Extent: core.Extent{Off: 0, Len: int64(len(data))}, Version: v}}; !slices.Equal(r.held, want) {
		t.Fatalf("held = %v, want %v", r.held, want)
	}
}

// servedNothing checks that r served nothing, reported want missing and left
// p, of size bytes, untouched.
func servedNothing(t *testing.T, r readResult, p []byte, want []core.Extent) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("ReadAt: %v", r.err)
	}
	if r.n != 0 || len(r.held) != 0 {
		t.Fatalf("the read served %d bytes, held %v", r.n, r.held)
	}
	if !slices.Equal(r.missing, want) {
		t.Fatalf("missing = %v, want %v", r.missing, want)
	}
	if !bytes.Equal(p, sentinels(len(p))) {
		t.Fatal("p was touched by a read that served nothing")
	}
}

// newRetirer returns a retirement executor over c's ledger, descriptor cache
// and storage, with no pool, so every retirement unlinks, and with the
// durability layer's retired-segment hook, as the journal wires it.
func newRetirer(c *Core, seam *kit.Seam) *segment.Retirer {
	return segment.NewRetirer(segment.RetirerConfig{
		Set:     c.set,
		Cache:   c.cache,
		Maker:   segment.NewMaker(seam.FS(), c.set, unboundedBytes, 0, nil),
		FS:      seam.FS(),
		Retired: c.dur.Retired,
	})
}

// sealedRecord writes data at 0 of f, syncs it and seals its stream's
// segment early, then writes one byte far past it so the stream rolls off the
// sealed segment and drops its own reference to it: the record's segment can
// then be retired. It returns the index's piece of the record.
func sealedRecord(t *testing.T, c *Core, s *Share, f core.FileID, data []byte) index.Placed {
	t.Helper()
	mustWrite(t, c, s, f, 0, data)
	syncFiles(t, c, f)
	pc := recordOf(t, c, f, 0)
	if pc.Extent.Len != int64(len(data)) {
		t.Fatalf("the write landed in more than one record: %+v", pc)
	}
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	mustWrite(t, c, s, f, 1<<30, []byte{1})
	if next := recordOf(t, c, f, 1<<30).Record.Segment; next == pc.Record.Segment {
		t.Fatalf("the stream did not roll off sealed segment %d", next)
	}
	syncFiles(t, c, f)
	return pc
}

// retire retires seg through the executor, as reclamation does once it found
// the segment not held. It reports a refusal with Errorf, so it may run on the
// goroutine of the read it races.
func retire(t *testing.T, c *Core, seam *kit.Seam, seg core.SegmentID) {
	t.Helper()
	if _, err := newRetirer(c, seam).Retire(seg, segment.NotHeld); err != nil {
		t.Errorf("Retire(%d): %v", seg, err)
	}
}

// waitDraining waits until seg's descriptor cache refuses a new reference: a
// retirement's drain of it has begun.
func waitDraining(t *testing.T, c *Core, seg core.SegmentID) {
	t.Helper()
	for deadline := time.Now().Add(watchdog); ; runtime.Gosched() {
		ref, err := c.cache.Acquire(seg)
		if errors.Is(err, segment.ErrSegmentGone) {
			return
		}
		if err == nil {
			ref.Release()
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retirement of segment %d never began its drain: %v", seg, err)
		}
	}
}

func TestReadRacingSegmentRetirement(t *testing.T) {
	data := pattern(4<<10, 21)
	f := testFile(1)

	// The read reads a record's header and then its body through one segment
	// reference. With either read held at storage, the retirement's drain
	// begins behind that reference and waits for it before it unlinks: the
	// read serves the record, from storage not yet freed, and the body read
	// that follows a held header read is not refused by the drain begun before
	// it.
	for _, held := range []struct {
		name string
		nth  int // the read of the segment held: 1 the header's, 2 the body's
	}{{"header", 1}, {"body", 2}} {
		t.Run("retired while the "+held.name+" read holds the reference", func(t *testing.T) {
			c, seam := newTestCore(t)
			s := mustShare(t, c, 1)
			orig := sealedRecord(t, c, s, f, data)
			name := format.SegmentName(orig.Record.Segment)

			hold := seam.HoldFirst(t, kit.OpRead, name)
			p := sentinels(len(data))
			read := readAsync(c, s, f, 0, p)
			within(t, hold.Reached(), "the read's header read")
			if held.nth == 2 {
				// The header read already passed the hold it matched, so the
				// next read of the segment, the body's, is the one this stops.
				body := seam.HoldFirst(t, kit.OpRead, name)
				hold.Release()
				within(t, body.Reached(), "the read's body read")
				hold = body
			}

			retired := make(chan error, 1)
			go func() {
				_, err := newRetirer(c, seam).Retire(orig.Record.Segment, segment.NotHeld)
				retired <- err
			}()
			waitDraining(t, c, orig.Record.Segment)
			if n := seam.CallsFor(kit.OpRemove, name); n != 0 {
				t.Fatalf("the segment was unlinked %d times while a read held a reference to it", n)
			}
			select {
			case err := <-retired:
				t.Fatalf("the retirement returned (%v) while a read held a reference to its segment", err)
			default:
			}

			hold.Release()
			served(t, within(t, read, "the held read"), p, data, orig.Version)
			if err := within(t, retired, "the retirement"); err != nil {
				t.Fatalf("Retire: %v", err)
			}
			if seam.CallsFor(kit.OpRemove, name) == 0 {
				t.Fatal("the retirement did not unlink the segment")
			}
		})
	}

	// The read is held after its resolve and before it takes its reference,
	// while the retirement runs to its end beside it: the read then finds the
	// segment gone. It either serves the record or reports the extent missing
	// with p untouched; with the index still naming the gone segment, its one
	// further resolve finds it gone too, so it reports the extent missing.
	t.Run("retired before the read takes its reference", func(t *testing.T) {
		parked, resume := make(chan struct{}), make(chan struct{})
		var armed atomic.Bool
		c, seam := newTestCore(t, func(o *coreOptions) {
			o.beforeRead = func() {
				if armed.CompareAndSwap(true, false) {
					close(parked)
					<-resume
				}
			}
		})
		release := releaser(t, resume)
		s := mustShare(t, c, 1)
		orig := sealedRecord(t, c, s, f, data)

		armed.Store(true)
		p := sentinels(len(data))
		read := readAsync(c, s, f, 0, p)
		within(t, parked, "the read up to its storage reads")
		retire(t, c, seam, orig.Record.Segment)
		release()

		r := within(t, read, "the read")
		if r.err == nil && r.n == len(data) && bytes.Equal(p, data) {
			return
		}
		servedNothing(t, r, p, []core.Extent{{Off: 0, Len: int64(len(data))}})
	})

	// readRetired builds a core whose read, once it resolved f's span, runs
	// between that resolve and its first storage read whatever change is
	// handed to it, as a change racing the read would. The change runs on the
	// read's goroutine, so it reports with Errorf; h is the record's header,
	// read before the read began.
	readRetired := func(t *testing.T, change func(c *Core, seam *kit.Seam, orig index.Placed, h format.RecordHeader)) (readResult, []byte, index.Placed) {
		t.Helper()
		var c *Core
		var seam *kit.Seam
		var orig index.Placed
		var h format.RecordHeader
		var armed atomic.Bool
		c, seam = newTestCore(t, func(o *coreOptions) {
			o.beforeRead = func() {
				if armed.CompareAndSwap(true, false) {
					change(c, seam, orig, h)
				}
			}
		})
		s := mustShare(t, c, 1)
		orig = sealedRecord(t, c, s, f, data)
		h = headerAt(t, seam, orig.Record)
		armed.Store(true)
		p := sentinels(len(data))
		r := within(t, readAsync(c, s, f, 0, p), "the read")
		if armed.Load() {
			t.Fatal("the read never reached the point between its resolve and its read")
		}
		if seam.CallsFor(kit.OpRemove, format.SegmentName(orig.Record.Segment)) == 0 {
			t.Fatal("the record's segment was not unlinked")
		}
		return r, p, orig
	}

	// The record was copied to another segment and the index repointed to the
	// copy before its segment was retired: the read resolves again and serves
	// the copy.
	t.Run("repointed then retired", func(t *testing.T) {
		r, p, orig := readRetired(t, func(c *Core, seam *kit.Seam, orig index.Placed, h format.RecordHeader) {
			unlock := c.domains.Lock(f)
			pl, err := c.Streams().For(f).Append(format.RecordHeader{
				Kind: format.KindWrite, File: f, Extent: h.Extent, Seq: h.Seq, Version: h.Version, Tag: h.Tag,
			}, format.RecordBody{Payload: data, MTime: 1})
			unlock()
			if err != nil || pl.N != int64(len(data)) || pl.Loc.Segment == orig.Record.Segment {
				t.Errorf("Append of the copy: %+v %v", pl, err)
				return
			}
			moved, hd, err := c.Index().RepointRecord(f, orig.Record, index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)})
			c.Account(hd)
			if err != nil || moved != 1 {
				t.Errorf("RepointRecord = %d, %v", moved, err)
				return
			}
			retire(t, c, seam, orig.Record.Segment)
		})
		served(t, r, p, data, orig.Version)
	})

	// The extent was lost and its segment retired: the read resolves again,
	// finds the extent gone and reports it missing.
	t.Run("dropped then retired", func(t *testing.T) {
		r, p, _ := readRetired(t, func(c *Core, seam *kit.Seam, orig index.Placed, _ format.RecordHeader) {
			v, err := c.counters.Draw()
			if err != nil {
				t.Errorf("Draw: %v", err)
				return
			}
			hd, err := c.Index().Lose(f, orig.Extent, v)
			c.Account(hd)
			if err != nil {
				t.Errorf("Lose: %v", err)
				return
			}
			retire(t, c, seam, orig.Record.Segment)
		})
		servedNothing(t, r, p, []core.Extent{{Off: 0, Len: int64(len(data))}})
	})

	// The segment was retired while the index still names it: the one
	// further resolve names the same gone segment, so the read reports the
	// extent missing rather than resolving forever.
	t.Run("retired while still named", func(t *testing.T) {
		r, p, _ := readRetired(t, func(c *Core, seam *kit.Seam, orig index.Placed, _ format.RecordHeader) {
			retire(t, c, seam, orig.Record.Segment)
		})
		servedNothing(t, r, p, []core.Extent{{Off: 0, Len: int64(len(data))}})
	})
}

// signalDomains closes asked at the first Lock of id once armed is set, then
// takes the domain as the wrapped table does.
type signalDomains struct {
	durability.Domains
	id    core.FileID
	armed *atomic.Bool
	asked chan struct{}
}

func (d signalDomains) Lock(id core.FileID) func() {
	if id == d.id && d.armed.CompareAndSwap(true, false) {
		close(d.asked)
	}
	return d.Domains.Lock(id)
}

// bucketMate returns a file that shares x's floor bucket, found by settling
// candidate files through the index alone and watching x's as-of, which x,
// holding no entry, reads from its bucket. Each candidate holds one piece at
// a fresh version in a segment no read reaches; FailWindow then drops it,
// which leaves the file no content and no marker, so its change version is
// settled into its bucket. The candidates are fixed, so the search is
// deterministic, and bounded far above the bucket count. The candidates'
// pieces name a segment that does not exist and are dropped at once, which is
// harmless here because no read and no retirement ever reaches it.
func bucketMate(t *testing.T, c *Core, x core.FileID) core.FileID {
	t.Helper()
	const nowhere = core.SegmentID(1 << 40)
	for i := range 1 << 16 {
		z := core.NewFileID([16]byte{0xbe, 0xef, byte(i >> 8), byte(i)})
		if z == x {
			continue
		}
		v, err := c.counters.Draw()
		if err != nil {
			t.Fatalf("Draw: %v", err)
		}
		seq, err := c.counters.NextSeq()
		if err != nil {
			t.Fatalf("NextSeq: %v", err)
		}
		pc := index.Placed{Extent: core.Extent{Off: 0, Len: 1}, Record: index.RecordAt{Segment: nowhere}, Version: v}
		hd, err := c.Index().WriteRetain(1, z, pc, seq)
		c.Account(hd)
		if err != nil {
			t.Fatalf("WriteRetain: %v", err)
		}
		_, hd, err = c.Index().FailWindow(z, []index.WindowRecord{{Record: pc.Record, Version: v, Extent: pc.Extent}})
		c.Account(hd)
		if err != nil {
			t.Fatalf("FailWindow: %v", err)
		}
		if c.Index().AsOf(x) == v {
			return z
		}
	}
	t.Fatal("no candidate file shares the floor bucket of x")
	return core.FileID{}
}

func TestReadAsOfWithWriteHeldAtTheSeam(t *testing.T) {
	// A write of X draws its version and appends its record, and is held
	// before it publishes. Z, sharing X's floor bucket, is then written and
	// settled, raising the bucket above the held write's version. A read of X
	// must not report that bucket while the write it did not serve is in
	// flight: it either waits for the write and serves it, or reports an asOf
	// below it.
	t.Run("write", func(t *testing.T) {
		x := testFile(1)
		data := pattern(4<<10, 41)

		var holdWrite, watchLock atomic.Bool
		appended, release := make(chan struct{}), make(chan struct{})
		asked := make(chan struct{})
		resolving, resolveGo := make(chan struct{}, 1), make(chan struct{})
		c, _ := newTestCore(t, func(o *coreOptions) {
			o.afterAppend = func() {
				if holdWrite.CompareAndSwap(true, false) {
					close(appended)
					<-release
				}
			}
			o.wrapDomains = func(d durability.Domains) durability.Domains {
				return signalDomains{Domains: d, id: x, armed: &watchLock, asked: asked}
			}
			o.beforeResolve = func() {
				resolving <- struct{}{}
				<-resolveGo
			}
		})
		releaseWrite, releaseResolve := releaser(t, release), releaser(t, resolveGo)
		s := mustShare(t, c, 1)
		z := bucketMate(t, c, x)

		holdWrite.Store(true)
		type written struct {
			v   core.Version
			err error
		}
		wrote := make(chan written, 1)
		go func() {
			v, err := c.WriteAt(context.Background(), s, x, 0, data, 1)
			wrote <- written{v, err}
		}()
		within(t, appended, "the write of X up to its append")

		// Z is written and settled while X's write is in flight: its version is
		// drawn after X's, and its bucket, X's too, is raised to it.
		vz := mustWrite(t, c, s, z, 0, []byte("z"))
		zr := recordOf(t, c, z, 0)
		_, hd, err := c.Index().FailWindow(z, []index.WindowRecord{{Record: zr.Record, Version: vz, Extent: zr.Extent}})
		c.Account(hd)
		if err != nil {
			t.Fatalf("FailWindow: %v", err)
		}
		if got := c.Index().AsOf(x); got != vz {
			t.Fatalf("X's bucket reads %v, want Z's settled %v: Z does not share it", got, vz)
		}

		watchLock.Store(true)
		p := sentinels(len(data))
		read := readAsync(c, s, x, 0, p)
		var r readResult
		select {
		case <-asked:
			// The read waits on X's append domain: the write publishes first.
			releaseWrite()
			releaseResolve()
			r = within(t, read, "the read of X")
		case <-resolving:
			// The read took its asOf without waiting on the write: it resolves
			// before the write publishes.
			releaseResolve()
			r = within(t, read, "the read of X")
			releaseWrite()
		case <-time.After(watchdog):
			t.Fatal("the read of X neither asked for X's append domain nor resolved its span")
		}
		w := within(t, wrote, "the held write of X")
		if w.err != nil {
			t.Fatalf("WriteAt: %v", w.err)
		}
		if r.err != nil {
			t.Fatalf("ReadAt: %v", r.err)
		}
		if vz.Compare(w.v) <= 0 {
			t.Fatalf("Z's version %v is not above the held write's %v", vz, w.v)
		}

		servedWrite := slices.Contains(r.held, core.Held{Extent: core.Extent{Off: 0, Len: int64(len(data))}, Version: w.v}) && bytes.Equal(p, data)
		if !servedWrite && r.asOf.Compare(w.v) >= 0 {
			t.Fatalf("asOf = %v names the held write %v, which the read did not serve (held %v, missing %v)", r.asOf, w.v, r.held, r.missing)
		}
	})

	// The same row with a truncate to zero of X held in the write's place,
	// settled once it finishes, and a fill at the read's asOf then refused.
	t.Run("truncate", func(t *testing.T) {
		t.Skip("the core has no Truncate and no Fill yet, and the index exports neither a truncate, a deallocate " +
			"nor a removal settle, nor a fill check: a truncate cannot be held between drawing its version and " +
			"publishing it, and no fill can be refused against the read's asOf")
	})
}

func TestReadHoldsNoIndexLockDuringIO(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	data := pattern(4<<10, 51)
	v := mustWrite(t, c, s, f, 0, data)
	syncFiles(t, c, f)
	rec := recordOf(t, c, f, 0).Record

	hold := seam.HoldFirst(t, kit.OpRead, format.SegmentName(rec.Segment))
	p := sentinels(len(data))
	read := readAsync(c, s, f, 0, p)
	within(t, hold.Reached(), "the read's storage read")

	// A loss takes the file's index lock exclusively with no append domain.
	lv, err := c.counters.Draw()
	if err != nil {
		t.Fatal(err)
	}
	lost := make(chan error, 1)
	go func() {
		hd, err := c.Index().Lose(f, core.Extent{Off: 16 << 10, Len: 1}, lv)
		c.Account(hd)
		lost <- err
	}()
	if err := within(t, lost, "a loss of the file whose read is held at storage"); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	// A write to the same file takes its append domain, then its index lock
	// exclusively to publish, while the read's I/O is held.
	wrote := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, f, 8<<10, pattern(100, 52), 1)
		wrote <- err
	}()
	if err := within(t, wrote, "a write to the file whose read is held at storage"); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	hold.Release()
	served(t, within(t, read, "the held read"), p, data, v)
}

// sameStream fails the test unless a and b append through one stream, so a
// write of b held at the seam holds the stream a's read reads from.
func sameStream(t *testing.T, c *Core, a, b core.FileID) {
	t.Helper()
	if c.Streams().For(a) != c.Streams().For(b) {
		t.Fatalf("files %v and %v append through different streams", a, b)
	}
}

func TestReadOfFileANotBlockedByFileBWrite(t *testing.T) {
	// B appends through A's stream, so B's write held anywhere holds what
	// A's read would wait on if it waited on its stream.
	a, b := testFile(1), testFile(3)
	data := pattern(4<<10, 61)

	// B's write holds B's append domain between its append and its publish.
	t.Run("held after its append", func(t *testing.T) {
		var armed atomic.Bool
		appended, release := make(chan struct{}), make(chan struct{})
		c, _ := newTestCore(t, func(o *coreOptions) {
			o.afterAppend = func() {
				if armed.CompareAndSwap(true, false) {
					close(appended)
					<-release
				}
			}
		})
		releaseWrite := releaser(t, release)
		sameStream(t, c, a, b)
		s := mustShare(t, c, 1)
		v := mustWrite(t, c, s, a, 0, data)
		syncFiles(t, c, a)

		armed.Store(true)
		wrote := make(chan error, 1)
		go func() {
			_, err := c.WriteAt(context.Background(), s, b, 0, pattern(100, 62), 1)
			wrote <- err
		}()
		within(t, appended, "the write of B up to its append")

		p := sentinels(len(data))
		served(t, within(t, readAsync(c, s, a, 0, p), "a read of A while B's write is held"), p, data, v)
		releaseWrite()
		if err := within(t, wrote, "the write of B"); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
	})

	// B's write is held at the storage seam, in its record's write.
	t.Run("held in its record write", func(t *testing.T) {
		c, seam := newTestCore(t)
		sameStream(t, c, a, b)
		s := mustShare(t, c, 1)
		v := mustWrite(t, c, s, a, 0, data)
		syncFiles(t, c, a)
		mustWrite(t, c, s, b, 0, pattern(100, 63))
		bseg := recordOf(t, c, b, 0).Record.Segment
		if aseg := recordOf(t, c, a, 0).Record.Segment; aseg != bseg {
			t.Fatalf("A's record is in segment %d and B's in %d, want one segment", aseg, bseg)
		}

		hold := seam.HoldFirst(t, kit.OpWrite, format.SegmentName(bseg))
		wrote := make(chan error, 1)
		go func() {
			_, err := c.WriteAt(context.Background(), s, b, 100, pattern(100, 64), 1)
			wrote <- err
		}()
		within(t, hold.Reached(), "the write of B up to its storage write")

		p := sentinels(len(data))
		served(t, within(t, readAsync(c, s, a, 0, p), "a read of A while B's write is held"), p, data, v)
		hold.Release()
		if err := within(t, wrote, "the write of B"); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
	})

	// A's record is corrupt and the active segment's seal is held in its
	// first sync: the read drops the record and returns, while the loss
	// record its drop owes still waits behind the seal.
	t.Run("corrupt record while the seal is held", func(t *testing.T) {
		c, seam := newTestCore(t)
		s := mustShare(t, c, 1)
		mustWrite(t, c, s, a, 0, data)
		syncFiles(t, c, a)
		rec := recordOf(t, c, a, 0).Record
		corruptPayload(t, c, seam, a, 0)

		hold := seam.HoldFirst(t, kit.OpSync, format.SegmentName(rec.Segment))
		sealed := make(chan error, 1)
		go func() {
			_, err := c.Streams().For(a).Seal(true)
			sealed <- err
		}()
		within(t, hold.Reached(), "the seal of A's segment up to its sync")

		p := sentinels(len(data))
		r := within(t, readAsync(c, s, a, 0, p), "a read of A's corrupt record while the seal is held")
		servedNothing(t, r, p, []core.Extent{{Off: 0, Len: int64(len(data))}})
		if hold.Waiting() != 1 {
			t.Fatal("the seal moved past its held sync before the read returned")
		}
		if _, ok := c.Index().Lookup(a, 0); ok {
			t.Fatal("the corrupt record is still held: Corrupt did not drop it")
		}
		hold.Release()
		if err := within(t, sealed, "the seal"); err != nil {
			t.Fatalf("Seal: %v", err)
		}
	})
}

// TestReadCorruptWithHeadroomExhausted reads a corrupt record when the loss
// record its drop owes cannot be appended: no spare can be made, the ready
// spares are drawn to exhaustion, the headroom class holds none, and the
// stream's segment is sealed, so the header-only record's roll is refused
// naming the headroom. The read reports the extent missing, returns no
// error, and appends nothing; the refusal stays with the worker that records
// the loss.
//
// decision: the headroom exhausted here is the spare pool's headroom class.
// The record-count headroom an open reserves for its own records is not
// admitted by the data path yet, so this does not draw that reserve down;
// the row's full form waits on that admission. For the same reason the count
// of storage writes cannot tell a read that tries to append a record and
// swallows the refusal from one that never tries: with no segment to append
// to, both write nothing. Telling them apart needs the record-count headroom
// admission, which would count the attempt.
func TestReadCorruptWithHeadroomExhausted(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	data := pattern(4<<10, 71)
	mustWrite(t, c, s, f, 0, data)
	syncFiles(t, c, f)
	rec := recordOf(t, c, f, 0).Record
	seq := headerAt(t, seam, rec).Seq
	corruptPayload(t, c, seam, f, 0)

	// The write's roll took a spare, and the pool makes its replacement in the
	// background: one whose make began before the fault below would still
	// land after the drain and take the loss record. The pool is let reach
	// its target, so no make is in flight when the fault is armed.
	awaitSpares(t, c, 0)
	// Every spare the pool would make fails as on a full device.
	if err := seam.Schedule(kit.Failing(kit.OpCreate, "*", 0, syscall.ENOSPC)); err != nil {
		t.Fatal(err)
	}
	refused := false
	for range 1 << 10 {
		if _, err := c.pool.Take(segment.Content); err != nil {
			if !errors.Is(err, core.ErrNoSpace) {
				t.Fatalf("Take(Content) = %v, want a spare refusal", err)
			}
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("the content spares were never drawn to a refusal")
	}
	if _, err := c.pool.Take(segment.Headroom); !errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("Take(Headroom) = %v, want a refusal", err)
	}
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	writes := seam.Calls(kit.OpWrite)

	p := sentinels(len(data))
	r := within(t, readAsync(c, s, f, 0, p), "the read of the corrupt record")
	if errors.Is(r.err, core.ErrNoSpace) {
		t.Fatalf("the read returned the append refusal: %v", r.err)
	}
	servedNothing(t, r, p, []core.Extent{{Off: 0, Len: int64(len(data))}})

	// The worker's loss record is refused for want of a segment, and its
	// sequence number joins the failed list.
	for deadline := time.Now().Add(watchdog); !slices.Contains(c.Durability().Failed(), seq); runtime.Gosched() {
		if time.Now().After(deadline) {
			t.Fatalf("the loss record of seq %d was never refused: failed = %v", seq, c.Durability().Failed())
		}
	}
	if got := seam.Calls(kit.OpWrite); got != writes {
		t.Fatalf("%d storage writes since the read began, want none", got-writes)
	}
}
