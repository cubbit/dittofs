package datapath

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// fuzzFiles are the files an input works on, spread over the core's two
// streams so an input reaches both whichever files it names. Three of the
// four carry a real tag's bytes in their own ids — F1's first byte is B's
// tag, F2's is A's, F4's last byte is B's — so resolving a share by reading
// a tag out of the FileID names the wrong share for a file the handle alone
// must decide; F3 carries none, and serves the stream spread as any id the
// streams differ on does.
func fuzzFiles(t *testing.T, c *Core) []core.FileID {
	t.Helper()
	files := []core.FileID{
		fileWith(byte(tagB), 0x01), fileWith(byte(tagA), 0x02),
		fileWith(0x21, 0x21), fileWith(0x23, 0x22),
	}
	if c.Streams().For(files[0]) == c.Streams().For(files[2]) {
		t.Fatal("the tag-carrying ids sit on one stream; they no longer span both")
	}
	return files
}

// FuzzTagClaims drives the two handles' writes and read-side checks over the
// four files and checks every call's outcome against an oracle kept here: a
// file's owner is the tag of the first write of its that served and claimed,
// set once and never unset — no operation here clears a tag a write claimed.
// A write serves only when nobody owns the file or this handle's tag does,
// serving an ErrOtherShare that names both tags otherwise. A read-side check
// follows the same rule and claims nothing, so the oracle answers it from
// what the writes before it claimed. A zero-length write claims nothing too —
// the first write of a file must carry bytes to claim it — so the oracle
// answers it without setting an owner, and the other handle's next write of
// the file still claims it.
//
// The input is a flat sequence of 4-byte operations: byte 0's low bit picks
// the handle, its next bit the operation and, on a write, the bit after that
// the zero-length shape; byte 2 picks the file and byte 3 the payload's first
// byte, so a mutation of any byte changes something the oracle answers.
//
// Every input builds a fresh core and a fresh oracle. The fixture costs a
// directory, a segment pool and the wait for the pool's spares — more than an
// input's dozen operations — but a core shared across the run would carry
// claims from earlier inputs that the fresh map of a later one does not
// know, and seeding the oracle from the index would test the index against
// itself. A fresh core per input keeps the oracle a plain map.
func FuzzTagClaims(f *testing.F) {
	for _, seed := range tagSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		c, seam := newTestCore(t)
		a, b := twoHandles(t, c)
		files := fuzzFiles(t, c)
		var handles = [2]*Share{a, b}

		// owners is the oracle: the tag that owns each file, zero when none
		// does. Both handles' tags are nonzero, so a zero entry means unowned.
		var owners [4]core.ShareTag

		var ops strings.Builder
		for pos, op := 0, 0; pos+3 < len(data); pos, op = pos+4, op+1 {
			h := int(data[pos] & 0x1)
			file := int(data[pos+2]) % len(files)
			owner := owners[file]
			fmt.Fprintf(&ops, "op %d: handle %d (tag %d) file %d (owner %d)", op, h, handles[h].Tag, file, owner)
			switch {
			case data[pos]&0x2 != 0:
				ops.WriteString(": a read-side check\n")
				err := c.checkTag(handles[h], files[file])
				if owner != 0 && owner != handles[h].Tag {
					wantOtherShare(t, err, files[file], handles[h].Tag, owner)
				} else if err != nil {
					t.Fatalf("%sthe check served %v, want nil", ops.String(), err)
				}
			default:
				// A write: zero-length when the byte's 0x4 bit is set. Both
				// shapes name the file all the same, so both serve or refuse
				// together; only the claim differs, for a zero-length write
				// makes none.
				var payload []byte
				if data[pos]&0x4 == 0 {
					payload = []byte{data[pos+3], 'x', 'y', 'z'}
				}
				fmt.Fprintf(&ops, ": a write of %q\n", payload)
				before := takeTagState(t, c, seam, files[file])
				v, err := c.WriteAt(context.Background(), handles[h], files[file], 0, payload, 1)
				if owner != 0 && owner != handles[h].Tag {
					wantOtherShare(t, err, files[file], handles[h].Tag, owner)
					if v != (core.Version{}) {
						t.Fatalf("%sthe refused write returned version %v", ops.String(), v)
					}
					fuzzAssertUnchanged(t, c, seam, files[file], before, ops.String())
				} else if err != nil {
					t.Fatalf("%sthe write served %v, want nil", ops.String(), err)
				} else if len(payload) > 0 {
					if v == (core.Version{}) {
						t.Fatalf("%sthe write served nil error but drew no version", ops.String())
					}
					if owner == 0 {
						owners[file] = handles[h].Tag
					}
				} else {
					// The zero-length write served clean and claimed nothing:
					// the zero version, no version drawn, no owner set.
					if v != (core.Version{}) {
						t.Fatalf("%sthe zero-length write returned version %v, want the zero version", ops.String(), v)
					}
					fuzzAssertUnchanged(t, c, seam, files[file], before, ops.String())
				}
			}
		}
	})
}

