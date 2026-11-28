package protocol

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

// A record written is read back as it was, and bytes of another kind of
// record, or none, are refused as corrupt rather than read as NFSv4's.
func TestRecordRoundTripAndKind(t *testing.T) {
	want := wire.ClientRec{
		Kind: wire.ClientRecKindNFS4,
		NFS4: wire.ClientRecNFS4{
			Minor:       1,
			SessionSeq:  7,
			MustEnforce: []uint32{10},
			MustAllow:   []uint32{20, 40},
		},
	}

	b, err := WriteRecord(want)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ReadRecord(3, b)
	if err != nil {
		t.Fatal(err)
	}

	if got.NFS4.SessionSeq != 7 || got.NFS4.Minor != 1 || len(got.NFS4.MustAllow) != 2 {
		t.Errorf("read %+v, want %+v", got, want)
	}

	nlm, err := wire.AppendClientRec(nil, &wire.ClientRec{
		Kind: wire.ClientRecKindNLM,
		NLM:  wire.ClientRecNLM{Host: []byte{1, 2, 3, 4}, Server: []byte{5, 6, 7, 8}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ReadRecord(3, nlm); !errors.Is(err, wire.ErrRecCorrupt) {
		t.Errorf("a record of another kind read as %v, want ErrRecCorrupt", err)
	}

	if _, err := ReadRecord(3, nil); !errors.Is(err, wire.ErrRecTruncated) {
		t.Errorf("no bytes read as %v, want ErrRecTruncated", err)
	}
}

// SequenceOf reads the low four bytes.
func TestSequenceOfReadsTheLowHalf(t *testing.T) {
	if got := SequenceOf([8]byte{9, 9, 9, 9, 0, 0, 1, 2}); got != 258 {
		t.Errorf("SequenceOf = %d, want 258", got)
	}
}
