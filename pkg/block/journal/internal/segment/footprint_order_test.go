package segment_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// footprintWatch is a seam fault that passes every call and, as each one
// arrives, compares the set's lock-free footprint with the bytes the files of
// the directory occupy. The bytes are the files' sizes, a proxy for what the
// segments allocate: Allocate sets each file to the cap, so the two agree here.
// Every call arrives after the one before it completed, so the watch sees the
// state between each pair of storage steps of a make or a retirement. failOn,
// when set, names one op the watch fails with EIO the first time it sees it.
type footprintWatch struct {
	dir    string
	set    *segment.Set
	failOn string

	mu    sync.Mutex
	seen  int
	fails []string
}

func (w *footprintWatch) At(op string, key []byte) (time.Duration, int, error) {
	var disk int64
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		w.note(fmt.Sprintf("before %s %s: reading the directory: %v", op, key, err))
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			disk += info.Size()
		}
	}
	used := w.set.Counters().UsedBytes
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen++
	if used < disk {
		w.fails = append(w.fails, fmt.Sprintf("before %s %s: UsedBytes %d < %d bytes on disk", op, key, used, disk))
	}
	if op == w.failOn {
		w.failOn = ""
		return 0, 0, syscall.EIO
	}
	return 0, 0, nil
}

func (w *footprintWatch) note(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fails = append(w.fails, s)
}

// check fails t on every step the watch saw the footprint below the disk, and
// when the watch saw fewer than min calls, so a watch that was never armed
// cannot pass.
func (w *footprintWatch) check(t *testing.T, min int) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen < min {
		t.Fatalf("the watch saw %d storage calls, want at least %d", w.seen, min)
	}
	for _, f := range w.fails {
		t.Errorf("%s", f)
	}
}

func footprintEnv(t *testing.T, failOn string, targets segment.Targets) (*kit.Seam, *segment.Set, *segment.Maker, *segment.Retirer, *footprintWatch) {
	t.Helper()
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("NewSeam: %v", err)
	}
	set := segment.NewSet(segCap, nil)
	m := segment.NewMaker(seam.FS(), set, noMax, 0, nil)
	var p *segment.Pool
	if targets.Content >= 0 {
		p = segment.NewPool(m, targets, nil, nil)
	}
	r := segment.NewRetirer(segment.RetirerConfig{
		Set:     set,
		Cache:   segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64)),
		Pool:    p,
		Maker:   m,
		FS:      seam.FS(),
		Durable: func(segment.Retirement) {},
	})
	w := &footprintWatch{dir: filepath.Clean(dir), set: set, failOn: failOn}
	seam.SetFault(w)
	return seam, set, m, r, w
}

// A make reserves its cap before the file takes the space, a failed make
// returns the reservation only after the unlink, and a retirement lowers the
// footprint only after the unlink and a recycle keeps it whole: at every
// storage step of each, UsedBytes covers what the directory holds.
func TestFootprintCoversTheDiskAtEveryStorageStep(t *testing.T) {
	t.Run("make", func(t *testing.T) {
		_, _, m, _, w := footprintEnv(t, "", noPool)
		if _, err := m.Make(); err != nil {
			t.Fatalf("Make: %v", err)
		}
		w.check(t, 5)
	})
	t.Run("failed make", func(t *testing.T) {
		_, set, m, _, w := footprintEnv(t, kit.OpRename, noPool)
		if _, err := m.Make(); !errors.Is(err, syscall.EIO) {
			t.Fatalf("Make: %v, want EIO", err)
		}
		w.check(t, 5)
		if used := set.Counters().UsedBytes; used != 0 {
			t.Fatalf("UsedBytes %d after a failed make, want 0", used)
		}
	})
	t.Run("unlink", func(t *testing.T) {
		_, set, m, r, w := footprintEnv(t, "", noPool)
		id := sealedSegment(t, set, m)
		before := w.seenCount()
		_, err := r.Retire(id, segment.NotHeld)
		w.check(t, before+2)
		if err != nil {
			t.Fatalf("Retire: %v", err)
		}
		if used := set.Counters().UsedBytes; used != 0 {
			t.Fatalf("UsedBytes %d after an unlink, want 0", used)
		}
	})
	t.Run("recycle", func(t *testing.T) {
		_, set, m, r, w := footprintEnv(t, "", segment.Targets{Content: 1})
		id := sealedSegment(t, set, m)
		before := w.seenCount()
		res, err := r.Retire(id, segment.NotHeld)
		if err != nil || !res.Recycled {
			t.Fatalf("Retire = %+v, %v, want a recycle", res, err)
		}
		w.check(t, before+3)
	})
}

func (w *footprintWatch) seenCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}

// sealedSegment makes a spare and takes and seals it full, the state a
// retirement starts from.
func sealedSegment(t *testing.T, set *segment.Set, m *segment.Maker) core.SegmentID {
	t.Helper()
	id, err := m.Make()
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	if err := set.Take(id); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := set.Seal(id, true); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return id
}
