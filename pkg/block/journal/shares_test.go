package journal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// newTestJournal builds a Journal over a core on a real directory, with two
// streams and small segments, and closes the core when the test ends. The
// journal has no version limit.
func newTestJournal(t *testing.T) *Journal {
	t.Helper()
	j, _ := newSeamedJournal(t)
	return j
}

// newSeamedJournal is newTestJournal that also returns the storage seam, for a
// test that holds a call at storage.
func newSeamedJournal(t *testing.T) (*Journal, *kit.Seam) {
	t.Helper()
	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	c, err := datapath.New(datapath.Deps{
		FS:          seam.FS(),
		Clock:       kit.NewStopwatchClock(),
		Dir:         t.TempDir(),
		Identity:    core.JournalID{0xd, 0xa, 0x7, 0xa},
		Streams:     2,
		SegmentCap:  64 << 10,
		ExtentLimit: 1 << 20,
		Descriptors: core.NewDescriptorBudget(64),
		Targets:     segment.Targets{Content: 4},
	}, datapath.State{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return newJournal(c), seam
}

// withLimit sets a generous version limit on j.
func withLimit(t *testing.T, j *Journal) {
	t.Helper()
	if err := j.SetVersionLimit(Version{Counter: 1 << 40}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
}

// writeReady writes through h, retrying a refusal for want of a spare segment,
// which clears once the pool has readied its spares.
func writeReady(t *testing.T, h *Handle, id FileID, p string) Version {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, err := h.WriteAt(context.Background(), id, 0, []byte(p), 1)
		var ns *NoSpaceError
		if errors.As(err, &ns) && ns.Limit == LimitSpare && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		return v
	}
}

func fileOf(b byte) FileID { return NewFileID([16]byte{b}) }

func TestJournalShare_HandlesCarryTheirTags(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	a, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share 1: %v", err)
	}
	b, err := j.Share(2, 0, Version{})
	if err != nil {
		t.Fatalf("Share 2: %v", err)
	}
	if a == b {
		t.Fatal("two tags share one handle")
	}
	if got := a.s.Tag; got != 1 {
		t.Errorf("first handle carries tag %d, want 1", got)
	}
	if got := b.s.Tag; got != 2 {
		t.Errorf("second handle carries tag %d, want 2", got)
	}

	f := fileOf(1)
	writeReady(t, a, f, "owned by one")
	_, err = b.WriteAt(context.Background(), f, 0, []byte("x"), 1)
	var other *OtherShareError
	if !errors.As(err, &other) || other.Owner != 1 || other.Tag != 2 {
		t.Fatalf("the other handle's write of the file = %v, want an OtherShareError owned by 1", err)
	}
	if !errors.Is(err, ErrOtherShare) {
		t.Errorf("err = %v, want ErrOtherShare", err)
	}
}

func TestJournalShare_RefusesWhatTheCoreRefuses(t *testing.T) {
	j := newTestJournal(t)
	if _, err := j.Share(0, 0, Version{}); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("tag 0: err = %v, want ErrInvalidSetting", err)
	}
	if _, err := j.Share(1, -1, Version{}); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("negative limit: err = %v, want ErrInvalidSetting", err)
	}
	if _, err := j.Share(1, 0, Version{Epoch: 1}); !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("floor epoch: err = %v, want ErrUnknownFormat", err)
	}
}

func TestJournalShare_SameTagSameHandle(t *testing.T) {
	j := newTestJournal(t)
	a, err := j.Share(1, 10, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	b, err := j.Share(1, 20, Version{})
	if err != nil {
		t.Fatalf("second Share: %v", err)
	}
	if a != b {
		t.Fatal("a second Share of an open tag returned another handle")
	}
	if got := b.s.Limit(); got != 20 {
		t.Fatalf("limit after the second Share = %d, want 20", got)
	}

	var wg sync.WaitGroup
	got := make([]*Handle, 8)
	for i := range got {
		wg.Go(func() {
			h, err := j.Share(2, 0, Version{})
			if err != nil {
				t.Errorf("Share: %v", err)
				return
			}
			got[i] = h
		})
	}
	wg.Wait()
	for i, h := range got {
		if h == nil || h != got[0] {
			t.Fatalf("concurrent Share %d returned another handle", i)
		}
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share after Close: %v", err)
	}
	if c == a {
		t.Fatal("a Share after Close returned the closed handle")
	}
}

func TestHandleClose_PublicRefusesAndWaits(t *testing.T) {
	j, seam := newSeamedJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	writeReady(t, h, fileOf(1), "kept")

	hold := seam.Hold(t, kit.OpWrite, "*")
	defer hold.Release()
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := h.WriteAt(context.Background(), fileOf(2), 0, []byte("held"), 1); err != nil {
			t.Errorf("the write in flight: %v", err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for hold.Waiting() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the write never reached the seam")
		}
		time.Sleep(time.Millisecond)
	}

	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()

	refused := false
	for deadline := time.Now().Add(5 * time.Second); !refused && time.Now().Before(deadline); {
		_, err := h.WriteAt(context.Background(), fileOf(3), 0, nil, 1)
		refused = errors.Is(err, ErrClosed)
		if !refused {
			time.Sleep(time.Millisecond)
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

	hold.Release()
	wg.Wait()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the call finished")
	}
	if err := h.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close = %v, want ErrClosed", err)
	}
	fresh, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share after Close: %v", err)
	}
	if fresh == h {
		t.Fatal("a Share after Close returned the closed handle")
	}
	writeReady(t, fresh, fileOf(1), "again")
}

func TestJournalDraw_PublicLimitSurface(t *testing.T) {
	j := newTestJournal(t)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}

	var ns *NoSpaceError
	if _, err := j.Draw(); !errors.As(err, &ns) || ns.Limit != LimitVersion {
		t.Fatalf("Draw before a limit = %v, want a NoSpaceError naming LimitVersion", err)
	}
	ns = nil
	if _, err := h.WriteAt(context.Background(), fileOf(1), 0, []byte("x"), 1); !errors.As(err, &ns) || ns.Limit != LimitVersion {
		t.Fatalf("WriteAt before a limit = %v, want a NoSpaceError naming LimitVersion", err)
	}

	if err := j.SetVersionLimit(Version{Epoch: 1, Counter: 5}); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("SetVersionLimit with an epoch = %v, want ErrUnknownFormat", err)
	}
	ns = nil
	if _, err := j.Draw(); !errors.As(err, &ns) {
		t.Fatalf("a refused mark set a limit: Draw = %v", err)
	}

	withLimit(t, j)
	v1, err := j.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	v2 := writeReady(t, h, fileOf(1), "x")
	if v2.Counter <= v1.Counter {
		t.Errorf("versions did not rise: Draw %v then WriteAt %v", v1, v2)
	}
}
