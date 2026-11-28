package provider

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// recordVersion1 is the key record codec's one version.
const recordVersion1 byte = 1

// errKeyRecord is bytes that are not a key record this codec reads.
var errKeyRecord = errors.New("provider: key record malformed")

// keyRecord is the part of a key record the provider's codec holds; its kind,
// ID and state travel beside it, in the Row. masterKeyID is empty, and key is
// the key itself, for a key held in the clear; otherwise key is the key's
// stored wrap. A destroyed record keeps its fingerprint and since only.
type keyRecord struct {
	fingerprint [32]byte
	since       int64 // Unix nanoseconds at which the record entered its state
	masterKeyID []byte
	key         []byte
}

// encode is r as version 1: version (1 byte) ‖ fingerprint (32 bytes) ‖ since
// (8 bytes, big-endian) ‖ master key ID ‖ key, the last two each after a
// 4-byte big-endian length.
func (r keyRecord) encode() []byte {
	b := make([]byte, 0, 1+32+8+4+len(r.masterKeyID)+4+len(r.key))
	b = append(b, recordVersion1)
	b = append(b, r.fingerprint[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(r.since))
	b = lenPrefixed(b, r.masterKeyID)
	return lenPrefixed(b, r.key)
}

// decodeKeyRecord reads b as a key record, refusing an unknown version, a
// length that runs past b, and bytes after the key. The fields returned share
// b's bytes.
func decodeKeyRecord(b []byte) (keyRecord, error) {
	if len(b) == 0 {
		return keyRecord{}, fmt.Errorf("%w: empty", errKeyRecord)
	}
	if b[0] != recordVersion1 {
		return keyRecord{}, fmt.Errorf("%w: version %d", errKeyRecord, b[0])
	}
	if len(b) < 1+32+8 {
		return keyRecord{}, fmt.Errorf("%w: %d bytes, too short for its header", errKeyRecord, len(b))
	}

	var r keyRecord
	copy(r.fingerprint[:], b[1:33])
	r.since = int64(binary.BigEndian.Uint64(b[33:41]))
	rest := b[41:]

	var err error
	if r.masterKeyID, rest, err = field(rest); err != nil {
		return keyRecord{}, err
	}
	if r.key, rest, err = field(rest); err != nil {
		return keyRecord{}, err
	}
	if len(rest) != 0 {
		return keyRecord{}, fmt.Errorf("%w: %d bytes after the key", errKeyRecord, len(rest))
	}

	return r, nil
}

// field reads one 4-byte length-prefixed field off b, returning it and what
// follows it.
func field(b []byte) (f, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, fmt.Errorf("%w: %d bytes, too short for a field's length", errKeyRecord, len(b))
	}
	n := uint64(binary.BigEndian.Uint32(b))
	b = b[4:]
	if uint64(len(b)) < n {
		return nil, nil, fmt.Errorf("%w: field of %d bytes runs past its %d", errKeyRecord, n, len(b))
	}
	return b[:n:n], b[n:], nil
}

// record is the version 1 bytes of a key record entering its state now, by
// the provider's clock: masterKeyID empty and key the key itself for a key
// held in the clear, or the key's stored wrap.
func (p *Provider) record(fp [32]byte, masterKeyID, key []byte) []byte {
	return keyRecord{fingerprint: fp, since: p.now().UnixNano(), masterKeyID: masterKeyID, key: key}.encode()
}
