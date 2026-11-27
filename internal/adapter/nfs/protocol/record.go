package protocol

import (
	"encoding/binary"
	"fmt"

	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// ReadRecord is the NFSv4 record the adapter bytes b of client id hold: the
// ops files touch no bytes, so the codec of the record is called from here.
func ReadRecord(id vfs.ClientID, b []byte) (wire.ClientRec, error) {
	rec, err := wire.DecodeClientRec(b)
	if err != nil {
		return wire.ClientRec{}, fmt.Errorf("decode the client record of %d: %w", id, err)
	}

	if rec.Kind != wire.ClientRecKindNFS4 {
		return wire.ClientRec{}, fmt.Errorf(
			"client %d: %w: kind %d",
			id,
			wire.ErrRecCorrupt,
			rec.Kind,
		)
	}

	return rec, nil
}

// WriteRecord is the adapter bytes of rec.
func WriteRecord(rec wire.ClientRec) ([]byte, error) {
	b, err := wire.AppendClientRec(nil, &rec)
	if err != nil {
		return nil, fmt.Errorf("encode the client record: %w", err)
	}

	return b, nil
}

// SequenceOf is the low half of a confirm read as a big-endian number: the
// sequence ID a client answers its CREATE_SESSION with.
func SequenceOf(confirm [8]byte) uint32 {
	return binary.BigEndian.Uint32(confirm[4:])
}
