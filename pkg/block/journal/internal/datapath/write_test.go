package datapath

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// onDisk is a record read back from a segment file.
type onDisk struct {
	Header format.RecordHeader
	Body   format.RecordBody
}

// readRecords decodes every record of every segment the seam now serves, in
// segment and offset order.
func readRecords(t *testing.T, seam *journaltest.Seam) []onDisk {
	t.Helper()
	names, err := seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	slices.Sort(names)
	var out []onDisk
	for _, name := range names {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		f, err := seam.Open(name)
		if err != nil {
			t.Fatalf("Open %s: %v", name, err)
		}
		size, err := f.Size()
		if err != nil {
			t.Fatalf("Size %s: %v", name, err)
		}
		sc, err := format.ScanSegment(f, size, testIdentity, id)
		if err != nil {
			t.Fatalf("ScanSegment %s: %v", name, err)
		}
		for _, r := range sc.Records {
			buf := make([]byte, r.Header.BodySize())
			if _, err := f.ReadAt(buf, r.Header.Offset+format.RecordHeaderSize); err != nil && len(buf) > 0 {
				t.Fatalf("reading a body in %s: %v", name, err)
			}
			body, err := format.VerifyRecordBody(buf, testIdentity, r.Header)
			if err != nil {
				t.Fatalf("VerifyRecordBody in %s: %v", name, err)
			}
			out = append(out, onDisk{Header: r.Header, Body: body})
		}
		_ = f.Close()
	}
	return out
}

// crashKeepingAll crashes the seam keeping every unsynced write, so the bytes
// a crash may leave are the ones read back.
func crashKeepingAll(t *testing.T, seam *journaltest.Seam) {
	t.Helper()
	n := seam.Unsynced()
	if n > 64 {
		t.Fatalf("%d unsynced writes, more than Keeping can name", n)
	}
	mask := uint64(1)<<n - 1
	if n == 64 {
		mask = ^uint64(0)
	}
	crashedSeams.Store(seam, true)
	if err := seam.Crash(journaltest.Keeping(mask)); err != nil {
		t.Fatalf("Crash: %v", err)
	}
}

// warm writes once to every file so each one's stream holds an active segment
// and waits for the pool to ready its spares again, so the next record write
// is the only write the seam sees.
func warm(t *testing.T, c *Core, s *Share, files ...core.FileID) {
	t.Helper()
	for _, f := range files {
		if _, err := c.WriteAt(context.Background(), s, f, 1<<20, []byte("w"), 1); err != nil {
			t.Fatalf("warm-up WriteAt: %v", err)
		}
	}
	for deadline := time.Now().Add(5 * time.Second); c.pool.Counters().Spares < 2*testStreams; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the pool never readied its spares: %+v", c.pool.Counters())
		}
	}
}

// recordHolds are holds on the record writes of the files' active segments.
type recordHolds []*journaltest.Hold

// holdRecordWrites holds writes to the segment files that the warmed files'
// last pieces are in, one hold per distinct segment. A background write of
// the pool goes to another file, so a held call is a record write.
func holdRecordWrites(t *testing.T, c *Core, seam *journaltest.Seam, files ...core.FileID) recordHolds {
	t.Helper()
	var holds recordHolds
	seen := map[core.SegmentID]bool{}
	for _, f := range files {
		p, ok := c.Index().Lookup(f, 1<<20)
		if !ok {
			t.Fatalf("file %v has no warm piece", f)
		}
		if seen[p.Record.Segment] {
			continue
		}
		seen[p.Record.Segment] = true
		holds = append(holds, seam.Hold(t, journaltest.OpWrite, format.SegmentName(p.Record.Segment)))
	}
	return holds
}

func (h recordHolds) release() {
	for _, x := range h {
		x.Release()
	}
}

