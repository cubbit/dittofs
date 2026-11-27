package wire

import (
	"errors"
)

var (
	errFrameShort   = errors.New("smb: a Direct TCP header shorter than 4 bytes")
	errFrameZero    = errors.New("smb: a Direct TCP header whose first byte is not zero")
	errFrameTooLong = errors.New("smb: a message past the 8 MiB and 4 KiB cap")
)

// FrameLen reads a Direct TCP header (MS-SMB2 2.1): a zero byte, then the
// length of the message that follows in three big-endian bytes, the header
// not counted. It refuses a header shorter than 4 bytes, a first byte that is
// not zero and a length past MaxRecord, each of which closes the connection
// before anything is reserved for the message (RFC 20 §5.2, RFC 22 §3.3).
func FrameLen(hdr []byte) (int, error) {
	if len(hdr) < FrameHeaderLen {
		return 0, errFrameShort
	}
	// decision: a first byte of 0x85, a NetBIOS session keepalive MS-SMB2
	// 2.1 allows on the same stream, is refused here like any other nonzero
	// byte, and the connection closes. Answering it with a skip, as v1's
	// reader did, is a change to the record contract, not this package's; it
	// comes with the reader if ever a capture shows one. Withdraw this
	// marker and skip it inside the record loop if that day comes.
	if hdr[0] != 0 {
		return 0, errFrameZero
	}
	n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n > MaxRecord {
		return 0, errFrameTooLong
	}

	return n, nil
}

// AppendFrame appends to dst the Direct TCP header of a message of n bytes
// and returns it. n past MaxRecord, or negative, is refused with the same
// error FrameLen answers that length with: the writer states a length the
// reader would refuse, so it is not written at all. The refusal is not a
// panic because remote input reaches this without a bug anywhere: a record
// of bare 64-byte headers, each answered by the 64+9-byte error reply that
// pads to 80, carries about 131k members whose replies come to about
// 10.5 MB, past the cap, so the writer of a flooded record meets this.
// The stage above caps what a record's answers may come to before it; this
// is the floor under that cap.
func AppendFrame(dst []byte, n int) ([]byte, error) {
	if n < 0 || n > MaxRecord {
		return dst, errFrameTooLong
	}
	return append(dst, 0, byte(n>>16), byte(n>>8), byte(n)), nil
}