// tagState is the state around a call that a refusal must leave standing:
// the counters' next issues, the written bytes, the held/dirty/retained
// accounting, the index entries of the file and the number of its records on
// disk. Taking it draws one sequence number, which the unchanged assertion
// accounts for: it expects the next seq one past this snapshot's own draw.
type tagState struct {
	nextSeq               core.Seq
	nextVersion           uint64
	written               uint64
	held, dirty, retained int64
	entries               int
	records               int
}

// takeTagState takes the state snapshot for f, counting only its records on
// disk: a pass the durability worker made beside this input moves no other
// file's records into the count.
func takeTagState(t *testing.T, c *Core, seam *kit.Seam, f core.FileID) tagState {
	t.Helper()
	seq, err := c.counters.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	s := tagState{
		nextSeq:     seq,
		nextVersion: c.counters.NextVersion().Counter,
		written:     c.WrittenBytes(),
		entries:     len(c.Index().FileEntries(f)),
	}
	s.held, s.dirty, s.retained = c.heldBytes()
	for _, rec := range readRecords(t, seam) {
		if rec.Header.File == f {
			s.records++
		}
	}
	return s
}

// fuzzAssertUnchanged asserts that the refused write left everything the
// snapshot took as it was: the counters' next issues stand — the refused
// write drew no version, and the next seq is one past the snapshot's own
// draw — no written byte moved, the held/dirty/retained accounting stands,
// the file's index entries stand and no record of the file reached the disk.
func fuzzAssertUnchanged(t *testing.T, c *Core, seam *kit.Seam, f core.FileID, before tagState, log string) {
	t.Helper()
	if got := c.counters.NextVersion().Counter; got != before.nextVersion {
		t.Fatalf("%sthe next version after the refusal = %d, want %d: a version was drawn", log, got, before.nextVersion)
	}
	if got, err := c.counters.NextSeq(); err != nil || got != before.nextSeq+1 {
		t.Fatalf("%sthe next seq after the refusal = %d, %v, want %d: the refused write drew a sequence number", log, got, err, before.nextSeq+1)
	}
	if got := c.WrittenBytes(); got != before.written {
		t.Fatalf("%sthe written bytes after the refusal = %d, want %d", log, got, before.written)
	}
	held, dirty, retained := c.heldBytes()
	if held != before.held || dirty != before.dirty || retained != before.retained {
		t.Fatalf("%sthe held/dirty/retained after the refusal = %d/%d/%d, want %d/%d/%d", log, held, dirty, retained, before.held, before.dirty, before.retained)
	}
	if got := len(c.Index().FileEntries(f)); got != before.entries {
		t.Fatalf("%sthe file holds %d entries after the refusal, want %d", log, got, before.entries)
	}
	var records int
	for _, rec := range readRecords(t, seam) {
		if rec.Header.File == f {
			records++
		}
	}
	if records != before.records {
		t.Fatalf("%sthe file holds %d records on disk after the refusal, want %d", log, records, before.records)
	}
}

// tagSeeds are the hand-written inputs, at four bytes per operation as the
// fuzz loop decodes them: byte 0 = handle bit 0x1, operation bit 0x2 and, on
// a write, zero-length bit 0x4; byte 2 = the file; byte 3 = the payload's
// first byte.
var tagSeeds = [][]byte{
	// A claims file 0; B's write and B's read-side check of it are refused.
	{0x00, 0, 0x00, 'a', 0x01, 0, 0x00, 'b', 0x03, 0, 0x00, 'b'},

	// B claims file 1; A's write and A's read-side check of it are refused.
	{0x01, 0, 0x01, 'b', 0x00, 0, 0x01, 'a', 0x02, 0, 0x01, 'a'},

	// B's read-side check of unowned file 2 claims nothing, so A's write of
	// it serves and claims it; B's check of it is refused after.
	{0x03, 0, 0x02, 'x', 0x00, 0, 0x02, 'a', 0x03, 0, 0x02, 'x'},

	// Both handles' read-side checks of unowned file 3 claim nothing, so
	// A's write of it serves and claims it.
	{0x02, 0, 0x03, 'x', 0x03, 0, 0x03, 'x', 0x00, 0, 0x03, 'a', 0x03, 0, 0x03, 'x'},

	// A's zero-length write of unowned file 0 claims nothing, so B's write of
	// it serves and claims it, and A's zero-length write of B's file is then
	// refused — the wrong handle never reaches the shape's clean return.
	{0x04, 0, 0x00, 'a', 0x01, 0, 0x00, 'b', 0x04, 0, 0x00, 'a'},

	// Both handles write every file: each file's first writer claims it and
	// every other handle's write of it is refused. Eight operations, all four
	// files on both handles.
	{0x00, 0, 0x00, 'a', 0x01, 0, 0x00, 'b',
		0x00, 0, 0x01, 'a', 0x01, 0, 0x01, 'b',
		0x00, 0, 0x02, 'a', 0x01, 0, 0x02, 'b',
		0x00, 0, 0x03, 'a', 0x01, 0, 0x03, 'b'},
}
