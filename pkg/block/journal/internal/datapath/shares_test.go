package datapath

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// fileWith returns a file id whose first and last bytes are a and b, so two
// ids differ in bytes a reading of the tag from the id would take.
func fileWith(a, b byte) core.FileID {
	var id [16]byte
	id[0], id[15] = a, b
	return core.NewFileID(id)
}

func shareTags(c *Core) []core.ShareTag {
	tags := c.Shares()
	slices.Sort(tags)
	return tags
}

// Two handles on one core write different files and each record carries its
// own handle's tag.
func TestShare_TwoHandlesWriteTheirOwnTag(t *testing.T) {
	c, seam := newTestCore(t)
	a, b := mustShare(t, c, 11), mustShare(t, c, 22)
	fa, fb := fileWith(0x22, 0x11), fileWith(0x11, 0x22)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, a, fa, 0, []byte("aaaa"), 1); err != nil {
		t.Fatalf("WriteAt through a: %v", err)
	}
	if _, err := c.WriteAt(ctx, b, fb, 0, []byte("bbbb"), 1); err != nil {
		t.Fatalf("WriteAt through b: %v", err)
	}
	got := map[core.FileID]core.ShareTag{}
	for _, r := range readRecords(t, seam) {
		if r.Header.Kind == 0 {
			continue
		}
		got[r.Header.File] = r.Header.Tag
	}
	if got[fa] != 11 || got[fb] != 22 {
		t.Fatalf("record tags = %v for %v and %v, want 11 and 22", got, fa, fb)
	}
}

// A file the journal knows under one handle is refused through another, and
// the index keeps its one entry under the first.
func TestShare_FileIDNamesNoShare(t *testing.T) {
	c, _ := newTestCore(t)
	a, b := mustShare(t, c, 1), mustShare(t, c, 2)
	f := testFile(7)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, a, f, 0, []byte("aaaa"), 1); err != nil {
		t.Fatalf("WriteAt through a: %v", err)
	}
	_, err := c.WriteAt(ctx, b, f, 0, []byte("bbbb"), 1)
	if !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("WriteAt through b = %v, want ErrOtherShare", err)
	}
	var e *core.OtherShareError
	if !errors.As(err, &e) || e.Tag != 2 || e.Owner != 1 || e.File != f {
		t.Fatalf("error = %#v, want an OtherShareError naming tag 2 and owner 1", e)
	}
	entries := c.Index().FileEntries(f)
	if len(entries) != 1 || entries[0].Tag != 1 {
		t.Fatalf("entries = %+v, want one entry owned by tag 1", entries)
	}
}

func TestShare_SameTagReturnsTheSameHandle(t *testing.T) {
	c, _ := newTestCore(t)
	first, err := c.Share(5, 1<<30, core.Version{Counter: 10})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	second, err := c.Share(5, 2<<30, core.Version{Counter: 9e8})
	if err != nil {
		t.Fatalf("Share again: %v", err)
	}
	if first != second {
		t.Fatalf("a second Share on an open tag returned another handle")
	}
	if got := first.Limit(); got != 2<<30 {
		t.Fatalf("Limit = %d, want %d", got, int64(2<<30))
	}
	if got := shareTags(c); !slices.Equal(got, []core.ShareTag{5}) {
		t.Fatalf("Shares = %v, want [5]", got)
	}
	v, err := c.WriteAt(context.Background(), second, testFile(1), 0, []byte("a"), 1)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if v.Counter <= 9e8 {
		t.Fatalf("version %v is not above the second floor", v)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.WriteAt(context.Background(), second, testFile(1), 0, []byte("a"), 1); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("a call through the other reference = %v, want ErrClosed", err)
	}
}