func (h recordHolds) waiting() int {
	n := 0
	for _, x := range h {
		n += x.Waiting()
	}
	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestWriteAt_HeldDirtyAndSurvivesCrash(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	payload := bytes.Repeat([]byte{0xab}, 4096)
	v, err := c.WriteAt(context.Background(), s, f, 0, payload, 7)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if p, ok := c.Index().Lookup(f, 0); !ok || p.Version != v || p.Extent != (core.Extent{Off: 0, Len: 4096}) {
		t.Fatalf("Lookup = %+v, %v; want a piece of 4096 bytes at version %v", p, ok, v)
	}
	if h, d, _ := c.heldBytes(); h != 4096 || d != 4096 {
		t.Fatalf("held/dirty = %d/%d, want 4096/4096", h, d)
	}
	crashKeepingAll(t, seam)
	var found bool
	for _, r := range readRecords(t, seam) {
		if r.Header.Kind == format.KindWrite && r.Header.File == f {
			found = true
			if !bytes.Equal(r.Body.Payload, payload) {
				t.Fatalf("the record's payload differs from the write")
			}
		}
	}
	if !found {
		t.Fatal("no write record of the file survived the crash")
	}
}

func TestWriteAt_RecordCarriesTagVersionMTime(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 5)
	f := testFile(2)
	v, err := c.WriteAt(context.Background(), s, f, 8, []byte("abcd"), 12345)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	seq, err := c.counters.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Streams().For(f).Append(format.RecordHeader{Kind: format.KindTruncate, File: f, Seq: seq, Tag: 5}, format.RecordBody{MTime: 999}); err != nil {
		t.Fatalf("Append truncate: %v", err)
	}
	var w, tr *onDisk
	recs := readRecords(t, seam)
	for i := range recs {
		switch recs[i].Header.Kind {
		case format.KindWrite:
			w = &recs[i]
		case format.KindTruncate:
			tr = &recs[i]
		}
	}
	if w == nil || tr == nil {
		t.Fatalf("records read back: %+v", recs)
	}
	if w.Header.Tag != s.Tag || w.Header.Version != v || w.Header.Seq == 0 || w.Body.MTime != 12345 {
		t.Fatalf("write record tag/version/seq/mtime = %d/%v/%d/%d; want %d/%v/>0/12345",
			w.Header.Tag, w.Header.Version, w.Header.Seq, w.Body.MTime, s.Tag, v)
	}
	if tr.Body.MTime != 0 || len(tr.Body.Payload) != 0 {
		t.Fatalf("a truncate record carries a body: %+v", tr.Body)
	}
}

func TestWriteAt_PublishFollowsRecordWrite(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(3)
	warm(t, c, s, f)
	holds := holdRecordWrites(t, c, seam, f)
	done := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, f, 0, []byte("abcd"), 1)
		done <- err
	}()
	<-holds[0].Reached()
	select {
	case err := <-done:
		t.Fatalf("WriteAt returned (%v) while its record write was held", err)
	default:
	}
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the piece was published before its record was written")
	}
	holds.release()
	if err := <-done; err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, ok := c.Index().Lookup(f, 0); !ok {
		t.Fatal("the piece did not appear once the record was written")
	}
}

func TestWriteAt_PublishDoesNotWaitForSync(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(4)
	warm(t, c, s, f)
	wp, _ := c.Index().Lookup(f, 1<<20)
	seg := format.SegmentName(wp.Record.Segment)
	if err := seam.Schedule(journaltest.Failing(journaltest.OpSync, seg, 0, errors.New("sync boom"))); err != nil {
		t.Fatal(err)
	}
	before := seam.CallsFor(journaltest.OpSync, seg)
	if _, err := c.WriteAt(context.Background(), s, f, 0, []byte("abcd"), 1); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if got := seam.CallsFor(journaltest.OpSync, seg); got != before {
		t.Fatalf("WriteAt made %d sync calls", got-before)
	}
	if _, ok := c.Index().Lookup(f, 0); !ok {
		t.Fatal("the write was not published")
	}
}

