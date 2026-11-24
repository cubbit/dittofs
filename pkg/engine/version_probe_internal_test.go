package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/sim"
)

func TestVersionLimitProbeDoesNotAllocate(t *testing.T) {
	e, vl := newVersionLimitProbe(t, 128)
	j := vl.j
	limit := journal.Version{Counter: j.Stats().NextVersion.Counter + 32}
	if err := j.SetVersionLimit(limit); err != nil {
		t.Fatalf("set version limit: %v", err)
	}
	vl.last.Store(limit.Counter)

	if e.stillDue(vl) {
		t.Fatal("raise is due with 32 versions of headroom")
	}

	if _, err := j.Draw(); err != nil {
		t.Fatalf("draw a version: %v", err)
	}

	if !e.stillDue(vl) {
		t.Fatal("raise is not due with 31 versions of headroom")
	}

	if n := testing.AllocsPerRun(100, func() { _ = e.stillDue(vl) }); n != 0 {
		t.Errorf("version limit probe allocates %g times per call, want 0", n)
	}
}

func BenchmarkVersionLimitProbe(b *testing.B) {
	for _, shares := range []int{1, 1000, 10000} {
		b.Run(fmt.Sprint(shares), func(b *testing.B) {
			e, vl := newVersionLimitProbe(b, shares)
			vl.last.Store(64)

			b.ReportAllocs()
			for b.Loop() {
				e.stillDue(vl)
			}
		})
	}
}

func newVersionLimitProbe(tb testing.TB, shares int) (*Engine, *versionLimit) {
	tb.Helper()
	j := journaltest.New(tb).J
	for tag := 1; tag <= shares; tag++ {
		if _, err := j.Share(journal.ShareTag(tag), 0, journal.Version{}); err != nil {
			tb.Fatalf("attach tag %d: %v", tag, err)
		}
	}

	e := &Engine{d: Deps{Clock: sim.NewTestClock(time.Time{})}, block: 64}
	return e, newVersionLimit(JournalOf(j), marks{})
}
