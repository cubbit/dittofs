package openstate

import (
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

func TestReviewWatchSweepBound(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	dir := in(1)
	f.watch(t, a, dir, true, names)
	count := func() (n int) {
		for _, x := range f.sink.takeNotes() {
			n += len(x.ch)
		}
		return
	}
	for sweep := 0; sweep < 6; sweep++ {
		for i := 0; i < 30*sweep+1; i++ {
			f.changed(dir, ChangeAddEntry, fmt.Sprint(sweep, "-", i), i%2 == 0)
		}
		f.handWatched()
		if n := count(); n > 65 {
			t.Errorf("sweep %d handed %d changes", sweep, n)
		}
	}
}
