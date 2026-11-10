package engine_test

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// strandMessage is what start step 5 logs for each tag it strands.
const strandMessage = "unclaimed tag stranded"

// leaveTag leaves tag holding held bytes in the fake journal, offloaded of
// them offloaded, with no handle of it open, as a share the journal served
// before the restart does.
func (s *starter) leaveTag(t *testing.T, tag journal.ShareTag, held, offloaded int64) {
	t.Helper()

	j := s.j.FakeJournal

	if err := j.SetVersionLimit(journal.Version{Counter: 1 << 40}); err != nil {
		t.Fatal(err)
	}

	h, err := j.Share(tag, 0, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}

	f := journal.NewFileID(entity.NewFileID())

	if _, err := h.WriteAt(t.Context(), f, 0, make([]byte, held), 0); err != nil {
		t.Fatal(err)
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	j.SetOffloaded(tag, offloaded)
}

// A tag no share record names is stranded before Start returns, and before
// the share table is published, so before any file of the journal is served;
// it is reported at Warn with its held and its dirty bytes, which differ.
func TestStartStrandsTheTagNoShareRecordNames(t *testing.T) {
	r := newRig(t)

	r.leaveTag(t, 77, 8192, 4096)

	served := false

	r.j.onStrand = func(journal.ShareTag) {
		served = r.e.BindingOf(r.one.ID) == nil
	}

	r.begin(t)

	if got := r.j.Stranded(); len(got) != 1 || got[0] != 77 {
		t.Fatalf("stranded %v, want exactly [77]", got)
	}

	if served {
		t.Error("share one was served when the tag was stranded, want it not served yet")
	}

	var warns int

	for _, rec := range r.logged() {
		if rec.Message != strandMessage {
			continue
		}

		warns++

		tag, held, dirty := replayAttr(
			rec,
			"tag",
		), replayAttr(
			rec,
			"held",
		), replayAttr(
			rec,
			"dirty",
		)

		if tag != journal.ShareTag(77) || held != int64(8192) || dirty != int64(4096) {
			t.Errorf(
				"logged tag %v held %v dirty %v, want tag 77 held 8192 dirty 4096",
				tag,
				held,
				dirty,
			)
		}
	}

	if warns != 1 {
		t.Errorf("%d records %q at Warn or above, want 1", warns, strandMessage)
	}
}

// A tag a share record names is never stranded: a share the store records and
// Start was not handed, and a share being removed, keep their bytes.
func TestATagAShareRecordNamesIsNeverStranded(t *testing.T) {
	r := newRig(t)

	r.lc.removed = map[entity.ShareID]entity.TagRecord{
		entity.NewShareID(): {
			Tag:   70,
			State: entity.TagServing,
		},
		entity.NewShareID(): {
			Tag:   71,
			State: entity.TagOffloaded,
		},
	}

	r.leaveTag(t, 70, 4096, 0)
	r.leaveTag(t, 71, 4096, 0)
	r.begin(t)

	if got := r.j.Stranded(); len(got) != 0 {
		t.Errorf("stranded %v, want none: the store names both tags", got)
	}
}

// A share handed to Start whose tag the store does not name is attached, and
// its tag is not stranded: an open handle holds it.
func TestAnAttachedTagTheStoreDoesNotNameIsNotStranded(t *testing.T) {
	r := newRig(t)

	r.lc.hidden = map[entity.ShareID]bool{
		r.two.ID: true,
	}

	r.leaveTag(t, r.two.Tag, 4096, 0)
	r.begin(t)

	if got := r.j.Stranded(); len(got) != 0 {
		t.Errorf("stranded %v, want none: share two is attached", got)
	}
}

// A Strand that fails fails Start, and the journal Start opened is closed.
func TestAStrandFailureFailsStartAndClosesTheJournal(t *testing.T) {
	r := newRig(t)
	boom := errors.New("an injected strand failure")

	r.leaveTag(t, 77, 4096, 0)

	r.j.fail = map[string]error{
		"strand 77": boom,
	}

	r.atRestart()

	if err := r.start(t); !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want the strand failure", err)
	}

	if _, err := r.j.FakeJournal.Share(9, 0, journal.Version{}); !errors.Is(
		err,
		journal.ErrClosed,
	) {
		t.Errorf("Share on the journal after a failed Start = %v, want journal.ErrClosed", err)
	}
}