func TestShare_LimitIsWhatShareSet(t *testing.T) {
	c, _ := newTestCore(t)
	s, err := c.Share(3, 1234, core.Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if got := s.Limit(); got != 1234 {
		t.Fatalf("Limit = %d, want 1234", got)
	}
}

// An attached share's floor raises the counter and the floor buckets; a lower
// floor lowers neither.
func TestShare_AttachedShareRaisesCounter(t *testing.T) {
	counters := &index.Counters{}
	if err := counters.RaiseFloor(core.Version{Counter: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := counters.SetVersionLimit(core.Version{Counter: limitCount}); err != nil {
		t.Fatal(err)
	}
	c, _ := newTestCore(t, func(o *coreOptions) { o.state = State{Counters: counters} })
	const floor = uint64(9e8)
	s, err := c.Share(1, 0, core.Version{Counter: floor})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if got := c.Index().AsOf(testFile(9)); got.Counter < floor {
		t.Fatalf("AsOf of a file with no entry = %v, want at least %d", got, floor)
	}
	v1, err := c.WriteAt(context.Background(), s, testFile(1), 0, []byte("a"), 1)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if v1.Counter <= floor {
		t.Fatalf("the first write's version %v is not above the floor %d", v1, floor)
	}

	asOf := c.Index().AsOf(testFile(9))
	other, err := c.Share(2, 0, core.Version{Counter: 5e8})
	if err != nil {
		t.Fatalf("Share with a lower floor: %v", err)
	}
	if got := c.Index().AsOf(testFile(9)); got != asOf {
		t.Fatalf("a lower floor moved a bucket from %v to %v", asOf, got)
	}
	v2, err := c.WriteAt(context.Background(), other, testFile(2), 0, []byte("b"), 1)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if v2.Compare(v1) <= 0 {
		t.Fatalf("a lower floor lowered the counter: %v after %v", v2, v1)
	}
}

// A refused attach registers nothing and moves neither a counter nor a bucket.
func TestShare_RefusalsChangeNothing(t *testing.T) {
	c, _ := newTestCore(t)
	f := testFile(9)
	before, err := c.Draw()
	if err != nil {
		t.Fatal(err)
	}
	asOf := c.Index().AsOf(f)
	high := core.Version{Counter: 9e8}
	cases := []struct {
		name  string
		tag   core.ShareTag
		limit int64
		floor core.Version
		want  error
	}{
		{"tag zero", 0, 0, high, core.ErrInvalidSetting},
		{"negative limit", 1, -1, high, core.ErrInvalidSetting},
		{"epoch floor", 1, 0, core.Version{Epoch: 1, Counter: 9e8}, core.ErrUnknownFormat},
	}
	for _, tc := range cases {
		s, err := c.Share(tc.tag, tc.limit, tc.floor)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: Share = %v, want %v", tc.name, err, tc.want)
		}
		if s != nil {
			t.Fatalf("%s: Share returned a handle with its refusal", tc.name)
		}
	}
	if got := c.Shares(); len(got) != 0 {
		t.Fatalf("Shares = %v after refusals, want none", got)
	}
	if got := c.Index().AsOf(f); got != asOf {
		t.Fatalf("a refusal moved a bucket from %v to %v", asOf, got)
	}
	after, err := c.Draw()
	if err != nil {
		t.Fatal(err)
	}
	if after.Counter >= before.Counter+1000 {
		t.Fatalf("a refusal raised the counter from %v to %v", before, after)
	}
}

// Close refuses new calls at once, waits for the call in flight, and keeps the
// extent held.
func TestHandleClose_WaitsForTheCallInFlight(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	warm(t, c, s, f)
	// other lands in a segment the hold below does not cover.
	pf, _ := c.Index().Lookup(f, 1<<20)
	var other core.FileID
	found := false
	for x := byte(2); x < 64 && !found; x++ {
		other = testFile(x)
		warm(t, c, s, other)
		po, _ := c.Index().Lookup(other, 1<<20)
		found = po.Record.Segment != pf.Record.Segment
	}
	if !found {
		t.Fatal("no file landed in a segment other than f's")
	}
	holds := holdRecordWrites(t, c, seam, f)
	defer holds.release()

	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := c.WriteAt(context.Background(), s, f, 0, []byte("abcd"), 1); err != nil {
			t.Errorf("the write in flight: %v", err)
		}
	})
	eventually(t, "the write to reach the seam", func() bool { return holds.waiting() == 1 })

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()

	refused := false
	for i := 0; i < 1_000_000 && !refused; i++ {
		_, err := c.WriteAt(context.Background(), s, testFile(2), 0, nil, 1)
		refused = errors.Is(err, core.ErrClosed)
		if !refused {
			runtime.Gosched()
		}
	}
	if !refused {
		t.Fatal("a new call was never refused once Close began")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while a call was in flight", err)
	case <-time.After(50 * time.Millisecond):
	}

	// A Share on the tag while the old handle drains makes a new handle, and a
	// write through it is neither refused nor held back by the drain.
	during, err := c.Share(1, 0, core.Version{})
	if err != nil {
		t.Fatalf("Share while the old handle drains: %v", err)
	}
	if during == s {
		t.Fatal("a Share on the tag returned the handle that is closing")
	}
	if _, err := c.WriteAt(context.Background(), during, other, 0, []byte("n"), 1); err != nil {
		t.Fatalf("a write through the new handle while the old one drains: %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v before the held write was released", err)
	default:
	}

	holds.release()
	wg.Wait()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the call finished")
	}
	fresh := mustShare(t, c, 1)
	if fresh == s {
		t.Fatal("a Share after Close returned the closed handle")
	}
	if fresh != during {
		t.Fatal("the handle made while the old one drained is not the tag's open handle")
	}
	if p, ok := c.Index().Lookup(f, 0); !ok || p.Extent.Len != 4 {
		t.Fatalf("Lookup = %+v, %v; want the extent still held", p, ok)
	}
	if _, err := c.WriteAt(context.Background(), fresh, f, 0, []byte("wxyz"), 1); err != nil {
		t.Fatalf("a new handle for the tag cannot write the file: %v", err)
	}
}

