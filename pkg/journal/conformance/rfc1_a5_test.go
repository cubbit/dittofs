//go:build journal_conformance

package conformance

import (
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// TestRFC1A_ReadAsOfAgainstAChangeInFlight checks that a read never reports
// an asOf above a change of its file still in flight. A file X the journal
// holds nothing of reports its floor bucket as asOf; another file Z of the
// same bucket, written, deleted and settled while a change of X is held at
// the storage seam after its version was drawn, raises that bucket past the
// held change's version. A read of X taken then must serve the change or
// report an asOf below it: in the write leg the read is checked directly,
// and in the truncate leg a Fill with the read's asOf, after the truncate is
// settled, must be refused. A read that reports the raised bucket fails both.
//
// Z is found as the row says, by churning files and reading X after each,
// among the files on the other append stream than X's: a churn on X's own
// stream would queue behind the held append of X and never raise anything.
func TestRFC1A_ReadAsOfAgainstAChangeInFlight(t *testing.T) {
	for _, leg := range []string{"write", "truncate"} {
		t.Run(leg, func(t *testing.T) {
			f := journaltest.New(t)
			h := f.Share(0)
			x := journaltest.File(1000)
			if leg == "truncate" {
				x = journaltest.File(2000)
			}

			// X's stream, learned from a write of X that is then deleted and
			// settled, so X again holds nothing and reads its bucket.
			write(t, h, x, 0, pattern("x-probe", 4096))
			xStream := streamOf(t, f, entryAt(t, f, x, 0).Location.Segment, true)
			settleDelete(t, h, x)

			asOf := func() journal.Version {
				t.Helper()
				r := read(t, h, x, 0, 4096)
				if len(r.missing) != 1 {
					t.Fatalf("a read of X, which the journal holds nothing of, reports missing %+v, want it all", r.missing)
				}
				return r.asOf
			}

			// churn writes, deletes and settles file z, and returns the
			// segment its write landed in.
			churn := func(z journal.FileID) journal.SegmentID {
				t.Helper()
				write(t, h, z, 0, []byte{1})
				seg := entryAt(t, f, z, 0).Location.Segment
				settleDelete(t, h, z)
				return seg
			}

			const tries = 200000
			z, found := journal.FileID{}, false
			prev := asOf()
			for i := range tries {
				c := journaltest.File(100000 + i)
				seg := churn(c)
				cur := asOf()
				if cur.Compare(prev) > 0 && streamOf(t, f, seg, true) != xStream {
					z, found = c, true
					break
				}
				prev = cur
			}
			if !found {
				t.Fatalf("none of %d files on the other stream raised X's asOf: no bucket mate of X to churn", tries)
			}

			// Hold X's change at the seam, after its version is drawn: the
			// first segment write once it starts, which only its append makes
			// while the test's goroutine waits on it. It is the record's write,
			// or the header of a segment the append rolls to first.
			held := f.Seam.HoldFirst(t, kit.OpWrite, "*.seg")
			type change struct {
				v   journal.Version
				err error
			}
			changed := make(chan change, 1)
			go func() {
				var c change
				if leg == "write" {
					c.v, c.err = h.WriteAt(t.Context(), x, 0, pattern("x-in-flight", 4096), 1)
				} else {
					c.v, c.err = h.Truncate(t.Context(), x, 0)
				}
				changed <- c
			}()
			select {
			case <-held.Reached():
			case c := <-changed:
				t.Fatalf("X's %s returned %v without writing a segment: the hold missed it", leg, c.err)
			case <-time.After(10 * time.Second):
				t.Fatalf("X's %s never reached a segment write", leg)
			}

			// Z, a bucket mate on the other stream, raises the bucket past
			// the held change's version while it is in flight.
			churn(z)

			reads := make(chan readOut, 1)
			readErrs := make(chan error, 1)
			go func() {
				r, err := readErr(t.Context(), h, x, 0, 4096)
				if err != nil {
					readErrs <- err
					return
				}
				reads <- r
			}()
			// A bounded wait, the one this check can make: a read that waits
			// out the held change returns only after the release, while one
			// that takes its asOf without waiting returns in it. Either is
			// judged below by what it reported; the wait only lets the second
			// kind return while the change is still in flight.
			var r readOut
			early := false
			select {
			case r = <-reads:
				early = true
			case err := <-readErrs:
				t.Fatalf("the read of X under the held %s: %v", leg, err)
			case <-time.After(100 * time.Millisecond):
			}
			held.Release()

			c := <-changed
			stub(t, leg, c.err)
			if c.err != nil {
				t.Fatalf("X's %s: %v", leg, c.err)
			}
			if !early {
				select {
				case r = <-reads:
				case err := <-readErrs:
					t.Fatalf("the read of X under the held %s: %v", leg, err)
				}
			}

			if leg == "write" {
				served := len(r.missing) == 0 && len(r.held) == 1 && r.held[0].Version == c.v
				if !served && r.asOf.Compare(c.v) >= 0 {
					t.Fatalf("the read of X during its write at %+v served missing %+v, held %+v and asOf %+v: want the write served or asOf below its version", c.v, r.missing, r.held, r.asOf)
				}
				return
			}

			h.Settle(x, c.v)
			v, err := f.J.Draw()
			stub(t, "Draw", err)
			if err != nil {
				t.Fatal(err)
			}
			err = h.Fill(t.Context(), x, 0, pattern("x-fill", 4096), r.asOf, v)
			stub(t, "Fill", err)
			if !errors.Is(err, journal.ErrStaleFill) {
				t.Fatalf("a Fill of X at the asOf %+v a read reported during its truncate at %+v returned %v, want ErrStaleFill", r.asOf, c.v, err)
			}
		})
	}
}

// settleDelete deletes file id and settles the delete, so the journal drops
// its entry and raises its floor bucket.
func settleDelete(t *testing.T, h *journal.Handle, id journal.FileID) {
	t.Helper()

	d, err := h.Delete(t.Context(), id)
	stub(t, "Delete", err)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	h.Settle(id, d)
}
