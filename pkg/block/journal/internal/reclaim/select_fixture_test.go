package reclaim

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// selCap is the cap the selection fixture's segments are accounted at.
const selCap = int64(64 << 10)

// selTag is the share tag the selection tests' records are accounted under.
const selTag = core.ShareTag(1)

func selVer(n uint64) core.Version { return core.Version{Counter: n} }

func selFid(n byte) core.FileID { return core.NewFileID([16]byte{15: n}) }

// selFixture is a real index, a real segment ledger, and one real segment on
// a recorded storage seam: the segment exists on disk, holding nothing.
type selFixture struct {
	ix   *index.Index
	set  *segment.Set
	seam *kit.Seam
	seg  core.SegmentID
	// last is the largest segment id a Maker drew over the seam, so a later
	// spare draws above it instead of over its name.
	last core.SegmentID
}

// newSelFixture makes one spare over the seam and takes it into the set as the
// segment the tests start from.
func newSelFixture(t *testing.T) *selFixture {
	t.Helper()
	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(selCap, nil)
	maker := segment.NewMaker(seam.FS(), set, 1<<62, 0, nil)
	seg, err := maker.Make()
	if err != nil {
		t.Fatalf("the maker could not make a spare: %v", err)
	}
	if err := set.Take(seg); err != nil {
		t.Fatalf("Take: %v", err)
	}
	return &selFixture{ix: index.New(1 << 20), set: set, seam: seam, seg: seg, last: seg}
}

// takeIn makes one more spare over the fixture's seam and takes it into the
// fixture's set: a real further segment holding nothing.
func (f *selFixture) takeIn(t *testing.T) core.SegmentID {
	t.Helper()
	maker := segment.NewMaker(f.seam.FS(), f.set, 1<<62, f.last, nil)
	seg, err := maker.Make()
	if err != nil {
		t.Fatalf("the maker could not make a spare: %v", err)
	}
	if err := f.set.Take(seg); err != nil {
		t.Fatalf("Take: %v", err)
	}
	f.last = seg
	return seg
}