func TestWriteAt_VersionsAppendedInAssignmentOrder(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(5)
	const writers, each = 16, 20
	var wg sync.WaitGroup
	for g := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range each {
				off := int64(g*each+k) * 8
				if _, err := c.WriteAt(context.Background(), s, f, off, []byte("12345678"), 1); err != nil {
					t.Errorf("WriteAt: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	var versions []uint64
	for _, r := range readRecords(t, seam) {
		if r.Header.Kind == format.KindWrite && r.Header.File == f {
			versions = append(versions, r.Header.Version.Counter)
		}
	}
	if len(versions) != writers*each {
		t.Fatalf("%d write records on disk, want %d", len(versions), writers*each)
	}
	for i := 1; i < len(versions); i++ {
		if versions[i] <= versions[i-1] {
			t.Fatalf("record %d carries version %d after %d: appended out of assignment order", i, versions[i], versions[i-1])
		}
	}
}

func TestWriteAt_DifferentFilesRunInParallel(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f1, f2 := testFile(6), testFile(7)
	warm(t, c, s, f1, f2)
	holds := holdRecordWrites(t, c, seam, f1, f2)
	var wg sync.WaitGroup
	for _, f := range []core.FileID{f1, f2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.WriteAt(context.Background(), s, f, 0, []byte("abcd"), 1); err != nil {
				t.Errorf("WriteAt: %v", err)
			}
		}()
	}
	eventually(t, "both writers to reach the seam at once", func() bool { return holds.waiting() == 2 })
	holds.release()
	wg.Wait()
}

func TestWriteAt_OverwriteServesNewBytesAtOnce(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(8)
	ctx := context.Background()
	va, err := c.WriteAt(ctx, s, f, 0, []byte("AAAAAAAA"), 1)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := c.WriteAt(ctx, s, f, 4, []byte("BBBBBBBB"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := c.Index().Lookup(f, 0); !ok || p.Version != va {
		t.Fatalf("Lookup(0) = %+v, %v; want version %v", p, ok, va)
	}
	for _, off := range []int64{4, 7, 11} {
		if p, ok := c.Index().Lookup(f, off); !ok || p.Version != vb {
			t.Fatalf("Lookup(%d) = %+v, %v; want the overwriting version %v", off, p, ok, vb)
		}
	}
	if got := c.Index().Retained(f); len(got) != 1 || got[0].By != vb {
		t.Fatalf("retained = %+v, want one piece superseded by %v", got, vb)
	}
}

// probeDomains is a file append domain table that records which files'
// domains are held.
type probeDomains struct {
	inner durability.Domains
	mu    sync.Mutex
	held  map[core.FileID]int
}

func (p *probeDomains) Lock(id core.FileID) func() {
	unlock := p.inner.Lock(id)
	p.mu.Lock()
	p.held[id]++
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		if p.held[id]--; p.held[id] == 0 {
			delete(p.held, id)
		}
		p.mu.Unlock()
		unlock()
	}
}

func (p *probeDomains) snapshot() map[core.FileID]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[core.FileID]int{}
	for k, v := range p.held {
		out[k] = v
	}
	return out
}

func TestWriteAt_OnlyTheFileAppendDomainIsHeldDuringTheRecordWrite(t *testing.T) {
	var probe *probeDomains
	c, seam := newTestCore(t, func(o *coreOptions) {
		o.wrapDomains = func(d durability.Domains) durability.Domains {
			probe = &probeDomains{inner: d, held: map[core.FileID]int{}}
			return probe
		}
	})
	s := mustShare(t, c, 1)
	f := testFile(9)
	warm(t, c, s, f)
	holds := holdRecordWrites(t, c, seam, f)
	done := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, f, 0, []byte("abcd"), 1)
		done <- err
	}()
	<-holds[0].Reached()
	got := probe.snapshot()

	// Nothing but the file's append domain is held: the index's locks, for
	// this file and another, and the counters' are free during the write.
	within := func(what string, fn func()) {
		t.Helper()
		ok := make(chan struct{})
		go func() { fn(); close(ok) }()
		select {
		case <-ok:
		case <-time.After(2 * time.Second):
			t.Errorf("%s waited on the held record write", what)
		}
	}
	within("Lookup of another file", func() { c.Index().Lookup(testFile(99), 0) })
	within("Lookup of the written file", func() { c.Index().Lookup(f, 1<<20) })
	within("Account", func() {
		c.Account(index.HeldDelta{HeldBytes: 1})
		c.Account(index.HeldDelta{HeldBytes: -1})
	})
	holds.release()
	if err := <-done; err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if len(got) != 1 || got[f] != 1 {
		t.Fatalf("domains held during the record write = %v, want only the file's append domain", got)
	}
}

