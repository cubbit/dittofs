package walk_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
	"github.com/cubbit/dittofs/pkg/block/journal/walk"
)

var (
	identity = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	file     = [16]byte{0xAA, 0xBB}
)

// segment builds a segment file: its header, one write record of payload,
// then the tail the caller asks for, zero-padded to size.
func segment(t *testing.T, size int, payload []byte, tail func(next uint64) []byte) string {
	t.Helper()
	const id = 7
	b := raw.SegmentHeader{Identity: identity, ID: id}.Bytes()
	rec := raw.Record{
		Identity: identity, Kind: 1, Segment: id, FileID: file,
		Offset: 0, Length: uint64(len(payload)), Seq: 1,
		Version: raw.Version{Counter: 3}, Tag: 1,
		RecordOffset: uint32(len(b)),
		Write:        &raw.WriteBody{MTime: 5, Payload: payload},
	}.Bytes()
	b = append(b, rec...)
	if tail != nil {
		b = append(b, tail(uint64(len(b)))...)
	}
	b = append(b, make([]byte, size-len(b))...)
	path := filepath.Join(t.TempDir(), "00000000000000000007.seg")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFileReadsAVerifiedWriteAndItsPayload(t *testing.T) {
	path := segment(t, 4096, []byte("hello world"), nil)
	seg, err := walk.File(path, identity)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(seg.Records) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(seg.Records), seg.Records)
	}
	r := seg.Records[0]
	if r.Kind != "write" || r.Len != 11 || r.Counter != 3 || r.Seq != 1 || r.File != file {
		t.Errorf("record = %+v, want a write of 11 bytes at v3 seq 1 for the planted file", r)
	}
	if !bytes.HasSuffix(r.Body, []byte("hello world")) {
		t.Errorf("body %q does not end in the planted payload", r.Body)
	}
	if seg.Tail != walk.Untaken {
		t.Errorf("tail = %v, want untaken (a zero tail)", seg.Tail)
	}
}

func TestFileTellsTheThreeTailsApart(t *testing.T) {
	sealed := segment(t, 4096, []byte("x"), func(next uint64) []byte {
		return raw.SealMarker{Identity: identity, Segment: 7, Offset: next}.Bytes()
	})
	if seg, err := walk.File(sealed, identity); err != nil || seg.Tail != walk.Sealed {
		t.Errorf("sealed segment: tail %v err %v, want sealed", seg.Tail, err)
	}

	torn := segment(t, 4096, []byte("x"), func(uint64) []byte { return []byte("garbage that is no record") })
	if seg, err := walk.File(torn, identity); err != nil || seg.Tail != walk.Torn {
		t.Errorf("garbage tail: tail %v err %v, want torn", seg.Tail, err)
	}
}

func TestFileRefusesARecordWhoseBodyDoesNotVerify(t *testing.T) {
	path := segment(t, 4096, []byte("hello world"), nil)
	b, _ := os.ReadFile(path)
	b[48+96+8+2] ^= 0xFF // a payload byte: the header still verifies, the body does not
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	seg, err := walk.File(path, identity)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(seg.Records) != 0 {
		t.Errorf("a record with a flipped payload byte was shown: %+v", seg.Records)
	}
	if seg.Tail != walk.Torn {
		t.Errorf("tail = %v, want torn (the bad bytes are not a clean tail)", seg.Tail)
	}
}

func TestFileSpareAndForeignJournal(t *testing.T) {
	spare := filepath.Join(t.TempDir(), "00000000000000000001.seg")
	if err := os.WriteFile(spare, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if seg, err := walk.File(spare, identity); err != nil || !seg.Spare {
		t.Errorf("zero file: spare %v err %v, want a spare", seg.Spare, err)
	}

	other := [16]byte{9}
	path := segment(t, 4096, []byte("x"), nil)
	if _, err := walk.File(path, other); err == nil {
		t.Errorf("another journal's segment read without an error")
	}
}
