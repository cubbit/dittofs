package wire

import (
	"errors"
	"fmt"
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
	if hdr[0] != 0 {
		return 0, errFrameZero
	}
	n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n > MaxRecord {
		return 0, errFrameTooLong
	}

	return n, nil
}

// AppendFrame appends to dst the Direct TCP header of a message of n bytes and
// returns it. n past MaxRecord or 2^24's three bytes panics: the length the
// writer states is what the reader refuses, a program bug the same mark on
// the wire could never be, so it is not written at all.
func AppendFrame(dst []byte, n int) []byte {
	if n < 0 || n > MaxRecord {
		panic(fmt.Sprintf("smb: a frame of %d bytes, past %d", n, MaxRecord))
	}
	return append(dst, 0, byte(n>>16), byte(n>>8), byte(n))
}
