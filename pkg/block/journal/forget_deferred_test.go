package journal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform/platformtest"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/sim"
)

// deferralSettings are the small-segment settings the deferral legs run
// under, shaped like the fixture's own: two streams, 1 MiB segments — a
// piece of just over half a segment rolls the stream and leaves its sealed
// segment room for the repack's target beside the copy, so the deferred
// loss record the carry places needs no roll of its own.
func deferralSettings() Settings {
	s := Settings{
		MaxBytes:        256 << 20,
		Streams:         2,
		HeadroomRecords: 1024,
		IdleSealBytes:   (1 << 20) / 16,
		// Rolls the repack of a deferral's segment makes find a spare
		// ready, so the unit the leg drives runs whole rather than refused
		// mid-carry on the reserve the pool had not yet refilled.
		SpareTarget: 6,
	}
	core.SetSegmentCap(&s, 1<<20)
	return s
}

// deferralInst is the installation every deferral leg initialises under.
var deferralInst = InstallationID{'d', 'i', 't', 't', 'o', 'f', 's'}

// deferralOpts are the open options every open takes, storage through the
// seam beside the small-cap settings and a clock of its own; the spares wait
// parks Open on the pool's own settle, so every write the legs make finds its
// spares ready.
func deferralOpts(t *testing.T, seam *kit.Seam, dir string, clock core.Clock) []Option {
	t.Helper()
	return []Option{
		WithStorage(seam.FS()),
		WithClock(clock),
		// The probe source the platform-aware tests use, so the journal's
		// own barrier check reads an answer for the directory rather than
		// the host's mount table — the same option the seam's open tests
		// take, without which a host whose temporary directory refuses the
		// check (a barrierless mount) refuses Init here too.
		WithMountProbe(platformtest.SuitableGather(dir)),
		WithSettings(deferralSettings()),
		WithSparesReady(func(w core.SparesWait) {
			if err := w.WaitSpares(context.Background()); err != nil {
				t.Errorf("the pool did not ready its spares: %v", err)
			}
		}),
	}
}

// openDeferralJournal initialises a journal over a fresh seam and directory
// and opens it, answering the journal, the seam and the identity. Every
// later open of the same directory — a crash and reopen included — takes
// the same options, so the reopen sees the same segmentation the writes set
// up.
func openDeferralJournal(t *testing.T) (*Journal, *kit.Seam, string, JournalID, *sim.TestClock) {
	t.Helper()
	dir := t.TempDir()
	clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	id, err := Init(t.Context(), dir, deferralInst, deferralOpts(t, seam, dir, clock)...)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return reopenDeferralJournal(t, seam, dir, id, clock), seam, dir, id, clock
}

