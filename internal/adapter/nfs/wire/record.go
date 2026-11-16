package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// MaxFragments is the most fragments one record comes in (RFC 21 section 2.3).
const MaxFragments = 64

// MaxRecord is the most a record may be: the transfer class's capacity, 1 MiB
// and its 4 KiB of header room, its fragments together (RFC 21 section 2.3).
const MaxRecord = 1<<20 + 4<<10

// RecordMarkLen is the record mark's size, and lastFragment its last-fragment
// bit (RFC 5531 section 11).
const (
	RecordMarkLen = 4
	lastFragment  = 1 << 31
)

var (
	errShortMark = errors.New("a mark shorter than its four bytes")
	errPastCap   = errors.New("a fragment past the cap")
)

// Fragment reads a record mark: the length of the fragment's body and whether
// it ends the record. A mark shorter than four bytes is refused, and so is a
// length past MaxRecord, past which the connection closes (RFC 20 section 5.2).
func Fragment(mark []byte) (n int, last bool, err error) {
	if len(mark) < RecordMarkLen {
		return 0, false, fmt.Errorf("nfs/wire: %w: %d bytes", errShortMark, len(mark))
	}
	m := binary.BigEndian.Uint32(mark)
	n, last = int(m&^lastFragment), m&lastFragment != 0
	if n > MaxRecord {
		return 0, false, fmt.Errorf("nfs/wire: %w: %d bytes, past %d", errPastCap, n, MaxRecord)
	}
	return n, last, nil
}

// AppendRecordMark appends a record mark for a body of n bytes, its
// last-fragment bit set, and returns the record.
func AppendRecordMark(dst []byte, n int) []byte {
	return binary.BigEndian.AppendUint32(dst, lastFragment|uint32(n))
}