// Closing one handle neither fails nor delays another's calls.
func TestHandleClose_AnotherHandleUnaffected(t *testing.T) {
	c, seam := newTestCore(t)
	a, b := mustShare(t, c, 1), mustShare(t, c, 2)
	fa := testFile(1)
	warm(t, c, a, fa)
	pa, _ := c.Index().Lookup(fa, 1<<20)
	var fb core.FileID
	found := false
	for x := byte(2); x < 64 && !found; x++ {
		fb = testFile(x)
		warm(t, c, b, fb)
		pb, _ := c.Index().Lookup(fb, 1<<20)
		found = pb.Record.Segment != pa.Record.Segment
	}
	if !found {
		t.Fatal("no file landed in a segment other than a's")
	}
	holds := holdRecordWrites(t, c, seam, fa)
	defer holds.release()

	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := c.WriteAt(context.Background(), a, fa, 0, []byte("abcd"), 1); err != nil {
			t.Errorf("a's write: %v", err)
		}
	})
	eventually(t, "a's write to reach the seam", func() bool { return holds.waiting() == 1 })
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	eventually(t, "a to refuse new calls", func() bool {
		_, err := c.WriteAt(context.Background(), a, fa, 0, nil, 1)
		return errors.Is(err, core.ErrClosed)
	})
	for i := 0; i < 20; i++ {
		if _, err := c.WriteAt(context.Background(), b, fb, int64(i), []byte("b"), 1); err != nil {
			t.Fatalf("b's write while a closes: %v", err)
		}
	}
	holds.release()
	wg.Wait()
	if err := <-closed; err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func TestHandleClose_TwiceAndConcurrently(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	const closers = 10
	errs := make([]error, closers)
	var wg sync.WaitGroup
	for i := range closers {
		wg.Go(func() { errs[i] = s.Close() })
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a Close hung")
	}
	nils := 0
	for _, err := range errs {
		switch {
		case err == nil:
			nils++
		case !errors.Is(err, core.ErrClosed):
			t.Fatalf("Close = %v, want nil or ErrClosed", err)
		}
	}
	if nils != 1 {
		t.Fatalf("%d Close calls returned nil, want exactly one", nils)
	}
	if err := s.Close(); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("a later Close = %v, want ErrClosed", err)
	}
}

