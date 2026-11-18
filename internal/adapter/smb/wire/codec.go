package wire

import (
	"encoding/binary"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// The runtime of SMB2's generated codec (RFC 20 §5.1): the reads and writes
// the code wire/gen writes for each tagged struct calls. A body's offsets
// count from its SMB2 header (MS-SMB2 2.2); a structure inside it, a create
// context or a context's data, counts its own from its first byte.

var (
	errBody   = errors.New("smb: a field past its structure's bytes")
	errSize   = errors.New("smb: a StructureSize other than its layout's, or a body shorter than it")
	errMax    = errors.New("smb: a length or count past its field's maximum")
	errOffset = errors.New("smb: a field located past the message")
	errBefore = errors.New("smb: a field located in the bytes of a field before it")
	errExact  = errors.New("smb: a context's data longer than its structure")
	errAlign  = errors.New("smb: an offset off the alignment its layout asks")
	errStep   = errors.New("smb: a chained structure's step that is not a multiple of 8 inside its chain")
	errUTF16  = errors.New("smb: a UTF-16 string of an odd byte length")
	errWTF8   = errors.New("smb: a string to encode that is not WTF-8")
	errWide   = errors.New("smb: a length or offset its field is too narrow for")
	errArm    = errors.New("smb: a union holding a value in another arm than the one it names")
)

// Decoder reads one structure of an SMB2 message. Every check comes before
// the read or the allocation it guards (RFC 20 §5.2); once one refuses,
// every read returns zero values and Err keeps the first refusal.
type Decoder struct {
	B    []byte // the bytes the structure lies in
	Base int    // the offset B[0] is at: HeaderLen for a body, 0 for a structure inside one
	N    int    // where the next field read in order begins, past every field read so far
	Err  error
}

func (d *Decoder) fail(err error) {
	if d.Err == nil {
		d.Err = err
	}
}

// take returns the next n bytes and moves past them.
func (d *Decoder) take(n int) []byte {
	if d.Err != nil {
		return nil
	}
	if n > len(d.B)-d.N {
		d.fail(errBody)
		return nil
	}
	b := d.B[d.N : d.N+n : d.N+n]
	d.N += n
	return b
}

func (d *Decoder) Uint8() uint8 {
	if b := d.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (d *Decoder) Uint16() uint16 {
	if b := d.take(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

func (d *Decoder) Uint32() uint32 {
	if b := d.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (d *Decoder) Uint64() uint64 {
	if b := d.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

// Bytes reads a fixed-length field into dst.
func (d *Decoder) Bytes(dst []byte) { copy(dst, d.take(len(dst))) }

// Skip passes a Reserved field, or a layout field nothing reads.
func (d *Decoder) Skip(n int) { d.take(n) }

// Size reads a StructureSize, refused unless it is want, and refuses a body
// shorter than least: want, or want less one where the variable part's first
// byte, which an odd StructureSize counts, may be missing.
//
// decision: a request carries that byte and a reply need not, as the
// reference server and its client have it. The server refuses a request
// without it, "if (SMBD_SMB2_IN_DYN_LEN(req) < min_dyn_size)", but for
// IOCTL, QUERY_INFO and WRITE (source3/smbd/smb2_server.c), as MS-SMB2
// 3.3.5.9 has a CREATE refused; it sends a CREATE reply without it, a body of
// 0x58 for a StructureSize of 0x59 (source3/smbd/smb2_create.c); its client
// writes it, "dyn_len = MAX(1, blobs_offset + blob.length)"
// (libcli/smb/smb2cli_create.c), and reads a reply by "body_size &= 0xfffe"
// (libcli/smb/smbXcli_base.c). The empty tag marks a struct that may go
// without it, a reply or one of those three requests.
func (d *Decoder) Size(want uint16, least int) {
	if d.Uint16() != want || len(d.B) < least {
		d.fail(errSize)
	}
}

// At returns the n bytes at offset off, a field located by an offset and
// length pair, as a slice of the record (RFC 20 §5.3). It is refused before
// any read when n is past limit, when off is not a multiple of align, or when
// the bytes do not lie past every field before it and inside the
// structure's. A field of no bytes is nil and its offset unread (MS-SMB2
// 2.2.13.2's DataOffset, ignored when DataLength is 0).
//
// decision: a located field lies past every field before it in the struct's
// order, not only apart from them, so a decoded value encodes back within
// its maxima: a create context's data before its name would be written after
// it, its padding then pushing a chain of them past the chain's maximum. The
// reference server asks more, a context's name at 16 and its data after it.
// Withdraw it for an order MS-SMB2 or RFC 22 §16 allows.
func (d *Decoder) At(off, n, limit, align int) []byte {
	if d.Err != nil || n == 0 {
		return nil
	}
	// A 32-bit int reads a length or offset past 2^31 as a negative, which
	// passes every limit below: refuse it as the maximum it names.
	if off < 0 || n < 0 || n > limit {
		d.fail(errMax)
		return nil
	}
	if off%align != 0 {
		d.fail(errAlign)
		return nil
	}
	at := off - d.Base
	if at < d.N {
		d.fail(errBefore)
		return nil
	}
	if at > len(d.B) || n > len(d.B)-at {
		d.fail(errOffset)
		return nil
	}
	d.N = at + n
	return d.B[at : at+n : at+n]
}

// Next returns the n bytes of the field that follows the one before it, as
// a slice of the record, refused when n is past limit or the bytes are not
// there.
func (d *Decoder) Next(n, limit int) []byte {
	if d.Err != nil || n == 0 {
		return nil
	}
	if n < 0 || n > limit {
		d.fail(errMax)
		return nil
	}
	return d.take(n)
}

// Uint16s reads n 16-bit integers in order, refused before the slice is made
// when n is past limit or the bytes are not there.
func (d *Decoder) Uint16s(n, limit int) []uint16 {
	b := d.Next(2*n, 2*limit)
	if b == nil {
		return nil
	}
	s := make([]uint16, n)
	for i := range s {
		s[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return s
}

// UTF16 returns the UTF-16LE string b holds, refused when its length is odd,
// which no string's can be.
//
// decision: a surrogate that is not half of a pair is decoded, not refused,
// since RFC 22 §12.3 has the handler answer such a name
// STATUS_OBJECT_NAME_INVALID where §16 answers a decode failure
// STATUS_INVALID_PARAMETER, and it is kept apart from U+FFFD, which a client
// may send, by writing it as WTF-8: the three bytes UTF-8 would give its code
// point, which no valid UTF-8 holds, so the handler's check is
// utf8.ValidString. The epic 6 plan's §9 proposes RFC 22 §16 state this;
// withdraw it for what §16 settles.
func (d *Decoder) UTF16(b []byte) string {
	if len(b)%2 != 0 {
		d.fail(errUTF16)
		return ""
	}
	var s strings.Builder
	s.Grow(len(b) / 2 * 3)
	for i := 0; i < len(b); i += 2 {
		r := rune(binary.LittleEndian.Uint16(b[i:]))
		if !utf16.IsSurrogate(r) {
			s.WriteRune(r)
			continue
		}
		if i+4 <= len(b) {
			if pair := utf16.DecodeRune(r, rune(binary.LittleEndian.Uint16(b[i+2:]))); pair != utf8.RuneError {
				s.WriteRune(pair)
				i += 2
				continue
			}
		}
		s.WriteByte(0xE0 | byte(r>>12))
		s.WriteByte(0x80 | byte(r>>6)&0x3F)
		s.WriteByte(0x80 | byte(r)&0x3F)
	}
	return s.String()
}

// Sub returns a Decoder over b, a structure inside this one whose offsets
// count from its own first byte: a context's data, a linked chain's member.
//
// decision: the structure need not take every byte of b, so a negotiate
// context's data longer than its struct is accepted: MS-SMB2 2.2.3.1 sizes it
// by DataLength, and its struct is what the handler reads. A create context's
// is held to its struct's size, by Exact.
func (d *Decoder) Sub(b []byte) Decoder { return Decoder{B: b, Err: d.Err} }

// Join takes back the refusal of a Decoder Sub returned.
func (d *Decoder) Join(s *Decoder) { d.Err = s.Err }

// Exact takes back the refusal of a Decoder Sub returned, and refuses a
// structure that left bytes of it unread.
//
// decision: a create context's data decoded into a struct, an arm tagged
// exact, is its struct's size, as MS-SMB2 3.3.5.9 has it: "If the size of
// each individual create context is not equal to the DataLength of the create
// context, the server MUST fail the request with STATUS_INVALID_PARAMETER."
// Section 3.3.5.4 refuses a negotiate context's data only when shorter than
// its structure, which a read past the data refuses here too.
func (d *Decoder) Exact(s *Decoder) {
	d.Join(s)
	if s.N != len(s.B) {
		d.fail(errExact)
	}
}

// Link returns the member of a linked chain that begins at at in region, and
// where the next begins (MS-SMB2 2.2.13.2's Next). A member's first four bytes
// are its step to the next, 0 on the last, which spans the rest of region. A
// step that is not a multiple of 8, or that leads to no byte of region, is
// refused.
func (d *Decoder) Link(region []byte, at int) ([]byte, int) {
	if d.Err != nil {
		return nil, len(region)
	}
	if len(region)-at < 4 {
		d.fail(errBody)
		return nil, len(region)
	}
	step := int(binary.LittleEndian.Uint32(region[at:]))
	switch {
	case step == 0:
		return region[at:], len(region)
	case step < 0 || step%8 != 0 || step >= len(region)-at:
		d.fail(errStep)
		return nil, len(region)
	}
	return region[at : at+step : at+step], at + step
}

// Chain returns where a counted chain of n members begins, at offset off,
// each member at least least bytes (MS-SMB2 2.2.3's negotiate contexts). It is
// refused before anything is made when n is past limit, when off is not a
// multiple of 8, or when the bytes from off on, past every field before it,
// cannot hold n members; it returns -1 then, and for no members.
func (d *Decoder) Chain(off, n, limit, least int) int {
	if d.Err != nil || n == 0 {
		return -1
	}
	if off < 0 || n < 0 || n > limit {
		d.fail(errMax)
		return -1
	}
	if off%8 != 0 {
		d.fail(errAlign)
		return -1
	}
	at := off - d.Base
	if at < d.N {
		d.fail(errBefore)
		return -1
	}
	if at > len(d.B) || n*least > len(d.B)-at {
		d.fail(errOffset)
		return -1
	}
	return at
}

// Member returns a Decoder over the bytes of a counted chain's member, from
// the first multiple of 8 at or past at, counted as offsets are, and the
// place it begins (MS-SMB2 2.2.3.1: each at the first 8-byte aligned offset
// after the one before).
func (d *Decoder) Member(at int) (Decoder, int) {
	at += (8 - (d.Base+at)%8) % 8
	if at > len(d.B) {
		d.fail(errBody)
	}
	if d.Err != nil {
		return Decoder{Err: d.Err}, at
	}
	return Decoder{B: d.B[at:]}, at
}

// After takes back the refusal of a Decoder Member returned, whose member
// began at start, and returns where that member ends, which every field read
// in order now follows.
func (d *Decoder) After(s *Decoder, start int) int {
	d.Err = s.Err
	end := start + s.N
	d.N = end
	return end
}

// Encoder appends one structure of an SMB2 reply. A layout field, an offset,
// a length or a count, is appended as a hole and filled once the field it
// describes is written.
type Encoder struct {
	B      []byte // the reply so far
	Origin int    // the place in B offsets count from: the header before a body, a structure's own first byte inside one
	Err    error
}

func (e *Encoder) fail(err error) {
	if e.Err == nil {
		e.Err = err
	}
}

// Pos is the place of the next byte appended.
func (e *Encoder) Pos() int { return len(e.B) }

func (e *Encoder) Uint8(v uint8)   { e.B = append(e.B, v) }
func (e *Encoder) Uint16(v uint16) { e.B = binary.LittleEndian.AppendUint16(e.B, v) }
func (e *Encoder) Uint32(v uint32) { e.B = binary.LittleEndian.AppendUint32(e.B, v) }
func (e *Encoder) Uint64(v uint64) { e.B = binary.LittleEndian.AppendUint64(e.B, v) }

// Bytes appends b, a fixed-length field or a field's bytes.
func (e *Encoder) Bytes(b []byte) { e.B = append(e.B, b...) }

// Zero appends n zero bytes: a Reserved field, or padding.
func (e *Encoder) Zero(n int) { e.B = append(e.B, make([]byte, n)...) }

// Fill pads the structure that began at start with zeros to n bytes: the
// variable part's first byte an odd StructureSize n counts, when that part
// is empty.
func (e *Encoder) Fill(start, n int) {
	if k := start + n - len(e.B); k > 0 {
		e.Zero(k)
	}
}

// Hole appends a layout field of w bytes and returns its place.
func (e *Encoder) Hole(w int) int {
	p := len(e.B)
	e.Zero(w)
	return p
}

// Begin pads to a multiple of align, counted as offsets are, and returns
// where the padding began and where the field after it begins.
func (e *Encoder) Begin(align int) (int, int) {
	begin := len(e.B)
	e.Zero((align - (begin-e.Origin)%align) % align)
	return begin, len(e.B)
}

// Len fills the w-byte layout field at p with n, a field's length or count,
// refused when n is past limit or the field cannot hold it.
func (e *Encoder) Len(p, w, n, limit int) {
	if n > limit {
		e.fail(errMax)
		return
	}
	e.put(p, w, n)
}

// Arm refuses the encode when a union holds a value in any arm beside the one
// its switch chose: the union writes one arm's bytes, and any other arm's
// would silently stay behind.
func (e *Encoder) Arm(empty bool) {
	if !empty {
		e.fail(errArm)
	}
}

// Off fills the w-byte layout field at p with the offset of the field that
// begins at at. A field of no bytes keeps offset 0 and drops the padding
// before it, from begin: MS-SMB2 2.2.13.2 asks it of a context's DataOffset.
//
// decision: every empty field is written so, at offset 0, as the reference
// server's capture shows a CREATE without contexts; withdraw it for a
// field whose layout gives an empty one an offset.
func (e *Encoder) Off(p, w, begin, at int) {
	if len(e.B) == at {
		e.B = e.B[:begin]
		return
	}
	e.put(p, w, at-e.Origin)
}

func (e *Encoder) put(p, w, v int) {
	// A 32-bit int cannot hold 1<<(8*w) at four bytes wide: the bound is
	// taken in uint64, and a negative is refused with it.
	if v < 0 || uint64(v) >= 1<<(8*uint(w)) {
		e.fail(errWide)
		return
	}
	switch w {
	case 2:
		binary.LittleEndian.PutUint16(e.B[p:], uint16(v))
	case 4:
		binary.LittleEndian.PutUint32(e.B[p:], uint32(v))
	}
}

// UTF16 appends s, WTF-8, as UTF-16LE: a surrogate written alone goes back
// as the code unit it was (Decoder.UTF16's decision). A string that is not
// WTF-8 is refused, and a pair written as two halves is refused with it: it
// is CESU-8, not WTF-8, and would name a file on the wire by another file's
// code point.
func (e *Encoder) UTF16(s string) {
	for i := 0; i < len(s); {
		if s[i] == 0xED && i+2 < len(s) && s[i+1]&0xE0 == 0xA0 && s[i+2]&0xC0 == 0x80 {
			if s[i+1]&0xF0 == 0xA0 && i+5 < len(s) && s[i+3] == 0xED && s[i+4]&0xF0 == 0xB0 && s[i+5]&0xC0 == 0x80 {
				e.fail(errWTF8)
				return
			}
			e.Uint16(0xD000 | uint16(s[i+1]&0x3F)<<6 | uint16(s[i+2]&0x3F))
			i += 3
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			e.fail(errWTF8)
			return
		}
		if hi, lo := utf16.EncodeRune(r); hi != utf8.RuneError {
			e.Uint16(uint16(hi))
			e.Uint16(uint16(lo))
		} else {
			e.Uint16(uint16(r))
		}
		i += n
	}
}

// Link ends a linked chain's member before the one about to begin: it pads
// to a multiple of 8 and writes the step from prev, the member before's first
// byte, into that member's first four bytes; prev is -1 before the first. It
// returns the place of the member about to begin.
func (e *Encoder) Link(prev int) int {
	if prev >= 0 {
		e.Begin(8)
		e.put(prev, 4, len(e.B)-prev)
	}
	return len(e.B)
}

// Sub returns an Encoder for a structure inside this one, whose offsets count
// from its own first byte.
func (e *Encoder) Sub() Encoder { return Encoder{B: e.B, Origin: len(e.B), Err: e.Err} }

// Join takes back what an Encoder Sub returned appended and refused.
func (e *Encoder) Join(s *Encoder) { e.B, e.Err = s.B, s.Err }
