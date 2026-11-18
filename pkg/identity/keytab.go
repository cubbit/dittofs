package identity

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The encryption types a keytab may hold (RFC 3962).
const (
	etypeAES128 = 17 // aes128-cts-hmac-sha1-96
	etypeAES256 = 18 // aes256-cts-hmac-sha1-96
)

// CheckKeytab refuses, with vfserr.ErrInvalid, a keytab that does not parse
// as MIT's version 2 format, holds no key, or holds any key that is not
// aes128-cts-hmac-sha1-96 or aes256-cts-hmac-sha1-96.
//
// decision: one key that is not AES refuses the whole keytab, never only that
// key, so an import that carried DES or RC4 is sent back to be exported again
// rather than half kept; the identity server runs this same check on each
// keytab version it is handed and drops a refused one whole. Version 1, the
// host-byte-order format no current tool writes, is refused. Overturned if
// RFC 18 §3.3 ever admits an encryption type beside AES.
func CheckKeytab(b []byte) error {
	if len(b) < 2 || b[0] != 5 || b[1] != 2 {
		return fmt.Errorf("%w: not a version 2 keytab", vfserr.ErrInvalid)
	}
	keys := 0
	for n := 2; n < len(b); {
		if len(b)-n < 4 {
			return fmt.Errorf("%w: keytab ends inside a record length", vfserr.ErrInvalid)
		}
		l := int64(int32(binary.BigEndian.Uint32(b[n:])))
		n += 4
		if l == 0 {
			break // a zero length ends the entries, as MIT's reader reads it
		}
		size := l
		if l < 0 {
			size = -l // a hole left by a deleted entry
		}
		if size > int64(len(b)-n) {
			return fmt.Errorf("%w: keytab record of %d bytes past its end", vfserr.ErrInvalid, size)
		}
		if l > 0 {
			etype, err := entryEtype(b[n : n+int(l)])
			if err != nil {
				return fmt.Errorf("%w: keytab entry: %w", vfserr.ErrInvalid, err)
			}
			if etype != etypeAES128 && etype != etypeAES256 {
				return fmt.Errorf("%w: keytab holds a key of encryption type %d, not AES", vfserr.ErrInvalid, etype)
			}
			keys++
		}
		n += int(size)
	}
	if keys == 0 {
		return fmt.Errorf("%w: keytab holds no key", vfserr.ErrInvalid)
	}
	return nil
}

var errShort = errors.New("entry ends early")

// entryEtype reads one entry, its principal, timestamp, key version number
// and key, and returns the key's encryption type. Every length is a signed
// 16-bit field and a negative one is refused.
func entryEtype(e []byte) (uint16, error) {
	p := 0
	u16 := func() (int, error) {
		if len(e)-p < 2 {
			return 0, errShort
		}
		v := int16(binary.BigEndian.Uint16(e[p:]))
		p += 2
		if v < 0 {
			return 0, errors.New("negative count or length")
		}
		return int(v), nil
	}
	skip := func(k int) error {
		if len(e)-p < k {
			return errShort
		}
		p += k
		return nil
	}
	comps, err := u16()
	if err != nil {
		return 0, err
	}
	for range comps + 1 { // the realm, then each component
		l, err := u16()
		if err != nil {
			return 0, err
		}
		if err := skip(l); err != nil {
			return 0, err
		}
	}
	if err := skip(4 + 4 + 1); err != nil { // name type, timestamp, key version number
		return 0, err
	}
	etype, err := u16()
	if err != nil {
		return 0, err
	}
	l, err := u16()
	if err != nil {
		return 0, err
	}
	if l == 0 {
		return 0, errors.New("empty key")
	}
	if err := skip(l); err != nil {
		return 0, err
	}
	return uint16(etype), nil
}