// reopenDeferralJournal opens dir through fresh options — the seam's FS is a
// view bound to the crashes it had seen, so every open after a crash takes
// its own — raising a generous version limit so the legs write without
// naming one. id is the identity Init answered, what every open expects
// whatever crash preceded it.
func reopenDeferralJournal(t *testing.T, seam *kit.Seam, dir string, id JournalID, clock core.Clock) *Journal {
	t.Helper()
	j, err := Open(t.Context(), dir, Version{}, id, nil, deferralOpts(t, seam, dir, clock)...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := j.SetVersionLimit(Version{Counter: 1 << 40}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	return j
}

// crashDeferralJournal crashes the seam under j — the power loss the kit
// models, dropping what no sync covered — and leaves the journal object
// dead: its files closed by the crash, its Close never to run again, as the
// fixture's own crash leaves the journal it abandons.
func crashDeferralJournal(t *testing.T, seam *kit.Seam, j *Journal) {
	t.Helper()
	if err := seam.Crash(); err != nil {
		t.Fatalf("the crash: %v", err)
	}
}

// deferralWrite writes n bytes of a distinct pattern at off to id through h,
// syncing the file after: what the legs write survives the crashes.
func deferralWrite(t *testing.T, h *Handle, id FileID, off int64, n int, step byte) {
	t.Helper()
	p := make([]byte, n)
	for i := range p {
		p[i] = step + byte(i%251)
	}
	if _, err := h.WriteAt(t.Context(), id, off, p, 0); err != nil {
		t.Fatalf("the write of %d bytes at %d: %v", n, off, err)
	}
	if _, err := h.Sync(t.Context(), id); err != nil {
		t.Fatalf("the sync after the write: %v", err)
	}
}

// sealedSegOf answers the lowest sealed segment whose entries hold tag's
// records, or the error that none stands — the try shape the clock-driven
// rows poll. It never fails the test itself.
func sealedSegOf(j *Journal, tag ShareTag) (SegmentID, error) {
	var best SegmentID
	found := false
	for _, row := range j.core.Set().Snapshot() {
		if row.State != segment.Active && row.Tags[tag] > 0 {
			if !found || row.ID < best {
				best, found = row.ID, true
			}
		}
	}
	if !found {
		return 0, errors.New("no sealed segment of the tag stands")
	}
	return best, nil
}

// sealedSegmentOf answers the lowest sealed segment whose entries hold tag's
// records — the segment a repack can select and carry, with the room the
// copy of its pieces leaves beside them. It fails the test when no such
// segment stands.
func sealedSegmentOf(t *testing.T, j *Journal, tag ShareTag) SegmentID {
	t.Helper()
	var best SegmentID
	found := false
	for _, row := range j.core.Set().Snapshot() {
		if row.State == segment.Active || row.Tags[tag] == 0 {
			continue
		}
		if !found || row.ID < best {
			best, found = row.ID, true
		}
	}
	if !found {
		t.Fatalf("no sealed segment holds tag %d's records", tag)
	}
	return best
}

// entryTags answers the share tag of every entry Inspect lists for id, so
// a leg asserts no record of the forgotten tag survived a reopen.
func entryTags(t *testing.T, j *Journal, id FileID) []ShareTag {
	t.Helper()
	es, err := j.Inspect().File(id)
	if err != nil {
		t.Fatalf("inspect the loss's file: %v", err)
	}
	out := make([]ShareTag, 0, len(es))
	for _, e := range es {
		out = append(out, e.Tag)
	}
	return out
}

// plantDeferral registers one deferred loss of the tag through the journal's
// own durability registry — the way the registry's own tests reach it, and
// the shape the open's exhausted-reserve pass leaves: the loss record of
// dropped content is owed to whatever copy retires seg, and seg's entries
// know nothing of the tag while it stands.
func plantDeferral(t *testing.T, j *Journal, tag ShareTag, file FileID, seg SegmentID, seq core.Seq) {
	t.Helper()
	if _, err := j.core.Durability().Lose(t.Context(), durability.Loss{
		Share:    core.ShareTag(tag),
		File:     core.FileID(file),
		Extent:   core.Extent{Off: 0, Len: 1 << 16},
		Version:  core.Version{Counter: 1},
		Seq:      seq,
		Reason:   core.LossStale,
		Deferred: true,
		Seg:      core.SegmentID(seg),
	}); err != nil {
		t.Fatalf("the planted deferred loss: %v", err)
	}
}

// TestForgetCoversADeferredLossTheSetDoesNotName checks the forget's
// deferral coverage at the edge the set's per-tag figures cannot see: a
// deferred loss of the tag recorded against a segment whose figures read
// zero for it — the crash shape, an unsynced record whose bytes never
// entered the counters, the registry keeping the entry across the reopen.
// The forget must answer for the deferral both at its emptiness check and
// in its sweep, or a later pass writes the loss record above the forget and
// the tag returns with it: recovery applies the records above the forget's
// sequence number too.
func TestForgetCoversADeferredLossTheSetDoesNotName(t *testing.T) {
	const piece = 600 << 10
	pattern := func(seed byte) []byte {
		p := make([]byte, piece)
		for i := range p {
			p[i] = seed + byte(i%251)
		}
		return p
	}
	tags := [2]ShareTag{3, 8}
	files := [2]FileID{
		NewFileID([16]byte{'d', 'e', 'f', 'e', 'r', '-', 't', 'a', 'g'}),
		NewFileID([16]byte{'d', 'e', 'f', 'e', 'r', '-', 'o', 't', 'h', 'e', 'r'}),
	}
	const budget = int64(8 * (1 << 20))

	// A journal with one rolled, sealed segment of the OTHER tag's records —
	// the zero-figure segment the deferral sits against — a deferred loss of
	// the forgotten tag planted before the crash, and T-side content only
	// where the leg asks for it. The crash over the seam leaves the
	// registry's entry standing: the registry is the directory's own state,
	// which survives the reopen.
	type st struct {
		j     *Journal
		seam  *kit.Seam
		dir   string
		id    JournalID
		su    SegmentID
		clock core.Clock
	}
	setup := func(t *testing.T, tagContent bool) st {
		t.Helper()
		j, seam, dir, id, clock := openDeferralJournal(t)
		th, err := j.Share(tags[0], 0, Version{})
		if err != nil {
			t.Fatalf("attach the forgotten tag: %v", err)
		}
		uh, err := j.Share(tags[1], 0, Version{})
		if err != nil {
			t.Fatalf("attach the other tag: %v", err)
		}
		// Two pieces of just over half a segment: the second rolls the
		// first closed, so the sealed segment holds only the other tag's
		// records and leaves room for the repack's target beside its copy.
		// The forgotten tag's content, where the leg asks for it, is
		// written after: it rolls to its own segment past the sealed one,
		// so the deferral's segment keeps zero figures of the tag — the
		// edge the set's per-tag census cannot see.
		deferralWrite(t, uh, files[1], 0, piece, 'u')
		deferralWrite(t, uh, files[1], piece, piece, 'v')
		if tagContent {
			if _, err := th.WriteAt(t.Context(), files[0], 0, pattern('a'), 0); err != nil {
				t.Fatalf("the forgotten tag's content: %v", err)
			}
			if _, err := th.Sync(t.Context(), files[0]); err != nil {
				t.Fatalf("its sync: %v", err)
			}
		}
		su := sealedSegmentOf(t, j, tags[1])
		plantDeferral(t, j, tags[0], files[0], su, core.Seq(1<<30))
		if err := th.Close(); err != nil {
			t.Fatalf("close the forgotten tag's handle: %v", err)
		}
		if err := uh.Close(); err != nil {
			t.Fatalf("close the other tag's handle: %v", err)
		}
		crashDeferralJournal(t, seam, j)
		return st{j: reopenDeferralJournal(t, seam, dir, id, clock), seam: seam, dir: dir, id: id, su: su, clock: clock}
	}

	// The forgotten-tag check: after the forget, a pass over the
	// deferral's segment and another crash, the file the deferred loss
	// named is claimed by nobody — the other tag's write lands — and the
	// forgotten tag holds no share. The registry probe beside the pass
	// tells whether the forget itself ended the entry or left it for the
	// pass the legs then drive either way.
	forgotten := func(t *testing.T, s st) {
		t.Helper()
		if got := s.j.core.Durability().DeferredAgainst(core.SegmentID(s.su)); len(got) != 0 {
			t.Fatalf("the forget left %d deferred losses of the tag standing against segment %d, whose per-tag figures it consulted name nothing", len(got), s.su)
		}
		if err := s.j.core.WaitSpares(t.Context()); err != nil {
			t.Fatalf("wait the pool before the pass: %v", err)
		}
		if _, err := s.j.Repack(t.Context(), budget, RepackScope{Segments: []SegmentID{s.su}}); err != nil {
			t.Fatalf("the repack over the deferral's segment: %v", err)
		}
		if _, err := s.j.core.Set().State(core.SegmentID(s.su)); err == nil {
			t.Fatalf("the repack left the deferral's segment in the set: the carry never ran")
		}
		crashDeferralJournal(t, s.seam, s.j)
		s.j = reopenDeferralJournal(t, s.seam, s.dir, s.id, s.clock)
		uh, err := s.j.Share(tags[1], 0, Version{})
		if err != nil {
			t.Fatalf("attach the other tag after the crash: %v", err)
		}
		if _, err := uh.WriteAt(t.Context(), files[0], 0, pattern('z'), 0); err != nil {
			t.Fatalf("the write through the other tag after the forget: %v, want the file claimed by nobody", err)
		}
		// The withheld records stay on disk and stay charged — the
		// Charging contract the rebuild itself names — so the share row of
		// a tag whose dead segments the crash kept may answer figures
		// until their retirement; the resurrection this row pins is the
		// one the caller sees: the file's claim and its records never come
		// back.
		for _, tag := range entryTags(t, s.j, files[0]) {
			if tag == tags[0] {
				t.Fatalf("an entry of the forgotten tag survived the forget, the crash and the reopen")
			}
		}
		if err := uh.Close(); err != nil {
			t.Fatalf("close the other tag's handle: %v", err)
		}
	}

	t.Run("a deferred loss of a tag holding nothing else", func(t *testing.T) {
		s := setup(t, false)
		if err := s.j.Forget(t.Context(), tags[0]); err != nil {
			t.Fatalf("the forget of the tag whose only claim is the pending deferral: %v", err)
		}
		forgotten(t, s)
	})

	t.Run("a deferred loss against a segment the set does not name", func(t *testing.T) {
		s := setup(t, true)

		if err := s.j.Forget(t.Context(), tags[0]); err != nil {
			t.Fatalf("the forget of the tag holding content beside the pending deferral: %v", err)
		}
		forgotten(t, s)
	})
}

// TestTheProbeOptionCarriesTheBarrierCheck pins the fixture's own
// platform-aware option: the barrier rule the journal's check enforces
// refuses a mount whose cache flushes are off, and the option is what lets
// the check read an answer for the temporary directory at all. The refusal
// this drives with the check's own test input is the proof the option is
// consulted — the failure the wide-diff runner saw is the same refusal from
// the host's real mount table, which a host whose temporary directory
// passes the check cannot reproduce any other way.
func TestTheProbeOptionCarriesTheBarrierCheck(t *testing.T) {
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	nobarrier := func(string) (storage.Probe, error) {
		return platformtest.NoBarrierMount(dir), nil
	}
	if _, err := Init(t.Context(), dir, deferralInst, WithStorage(seam.FS()), WithMountProbe(nobarrier)); err == nil {
		t.Fatalf("Init over a mount the barrier rule refuses succeeded, want the refusal the check is consulted for")
	}
}
