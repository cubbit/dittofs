package enginetest

import (
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// A refused Share or Reserve answers a nil interface, never one holding a nil
// pointer, which a caller's nil check would take for a handle.
func TestRefusalAnswersNil(t *testing.T) {
	h, err := wrapJournal(&journal.Journal{}, func(journal.ShareTag, engine.Handle) { t.Error("a refused Share handed a handle on") }).Share(1, 0, journal.Version{})
	if !errors.Is(err, journal.ErrNotImplemented) || h != nil {
		t.Errorf("Share = %v, %v; want nil, the journal's error", h, err)
	}
	r, err := handle{&journal.Handle{}}.Reserve(1, 1)
	if !errors.Is(err, journal.ErrNotImplemented) || r != nil {
		t.Errorf("Reserve = %v, %v; want nil, the journal's error", r, err)
	}
}

// RFC 1 §9.4: the fake answers Share as the journal does. Tag 0, a negative
// limit and a floor of another epoch are refused and raise nothing. A Share
// of a tag whose handle is open returns that handle, its limit replaced, the
// counter raised above the new floor and a file it holds nothing of read as
// of that floor (§3.4), so one Close closes it for both callers; a Share
// after that Close makes a new handle.
func TestShareAgreesWithTheJournal(t *testing.T) {
	for _, c := range []struct {
		name string
		open func(t *testing.T) engine.Journal
	}{
		{"fake", func(t *testing.T) engine.Journal {
			j := NewFakeJournal()
			if err := j.SetVersionLimit(journal.Version{Counter: 1 << 40}); err != nil {
				t.Fatal(err)
			}
			return j
		}},
		{"journal", func(t *testing.T) engine.Journal {
			return wrapJournal(journaltest.New(t).J, func(journal.ShareTag, engine.Handle) {})
		}},
	} {
		t.Run(c.name, func(t *testing.T) { shareTwice(t, c.open(t)) })
	}
}

func shareTwice(t *testing.T, j engine.Journal) {
	const floor, refused = 1 << 20, 1 << 30
	ctx := t.Context()
	id := journal.NewFileID([16]byte{'s'})
	p := []byte("sixsix")

	if _, err := j.Share(0, 0, journal.Version{Counter: refused}); !errors.Is(err, journal.ErrInvalidSetting) {
		t.Errorf("Share of tag 0 = %v, want ErrInvalidSetting", err)
	}
	if _, err := j.Share(1, 0, journal.Version{Epoch: 1, Counter: refused}); !errors.Is(err, journal.ErrUnknownFormat) {
		t.Errorf("Share with a floor of epoch 1 = %v, want ErrUnknownFormat", err)
	}
	if _, err := j.Share(1, -1, journal.Version{Counter: refused}); !errors.Is(err, journal.ErrInvalidSetting) {
		t.Errorf("Share with a negative limit = %v, want ErrInvalidSetting", err)
	}

	h, err := j.Share(1, 1, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}
	var ns *journal.NoSpaceError
	if _, err := h.WriteAt(ctx, id, 0, p, 7); !errors.As(err, &ns) || ns.Limit != journal.LimitShare {
		t.Errorf("a write past the first Share's limit = %v, want a NoSpaceError naming the share's limit", err)
	}

	again, err := j.Share(1, 0, journal.Version{Counter: floor})
	if err != nil {
		t.Fatal(err)
	}
	if again != h {
		t.Fatal("a second Share of an open tag returned a new handle")
	}
	none := journal.NewFileID([16]byte{'n'})
	if _, _, _, asOf, err := h.ReadAt(ctx, none, 0, make([]byte, 1)); err != nil || asOf.Compare(journal.Version{Counter: floor}) != 0 {
		t.Errorf("a read of a file the journal holds nothing of = %v, %v; want as of the second Share's floor %d", asOf, err, floor)
	}
	v, err := h.WriteAt(ctx, id, 0, p, 7)
	if err != nil {
		t.Fatalf("a write through the first caller's handle under the second Share's limit: %v", err)
	}
	if v.Counter != floor+1 {
		t.Errorf("a write after the second Share is at %d, want %d", v.Counter, floor+1)
	}

	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteAt(ctx, id, 0, p, 7); !errors.Is(err, journal.ErrClosed) {
		t.Errorf("a write through the first caller's handle after the second's Close = %v, want ErrClosed", err)
	}

	fresh, err := j.Share(1, 0, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}
	if fresh == h {
		t.Error("a Share after the handle's Close returned the closed handle")
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
}

// seen is what Since yields for one held piece of a write, without its
// version, which the fake and the journal number differently.
type seen struct {
	e    journal.Extent
	over []journal.Extent
}

func piecesOf(cs []journal.Change) []seen {
	var out []seen
	for _, c := range cs {
		out = append(out, seen{c.Extent, c.Over})
	}

	return out
}

// RFC 1 §3.10: the fake yields the same pieces and Over as the journal for
// the same writes, before and after a Settle, so the fake's own tests of Over
// hold it to the journal. The journal cannot reopen on its segments yet, so
// the reopen stays with the fake's own test.
func TestSinceYieldsOverAsTheJournalDoes(t *testing.T) {
	for _, c := range []struct {
		name string
		open func(t *testing.T) engine.Handle
	}{
		{"fake", func(t *testing.T) engine.Handle {
			_, h := fake(t, 0)

			return h
		}},
		{"journal", func(t *testing.T) engine.Handle {
			h, err := wrapJournal(journaltest.New(t).J, func(journal.ShareTag, engine.Handle) {}).
				Share(1, 0, journal.Version{})
			if err != nil {
				t.Fatal(err)
			}

			return h
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := c.open(t)
			v1 := write(t, h, fileA, 0, "aaaaaaaa")
			write(t, h, fileA, 4, "bbbbbbbb")
			v3 := write(t, h, fileA, 2, "cccc")

			// The runs are those of the fake's own test, for the same three writes.
			all := []seen{
				{ext(0, 2), nil},
				{ext(6, 6), []journal.Extent{ext(6, 2)}},
				{ext(2, 4), []journal.Extent{ext(2, 4)}},
			}
			if got := piecesOf(changes(t, h, fileA, journal.Version{})); !slices.EqualFunc(got, all, sameSeen) {
				t.Errorf("Since(0) = %v, want %v", got, all)
			}

			after := []seen{{ext(6, 6), nil}, {ext(2, 4), []journal.Extent{ext(4, 2)}}}
			if got := piecesOf(changes(t, h, fileA, v1)); !slices.EqualFunc(got, after, sameSeen) {
				t.Errorf("Since(v1) = %v, want %v", got, after)
			}

			h.Settle(fileA, v3)

			for i := range all {
				all[i].over = nil
			}

			if got := piecesOf(changes(t, h, fileA, journal.Version{})); !slices.EqualFunc(got, all, sameSeen) {
				t.Errorf("Since(0) after Settle = %v, want %v", got, all)
			}
		})
	}
}

func sameSeen(a, b seen) bool { return a.e == b.e && slices.Equal(a.over, b.over) }
