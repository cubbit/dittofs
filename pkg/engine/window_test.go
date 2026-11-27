package engine_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// windowFiles builds the three files the fuzz target writes, over the real
// journal with the fuzz segment cap: files 0 and 1 share one stream, file 2
// holds the other apart, as fuzzFileIDs put them.
func windowFiles(t *testing.T) (*enginetest.Fixture, [fuzzFiles]entity.FileID) {
	t.Helper()

	f := enginetest.New(t, enginetest.WithSegmentCap(fuzzSegmentCap))

	var ids [fuzzFiles]entity.FileID
	for i := range fuzzFileIDs {
		nf := kvstore.Regular(0o644)
		nf.ID = fuzzFileIDs[i]

		_, file := f.FS.Create(t, f.FS.Admin, f.FS.Root, fmt.Sprintf("f%d", i), nf)
		ids[i] = file.ID
	}

	return f, ids
}

// windowWrite writes length bytes of pattern at off, and answers the write's
// verifier. One with a nil Done is what a client writes outside the fuzz
// world's accounting.
func windowWrite(t *testing.T, f *enginetest.Fixture, id entity.FileID, off int64, pattern byte, length int) vfserr.Verifier {
	t.Helper()

	v, err := f.E.Write(t.Context(), f.Share.ID, id, off, bytes.Repeat([]byte{pattern}, length), engine.WriteOpts{})
	if err != nil {
		t.Fatalf("write %d bytes at %d: %v", length, off, err)
	}

	return v
}

// windowCommit commits the whole file, and answers the verifier and the loss
// sequence.
func windowCommit(t *testing.T, f *enginetest.Fixture, id entity.FileID) (vfserr.Verifier, uint64) {
	t.Helper()

	v, seq, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	return v, seq
}

// windowRead reads length bytes at off, or answers the error class of the
// read's failure.
func windowRead(t *testing.T, f *enginetest.Fixture, id entity.FileID, off int64, length int) ([]byte, string) {
	t.Helper()

	buf := make([]byte, length)
	n, err := f.E.Read(t.Context(), f.Share.ID, id, off, buf)
	if err != nil {
		return nil, errorClass(err)
	}

	return buf[:n], ""
}

// TestWindowOneStreamAlone holds the fixture's FailNextWindow to its decided
// semantics: it fails exactly the one stream's sync that was written to last,
// the armed stream's unsynced write alone reads lost and falls back to what it
// superseded, the other stream's unsynced write reads back all along, the
// committing file's loss sequence rises once, its next Commit answers a new
// verifier, and the write made after the loss commits under its own verifier.
// It is what the fail_next_window operation's model encodes, and what the
// scope's escape rule for the dropped later write watches.
func TestWindowOneStreamAlone(t *testing.T) {
	f, ids := windowFiles(t)

	// The first write of each stream, committed, so both streams hold a
	// synced segment no later step refills a spare for.
	windowWrite(t, f, ids[0], 0, 'a', 4096)
	cv0, seq0 := windowCommit(t, f, ids[0])
	if seq0 != 0 {
		t.Fatalf("the first commit answers a loss sequence of %d, want 0", seq0)
	}

	windowWrite(t, f, ids[2], 0, 'b', 4096)
	_, seqb := windowCommit(t, f, ids[2])
	if seqb != 0 {
		t.Fatalf("the other stream's commit answers a loss sequence of %d, want 0", seqb)
	}

	// An unsynced write on each stream: file 2's stays alive throughout. Its
	// write comes first, so the last written segment is file 0's and the window
	// fails file 0's stream, the one the checks below pin.
	windowWrite(t, f, ids[2], 4096, 'y', 512)
	vx := windowWrite(t, f, ids[0], 0, 'x', 512)

	f.FailNextWindow()

	// The armed stream's write alone reads lost; the first read answers the
	// loss itself.
	if got, class := windowRead(t, f, ids[0], 0, 8192); class != "lost" {
		t.Fatalf("the armed stream's read answered %s, want lost: %s", class, describe(0, string(got)))
	}

	wantB := append(bytes.Repeat([]byte{'b'}, 4096), bytes.Repeat([]byte{'y'}, 512)...)
	if got, class := windowRead(t, f, ids[2], 0, 8192); class != "" || !bytes.Equal(got, wantB) {
		gotView := fmt.Sprintf("%s (class %q)", describe(0, string(got)), class)
		t.Fatalf("the untouched stream's read answered %s, want its written 4096 bytes of 'b'", gotView)
	}

	if got, class := windowRead(t, f, ids[0], 0, 8192); class != "" || !bytes.Equal(got, bytes.Repeat([]byte{'a'}, 4096)) {
		gotView := fmt.Sprintf("%s (class %q)", describe(0, string(got)), class)
		t.Fatalf("after the loss the armed stream's file reads %s, want the 4096 bytes its synced write left", gotView)
	}

	// The committing file is reported once, with a new verifier.
	cv1, seq1 := windowCommit(t, f, ids[0])
	if seq1 == 0 || seq1 != seq0+1 {
		t.Errorf("the commit over the loss answers a loss sequence of %d, want exactly one raise, from %d", seq1, seq0)
	}
	if cv1 == cv0 || cv1 == vx {
		t.Errorf("the commit over the loss answers the verifier %x, want another than the previous %x and the lost write's %x", cv1, cv0, vx)
	}

	// The untouched stream's file commits its still alive write clean. The
	// loss record names only its own file, so the untouched file's loss
	// sequence stays zero, while the loss raised the journal's generation, a
	// verifier input the whole share shares, so its commit answers the new
	// verifier the loss made.
	cbv2, seqb2 := windowCommit(t, f, ids[2])
	if seqb2 != 0 {
		t.Errorf("the untouched stream's commit answers a loss sequence of %d, want 0", seqb2)
	}
	if cbv2 != cv1 {
		t.Errorf("the untouched stream's commit answers %x, want the post-loss %x", cbv2, cv1)
	}

	// The write made after the loss commits under its own verifier.
	vc := windowWrite(t, f, ids[0], 0, 'c', 512)
	cw, seqw := windowCommit(t, f, ids[0])
	if seqw != seq1 {
		t.Errorf("the commit of the write made after the loss answers a loss sequence of %d, want the same %d, no second report", seqw, seq1)
	}
	if cw != vc {
		t.Errorf("the commit of the write made after the loss answers %x, want the write's own %x", cw, vc)
	}

	if got, class := windowRead(t, f, ids[0], 0, 8192); class != "" || !bytes.HasPrefix(got, bytes.Repeat([]byte{'c'}, 512)) || len(got) != 4096 {
		gotView := fmt.Sprintf("%s (class %q)", describe(0, string(got)), class)
		t.Fatalf("the write made after the loss reads %s, want its 512 bytes over the 4096", gotView)
	}
}
