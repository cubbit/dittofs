package datapath

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

func TestReadNeverWrittenKeepsSentinel(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	id := core.NewFileID([16]byte{1})
	p := bytes.Repeat([]byte{0xA5}, 100)

	n, missing, held, _, err := c.ReadAt(context.Background(), s, id, 40, p)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
	if len(held) != 0 {
		t.Fatalf("held = %v, want none", held)
	}
	if want := []core.Extent{{Off: 40, Len: 100}}; len(missing) != 1 || missing[0] != want[0] {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	if !bytes.Equal(p, bytes.Repeat([]byte{0xA5}, 100)) {
		t.Fatal("p was touched")
	}
}

func TestReadOfNoBytesIsEmpty(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	id := core.NewFileID([16]byte{2})
	ctx := context.Background()

	n, missing, held, _, err := c.ReadAt(ctx, s, id, 7, nil)
	if err != nil || n != 0 || missing != nil || held != nil {
		t.Fatalf("empty read = %d %v %v %v", n, missing, held, err)
	}

	p := make([]byte, 4)
	if _, _, _, _, err := c.ReadAt(ctx, s, id, -1, p); !errors.Is(err, core.ErrInvalidExtent) {
		t.Fatalf("negative off: %v", err)
	}
	if _, _, _, _, err := c.ReadAt(ctx, s, id, math.MaxInt64-2, p); !errors.Is(err, core.ErrInvalidExtent) {
		t.Fatalf("overflowing end: %v", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, _, err := c.ReadAt(cctx, s, id, 0, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, _, _, _, err := c.ReadAt(cctx, s, id, 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled empty: %v", err)
	}
}

func TestReadResolvesAfterAsOfWithDomainReleased(t *testing.T) {
	var c *Core
	called := false
	f := core.NewFileID([16]byte{3})
	var v core.Version
	c, _ = newTestCore(t, func(o *coreOptions) {
		o.beforeResolve = func() {
			called = true
			// A write needs the file's append domain: this would deadlock if
			// the read still held it.
			s := mustShare(t, c, 2)
			var err error
			if v, err = c.WriteAt(context.Background(), s, f, 0, []byte("x"), 1); err != nil {
				t.Errorf("WriteAt in seam: %v", err)
			}
		}
	})
	s := mustShare(t, c, 3)
	p := []byte{0xA5}
	n, missing, held, asOf, err := c.ReadAt(context.Background(), s, f, 0, p)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("beforeResolve not called")
	}
	// The as-of was read before the write published, so it is below the
	// write's version, while the span resolved after it and serves it.
	if asOf.Compare(v) >= 0 {
		t.Fatalf("asOf = %v, want below the write's %v", asOf, v)
	}
	if n != 1 || p[0] != 'x' || len(missing) != 0 {
		t.Fatalf("read = %d %q missing %v, want the write's byte served", n, p, missing)
	}
	if want := []core.Held{{Extent: core.Extent{Off: 0, Len: 1}, Version: v}}; !slices.Equal(held, want) {
		t.Fatalf("held = %v, want %v", held, want)
	}
}

func TestReadThroughClosedHandleIsRefused(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p := bytes.Repeat([]byte{0xA5}, 8)
	n, missing, held, _, err := c.ReadAt(context.Background(), s, testFile(1), 0, p)
	if !errors.Is(err, core.ErrClosed) {
		t.Fatalf("err = %v, want core.ErrClosed", err)
	}
	if n != 0 || missing != nil || held != nil || !bytes.Equal(p, bytes.Repeat([]byte{0xA5}, 8)) {
		t.Fatalf("closed read = %d %v %v, p %x", n, missing, held, p)
	}
}

// pattern returns n bytes seeded by seed, none of them zero or a sentinel
// byte, from a xorshift stream: no offset shift a test uses maps the pattern
// onto itself, so a read that serves the right length from the wrong place in
// a record shows. pattern(m, seed) is a prefix of pattern(n, seed) for m < n.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*0x9e3779b9 + 0x7f4a7c15
	if x == 0 {
		x = 1
	}
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		v := byte(x%253) + 1 // 1..253
		if v >= 0xA4 {
			v += 2 // 0xA4 and 0xA5 are the tests' sentinels
		}
		b[i] = v
	}
	return b
}

func TestPatternIsNotPeriodic(t *testing.T) {
	const n = 4 << 20
	b := pattern(n, 3)
	if i := bytes.IndexByte(b, 0); i >= 0 {
		t.Fatalf("pattern has a zero byte at %d", i)
	}
	if bytes.IndexByte(b, 0xA4) >= 0 || bytes.IndexByte(b, 0xA5) >= 0 {
		t.Fatal("pattern holds a sentinel byte")
	}
	for _, d := range []int{1, 256, 1000, 4096, 65536, 1 << 20} {
		same := 0
		for i := 0; i+d < n; i++ {
			if b[i] == b[i+d] {
				same++
			}
		}
		// A stream with no period near d agrees with its shift on about one
		// byte in 253.
		if limit := (n - d) / 100; same > limit {
			t.Fatalf("pattern agrees with itself shifted by %d on %d of %d bytes", d, same, n-d)
		}
	}
	if bytes.Equal(pattern(4096, 1), pattern(4096, 2)) {
		t.Fatal("two seeds give the same pattern")
	}
}

// sentinel is the byte a test fills p with to see which bytes a read wrote.
const sentinel = 0xA4

func sentinels(n int) []byte { return bytes.Repeat([]byte{sentinel}, n) }

func TestReadServesRequestedBytesOnly(t *testing.T) {
	c, _ := newTestCore(t, func(o *coreOptions) { o.segmentCap = 4 << 20 })
	s := mustShare(t, c, 1)
	f := testFile(1)
	data := pattern(1<<20, 3)
	v, err := c.WriteAt(context.Background(), s, f, 0, data, 1)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	first, _ := c.Index().Lookup(f, 0)
	last, _ := c.Index().Lookup(f, 1<<20-1)
	if first.Record != last.Record {
		t.Fatalf("the write landed in more than one record: %+v %+v", first, last)
	}

	// The read starts off any 256-byte boundary inside the record, so a read
	// that ignored where in the payload the piece starts would serve the
	// wrong bytes.
	const off, size = 512<<10 + 1000, 4 << 10
	p := sentinels(size)
	n, missing, held, _, err := c.ReadAt(context.Background(), s, f, off, p)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != size || len(missing) != 0 {
		t.Fatalf("n = %d missing %v, want %d and none", n, missing, size)
	}
	if !bytes.Equal(p, data[off:off+size]) {
		t.Fatal("the bytes served are not the bytes written there")
	}
	if want := []core.Held{{Extent: core.Extent{Off: off, Len: size}, Version: v}}; !slices.Equal(held, want) {
		t.Fatalf("held = %v, want %v", held, want)
	}
}

func TestReadHeldNamesEachPieceVersionReadingEachRecordOnce(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()
	older, newer := pattern(8<<10, 1), pattern(2<<10, 2)
	v1, err := c.WriteAt(ctx, s, f, 0, older, 1)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := c.WriteAt(ctx, s, f, 2<<10, newer, 1)
	if err != nil {
		t.Fatal(err)
	}

	want := append(append(append([]byte(nil), older[:2<<10]...), newer...), older[4<<10:]...)
	p := sentinels(8 << 10)
	reads := seam.Calls(kit.OpRead)
	n, missing, held, _, err := c.ReadAt(ctx, s, f, 0, p)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	// Two records, each read once: its header, then its body.
	if got := seam.Calls(kit.OpRead) - reads; got != 4 {
		t.Fatalf("the read made %d storage reads, want 4: two per distinct record", got)
	}
	if n != 8<<10 || len(missing) != 0 || !bytes.Equal(p, want) {
		t.Fatalf("n = %d missing %v, bytes equal %v", n, missing, bytes.Equal(p, want))
	}
	wantHeld := []core.Held{
		{Extent: core.Extent{Off: 0, Len: 2 << 10}, Version: v1},
		{Extent: core.Extent{Off: 2 << 10, Len: 2 << 10}, Version: v2},
		{Extent: core.Extent{Off: 4 << 10, Len: 4 << 10}, Version: v1},
	}
	if !slices.Equal(held, wantHeld) {
		t.Fatalf("held = %v, want %v", held, wantHeld)
	}
}

func TestReadMissingIsExactAcrossManyExtents(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()
	var vs []core.Version
	for i, off := range []int64{100, 300, 500} {
		v, err := c.WriteAt(ctx, s, f, off, pattern(100, byte(i)), 1)
		if err != nil {
			t.Fatal(err)
		}
		vs = append(vs, v)
	}

	// The read starts in a gap and ends in one.
	p := sentinels(600)
	n, missing, held, _, err := c.ReadAt(ctx, s, f, 50, p)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	wantMissing := []core.Extent{{Off: 50, Len: 50}, {Off: 200, Len: 100}, {Off: 400, Len: 100}, {Off: 600, Len: 50}}
	if !slices.Equal(missing, wantMissing) {
		t.Fatalf("missing = %v, want %v", missing, wantMissing)
	}
	if n != 300 {
		t.Fatalf("n = %d, want 300", n)
	}
	wantHeld := []core.Held{
		{Extent: core.Extent{Off: 100, Len: 100}, Version: vs[0]},
		{Extent: core.Extent{Off: 300, Len: 100}, Version: vs[1]},
		{Extent: core.Extent{Off: 500, Len: 100}, Version: vs[2]},
	}
	if !slices.Equal(held, wantHeld) {
		t.Fatalf("held = %v, want %v", held, wantHeld)
	}
	for _, m := range wantMissing {
		if !bytes.Equal(p[m.Off-50:m.Off-50+m.Len], sentinels(int(m.Len))) {
			t.Fatalf("the bytes of missing %v were touched", m)
		}
	}
	for i, h := range wantHeld {
		if !bytes.Equal(p[h.Extent.Off-50:h.Extent.Off-50+h.Extent.Len], pattern(100, byte(i))) {
			t.Fatalf("the bytes of held %v are not the bytes written", h)
		}
	}
}
