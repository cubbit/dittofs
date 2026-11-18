package wire

import (
	"encoding/binary"
	"fmt"
)

// Decoder reads XDR (RFC 4506) from B, at N: the runtime the generated
// decoders call (RFC 20 section 5.1). Its first refusal stays in Err, and every
// read after it reads nothing and returns the zero value, so a generated
// decoder checks Err once, at its end.
type Decoder struct {
	B   []byte
	N   int
	Err error
}

func (d *Decoder) fail(err error) {
	if d.Err == nil {
		d.Err = err
	}
}

// take returns the next n bytes as a slice of B and skips their padding to
// four, refusing them, before it reads, unless both lie inside B (RFC 4506
// sections 4.9 and 4.10).
//
// decision: the padding must be there, but its bytes are not checked for zero:
// RFC 4506 has a sender write zeros, and the RPC header's reader accepts any,
// as this does. Overturned if a client is ever seen to depend on a refusal.
func (d *Decoder) take(n int) ([]byte, bool) {
	if d.Err != nil {
		return nil, false
	}
	end := d.N + n
	if end+(4-n%4)%4 > len(d.B) {
		d.fail(fmt.Errorf("nfs/wire: %w: %d bytes and their padding at %d", errPastRecord, n, d.N))
		return nil, false
	}
	b := d.B[d.N:end:end]
	d.N = end + (4-n%4)%4
	return b, true
}

// Uint32 reads an unsigned integer or an enum (RFC 4506 sections 4.2, 4.3).
func (d *Decoder) Uint32() uint32 {
	b, ok := d.take(4)
	if !ok {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

// Uint64 reads an unsigned hyper integer, or a hyper's two's complement
// (RFC 4506 section 4.5).
func (d *Decoder) Uint64() uint64 {
	b, ok := d.take(8)
	if !ok {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

// Bool reads a boolean, refusing a value other than 0 and 1, which no bool
// is (RFC 4506 section 4.4); optional data's flag is one (section 4.19).
func (d *Decoder) Bool() bool {
	switch v := d.Uint32(); v {
	case 0:
		return false
	case 1:
		return true
	default:
		d.fail(fmt.Errorf("nfs/wire: a boolean of %d", v))
		return false
	}
}

// Fixed reads a fixed-length opaque into dst, its length dst's, with no length
// word before it (RFC 4506 section 4.9).
func (d *Decoder) Fixed(dst []byte) {
	if b, ok := d.take(len(dst)); ok {
		copy(dst, b)
	}
}

// Opaque reads a variable-length opaque of at most max bytes, its length
// checked against max and the bytes left before anything is read, and returns
// it as a slice of B, never a copy (RFC 4506 section 4.10, RFC 20 section 5.3).
func (d *Decoder) Opaque(max int) []byte {
	n := d.Uint32()
	if d.Err != nil {
		return nil
	}
	if uint64(n) > uint64(max) {
		d.fail(fmt.Errorf("nfs/wire: %w: %d bytes, past %d", errTooLong, n, max))
		return nil
	}
	b, _ := d.take(int(n))
	return b
}

// String reads a string of at most max bytes as Opaque does; the string is a
// copy, a slice's bytes being the record's (RFC 4506 section 4.11).
func (d *Decoder) String(max int) string {
	return string(d.Opaque(max))
}

// Count reads a variable-length array's count, refusing one past max or one
// whose elements, each at least min bytes, cannot fit in the bytes left, so
// the array is never made before its bytes are there (RFC 4506 section 4.13,
// RFC 20 section 5.2).
func (d *Decoder) Count(max, min int) int {
	n := d.Uint32()
	if d.Err != nil {
		return 0
	}
	if uint64(n) > uint64(max) {
		d.fail(fmt.Errorf("nfs/wire: %w: a count of %d, past %d", errTooLong, n, max))
		return 0
	}
	if min > 0 && uint64(n) > uint64((len(d.B)-d.N)/min) {
		d.fail(fmt.Errorf("nfs/wire: %w: a count of %d, of %d bytes each", errPastRecord, n, min))
		return 0
	}
	return int(n)
}

// NoArm refuses a union's discriminant that selects no arm at the version
// decoded, where the union has no default (RFC 4506 section 4.15).
func (d *Decoder) NoArm(disc uint32) {
	d.fail(fmt.Errorf("nfs/wire: no arm for discriminant %d", disc))
}

// Encoder appends XDR to B: the runtime the generated encoders call. Its first
// refusal stays in Err, and the generated encoder then leaves its output as it
// found it.
type Encoder struct {
	B   []byte
	Err error
}

func (e *Encoder) fail(err error) {
	if e.Err == nil {
		e.Err = err
	}
}

func (e *Encoder) pad(n int) {
	for range (4 - n%4) % 4 {
		e.B = append(e.B, 0)
	}
}

// Uint32 writes an unsigned integer or an enum.
func (e *Encoder) Uint32(v uint32) { e.B = binary.BigEndian.AppendUint32(e.B, v) }

// Uint64 writes an unsigned hyper integer, or a hyper's two's complement.
func (e *Encoder) Uint64(v uint64) { e.B = binary.BigEndian.AppendUint64(e.B, v) }

// Bool writes a boolean, 1 or 0.
func (e *Encoder) Bool(v bool) {
	if v {
		e.Uint32(1)
		return
	}
	e.Uint32(0)
}

// Fixed writes a fixed-length opaque and its padding, with no length word.
func (e *Encoder) Fixed(b []byte) {
	e.B = append(e.B, b...)
	e.pad(len(b))
}

// Opaque writes a variable-length opaque, its length and its padding,
// refusing one past max, which the peer's decoder would refuse.
func (e *Encoder) Opaque(b []byte, max int) {
	if len(b) > max {
		e.fail(fmt.Errorf("nfs/wire: %w: %d bytes, past %d", errTooLong, len(b), max))
		return
	}
	e.Uint32(uint32(len(b)))
	e.Fixed(b)
}

// String writes a string as Opaque writes its bytes.
func (e *Encoder) String(s string, max int) {
	if len(s) > max {
		e.fail(fmt.Errorf("nfs/wire: %w: %d bytes, past %d", errTooLong, len(s), max))
		return
	}
	e.Uint32(uint32(len(s)))
	e.B = append(e.B, s...)
	e.pad(len(s))
}

// Count writes a variable-length array's count, refusing one past max.
func (e *Encoder) Count(n, max int) {
	if n > max {
		e.fail(fmt.Errorf("nfs/wire: %w: a count of %d, past %d", errTooLong, n, max))
		return
	}
	e.Uint32(uint32(n))
}

// NoArm refuses a union's discriminant that selects no arm at the version
// encoded.
func (e *Encoder) NoArm(disc uint32) {
	e.fail(fmt.Errorf("nfs/wire: no arm for discriminant %d", disc))
}