func TestWriteAt_ErrorsAppendNothing(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name  string
		limit uint64 // a version limit to set, or 0 to keep the default
		ctx   context.Context
		off   int64
		len   int
		want  func(error) bool
	}{
		{"version limit", 1, context.Background(), 0, 4, func(err error) bool { return errors.Is(err, core.ErrNoSpace) }},
		{"negative offset", 0, context.Background(), -1, 4, func(err error) bool { return errors.Is(err, core.ErrInvalidExtent) }},
		{"offset overflow", 0, context.Background(), math.MaxInt64 - 2, 4, func(err error) bool { return errors.Is(err, core.ErrInvalidExtent) }},
		{"cancelled", 0, cancelled, 0, 4, func(err error) bool { return errors.Is(err, context.Canceled) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counters := &index.Counters{}
			limit := tc.limit
			if limit == 0 {
				limit = 1 << 40
			}
			if err := counters.SetVersionLimit(core.Version{Counter: limit}); err != nil {
				t.Fatal(err)
			}
			c, seam := newTestCore(t, func(o *coreOptions) { o.state = State{Counters: counters} })
			s := mustShare(t, c, 1)
			f := testFile(10)
			writes := seam.Calls(journaltest.OpWrite)
			v, err := c.WriteAt(tc.ctx, s, f, tc.off, make([]byte, tc.len), 1)
			if !tc.want(err) {
				t.Fatalf("WriteAt = %v", err)
			}
			if v != (core.Version{}) {
				t.Fatalf("a refused write returned version %v", v)
			}
			if got := seam.Calls(journaltest.OpWrite); got != writes {
				t.Fatalf("the refused write made %d write calls", got-writes)
			}
			if h, d, r := c.heldBytes(); h != 0 || d != 0 || r != 0 || c.WrittenBytes() != 0 {
				t.Fatalf("counters = %d/%d/%d, written %d; want all zero", h, d, r, c.WrittenBytes())
			}
			if _, ok := c.Index().Lookup(f, 0); ok {
				t.Fatal("a piece was published")
			}
			if seq, err := counters.NextSeq(); err != nil || seq != 1 {
				t.Fatalf("NextSeq = %d, %v; the refused write drew a sequence number", seq, err)
			}
			if tc.limit == 0 {
				if v, err := counters.Draw(); err != nil || v.Counter != 1 {
					t.Fatalf("Draw = %v, %v; the refused write drew a version", v, err)
				}
			}
		})
	}
}

func TestWriteAt_ZeroLengthDoesNothing(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	writes := seam.Calls(journaltest.OpWrite)
	v, err := c.WriteAt(context.Background(), s, testFile(11), 0, nil, 1)
	if v != (core.Version{}) || err != nil {
		t.Fatalf("WriteAt of nothing = %v, %v; want the zero version and no error", v, err)
	}
	if seam.Calls(journaltest.OpWrite) != writes || c.WrittenBytes() != 0 {
		t.Fatal("a zero-length write appended or counted")
	}
	if got := c.counters.NextVersion(); got.Counter != 1 {
		t.Fatalf("next version = %v, a zero-length write drew one", got)
	}
}

func TestWriteAt_CountsWrittenBytes(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	ctx := context.Background()
	for _, n := range []int{4, 5, 0} {
		if _, err := c.WriteAt(ctx, s, testFile(12), 0, make([]byte, n), 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.WriteAt(ctx, s, testFile(12), -1, make([]byte, 100), 1); err == nil {
		t.Fatal("a negative offset was accepted")
	}
	if got := c.WrittenBytes(); got != 9 {
		t.Fatalf("WrittenBytes = %d, want 9", got)
	}
}

// A publish the index refuses after the record was appended returns the zero
// version with the error; the record stays in its segment.
func TestWriteAt_PublishRefusalReturnsNoVersion(t *testing.T) {
	c, seam := newTestCore(t)
	f := testFile(13)
	publish(t, c, f, 0, "aaaa") // the file belongs to tag 1
	v, err := c.WriteAt(context.Background(), mustShare(t, c, 2), f, 8, []byte("bbbb"), 1)
	if !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("WriteAt through another tag = %v, want ErrOtherShare", err)
	}
	if v != (core.Version{}) {
		t.Fatalf("a refused publish returned version %v", v)
	}
	if _, ok := c.Index().Lookup(f, 8); ok {
		t.Fatal("a refused write was published")
	}
	if c.WrittenBytes() != 0 {
		t.Fatalf("WrittenBytes = %d after a refused write", c.WrittenBytes())
	}
	var rec int
	for _, r := range readRecords(t, seam) {
		if r.Header.Kind == format.KindWrite && r.Header.Tag == 2 {
			rec++
		}
	}
	if rec != 1 {
		t.Fatalf("%d records of the refused write on disk, want 1", rec)
	}
}