// A call refused with ErrClosed leaves no in-flight count behind.
func TestHandleClose_RefusedCallLeavesNoCount(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		if _, err := c.WriteAt(context.Background(), s, testFile(1), 0, []byte("a"), 1); !errors.Is(err, core.ErrClosed) {
			t.Fatalf("WriteAt = %v, want ErrClosed", err)
		}
	}
	if got := s.state.Load(); got != closedBit {
		t.Fatalf("in-flight word = %#x after the refusals, want only the closed bit", got)
	}
	fresh := mustShare(t, c, 1)
	done := make(chan error, 1)
	go func() { done <- fresh.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close of the fresh handle = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing the fresh handle hung")
	}
}

// A cancelled context is reported before the handle's closed state, and a
// cancelled call on an open handle leaves no count.
func TestHandleClose_ContextBeforeClosed(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.WriteAt(cancelled, s, testFile(1), 0, []byte("a"), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteAt on an open handle = %v, want context.Canceled", err)
	}
	if got := s.state.Load(); got != 0 {
		t.Fatalf("in-flight word = %#x after a cancelled call, want 0", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteAt(cancelled, s, testFile(1), 0, []byte("a"), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteAt on a closed handle = %v, want context.Canceled", err)
	}
}

// Attach and close on rotating tags while writers write through other handles
// and a poller reads the registry: no race, and a Share never returns a handle
// that already refuses a call.
func TestShare_RaceAttachCloseAndWrite(t *testing.T) {
	c, _ := newTestCore(t)
	const (
		cyclers = 4
		writers = 2
		rounds  = 40
	)
	stop := make(chan struct{})
	var pollers sync.WaitGroup
	pollers.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = c.Shares()
			}
		}
	})
	var wg sync.WaitGroup
	for g := range cyclers {
		wg.Go(func() {
			// Every cycler rotates over the same two tags, so Share, a call
			// and Close genuinely race on one tag. A handle another cycler
			// closed refuses a call and a second Close with ErrClosed.
			//
			// This is a -race stress and asserts no more than that: whether a
			// handle Share returned is still open cannot be pinned down, as
			// another cycler may close it before the next line runs.
			for i := range rounds {
				tag := core.ShareTag(100 + (g+i)%2)
				s, err := c.Share(tag, 1, core.Version{})
				if err != nil {
					t.Errorf("Share(%d): %v", tag, err)
					return
				}
				if exit, err := s.enter(); err == nil {
					exit()
				} else if !errors.Is(err, core.ErrClosed) {
					t.Errorf("a call on tag %d: %v", tag, err)
					return
				}
				if err := s.Close(); err != nil && !errors.Is(err, core.ErrClosed) {
					t.Errorf("Close(%d): %v", tag, err)
					return
				}
			}
		})
	}
	for g := range writers {
		s := mustShare(t, c, core.ShareTag(1+g))
		wg.Go(func() {
			f := testFile(byte(40 + g))
			for i := range rounds {
				if _, err := c.WriteAt(context.Background(), s, f, int64(i), []byte("w"), 1); err != nil {
					t.Errorf("WriteAt: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	pollers.Wait()
	for _, tag := range shareTags(c) {
		if tag != 1 && tag != 2 && tag != 100 && tag != 101 {
			t.Fatalf("Shares holds tag %d, which no goroutine attached", tag)
		}
	}
	if got := shareTags(c); !slices.Contains(got, 1) || !slices.Contains(got, 2) {
		t.Fatalf("Shares = %v, want the writers' tags 1 and 2 still open", got)
	}
}

// Shares reads the registry without taking the lock Share and Close take.
func TestShares_PollerNeverBlocksOnShare(t *testing.T) {
	c, _ := newTestCore(t)
	mustShare(t, c, 1)
	c.registryMu.Lock()
	defer c.registryMu.Unlock()
	got := make(chan []core.ShareTag, 1)
	go func() { got <- c.Shares() }()
	select {
	case tags := <-got:
		if !slices.Equal(tags, []core.ShareTag{1}) {
			t.Fatalf("Shares = %v, want [1]", tags)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shares blocked on the registry lock")
	}
}
