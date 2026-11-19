package wire_test

import (
	"reflect"
	"runtime"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/smb/wire"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/resources"
)

// sr is the second decoder's reader: MS-SMB2 2.2's layouts read by hand, from
// the specification, sharing no code with the generator or wire's runtime. A
// read that does not fit sets bad, and every read after it returns zero.
type sr struct {
	b    []byte
	base int // the position b[0] is at: 64, past the header, for a body; 0 inside it
	at   int // where the next field in order begins
	end  int // past the last byte any field took
	bad  bool
}

// next is the next n bytes in order; nil for none.
func (r *sr) next(n int) []byte {
	if r.bad || n > len(r.b)-r.at {
		r.bad = true
		return nil
	}
	if n == 0 {
		return nil
	}
	p := r.b[r.at : r.at+n : r.at+n]
	r.at += n
	r.end = max(r.end, r.at)
	return p
}

// u reads an n-byte little-endian integer (MS-SMB2 2.2).
func (r *sr) u(n int) uint64 {
	var v uint64
	for i, c := range r.next(n) {
		v |= uint64(c) << (8 * i)
	}
	return v
}

func (r *sr) u8() uint8   { return uint8(r.u(1)) }
func (r *sr) u16() uint16 { return uint16(r.u(2)) }
func (r *sr) u32() uint32 { return uint32(r.u(4)) }
func (r *sr) u64() uint64 { return r.u(8) }

// size reads a StructureSize, which must be want, the body at least least
// bytes: an odd size counts the variable part's first byte, which a request
// carries and a reply need not.
func (r *sr) size(want uint16, least int) {
	if r.u16() != want || len(r.b) < least {
		r.bad = true
	}
}

// located is the n bytes an offset and length pair locate: none when n is 0,
// its offset then unread; else at most limit of them, at an offset a multiple
// of align, past every byte taken before them and inside b.
func (r *sr) located(off, n, limit, align int) []byte {
	if r.bad || n == 0 {
		return nil
	}
	p := off - r.base
	if n > limit || off%align != 0 || p < r.end || p > len(r.b) || n > len(r.b)-p {
		r.bad = true
		return nil
	}
	r.end = max(r.end, p+n)
	return r.b[p : p+n : p+n]
}

// text is b as UTF-16LE: a surrogate pair is its code point, and any other
// surrogate is the three bytes its value would take in UTF-8.
func (r *sr) text(b []byte) string {
	if len(b)%2 == 1 {
		r.bad = true
		return ""
	}
	var s []byte
	for i := 0; i < len(b); i += 2 {
		u := rune(b[i]) | rune(b[i+1])<<8
		if u >= 0xd800 && u < 0xdc00 && i+3 < len(b) {
			if lo := rune(b[i+2]) | rune(b[i+3])<<8; lo >= 0xdc00 && lo < 0xe000 {
				s = append(s, string(0x10000+(u-0xd800)<<10+(lo-0xdc00))...)
				i += 2
				continue
			}
		}
		if u >= 0xd800 && u < 0xe000 {
			s = append(s, 0xed, byte(0xa0|u>>6&0x1f), byte(0x80|u&0x3f))
			continue
		}
		s = append(s, string(u)...)
	}
	return string(s)
}

// inner reads a structure inside this one, b, with read, and takes its
// refusal.
func inner[T any](r *sr, b []byte, read func(*sr) T) T {
	s := &sr{b: b}
	v := read(s)
	r.bad = r.bad || s.bad
	return v
}

// The second decoder of each struct, written from its MS-SMB2 2.2 section.

func readClose(r *sr, _ uint32) CloseRequest { // 2.2.15
	var c CloseRequest
	r.size(24, 24)
	c.Flags = r.u16()
	r.next(4)
	copy(c.FileID[:], r.next(16))
	return c
}

func readEcho(r *sr, _ uint32) EchoRequest { // 2.2.28
	r.size(4, 4)
	r.next(2)
	return EchoRequest{}
}

func readWrite(r *sr, _ uint32) WriteRequest { // 2.2.21
	var w WriteRequest
	r.size(49, 48) // WRITE's Buffer may be empty
	dataOff, dataLen := int(r.u16()), int(r.u32())
	w.Offset = r.u64()
	copy(w.FileID[:], r.next(16))
	w.Channel = r.u32()
	w.RemainingBytes = r.u32()
	infoOff, infoLen := int(r.u16()), int(r.u16())
	w.Flags = r.u32()
	w.Data = r.located(dataOff, dataLen, 8<<20, 1)
	w.ChannelInfo = r.located(infoOff, infoLen, 1024, 1)
	return w
}

// readCreate reads 2.2.13, its create contexts a chain in which each
// context's Next steps from its first byte to the next one's, a multiple of 8
// inside the chain, and is 0 on the last, which takes the chain's rest
// (2.2.13.2).
func readCreate(r *sr, _ uint32) CreateRequest {
	var c CreateRequest
	r.size(57, 57)
	c.SecurityFlags = r.u8()
	c.RequestedOplockLevel = r.u8()
	c.ImpersonationLevel = r.u32()
	c.SmbCreateFlags = r.u64()
	r.next(8)
	c.DesiredAccess = AccessMask(r.u32())
	c.FileAttributes = r.u32()
	c.ShareAccess = r.u32()
	c.CreateDisposition = r.u32()
	c.CreateOptions = r.u32()
	nameOff, nameLen := int(r.u16()), int(r.u16())
	chainOff, chainLen := int(r.u32()), int(r.u32())
	c.Name = r.text(r.located(nameOff, nameLen, 1024, 1))
	chain := r.located(chainOff, chainLen, 4096, 8)
	for len(chain) > 0 && !r.bad {
		if len(chain) < 4 {
			r.bad = true
			break
		}
		step := int(chain[0]) | int(chain[1])<<8 | int(chain[2])<<16 | int(chain[3])<<24
		ctx := chain
		if step != 0 {
			if step%8 != 0 || step >= len(chain) {
				r.bad = true
				break
			}
			ctx = chain[:step:step]
		}
		c.Contexts = append(c.Contexts, inner(r, ctx, readCreateContext))
		chain = chain[len(ctx):]
	}
	r.end = max(r.end, 57) // the request takes the Buffer's byte its StructureSize counts
	return c
}

// readCreateContext reads 2.2.13.2: its offsets count from its own first
// byte, its data 8-byte aligned and read by its name, DH2Q's (2.2.13.2.11)
// and AlSi's (2.2.13.2.6) into their structs, each exactly its structure's
// size (3.3.5.9), a known name's kept as bytes up to 1024 and refused past
// them, any other's kept as bytes up to 1024 and ignored past them.
func readCreateContext(r *sr) CreateContext {
	var c CreateContext
	r.u32() // Next, which the chain reads
	nameOff, nameLen := int(r.u16()), int(r.u16())
	r.next(2)
	dataOff, dataLen := int(r.u16()), int(r.u32())
	c.Name = r.located(nameOff, nameLen, 16, 1)
	data := r.located(dataOff, dataLen, len(r.b), 8)
	switch string(c.Name) {
	case "DH2Q":
		r.bad = r.bad || len(data) != 32
		c.DurableV2 = inner(r, data, func(d *sr) DurableHandleRequestV2 {
			v := DurableHandleRequestV2{Timeout: d.u32(), Flags: d.u32()}
			d.next(8)
			copy(v.CreateGUID[:], d.next(16))
			return v
		})
	case "AlSi":
		r.bad = r.bad || len(data) != 8
		c.AllocationSize = inner(r, data, func(d *sr) AllocationSize { return AllocationSize{AllocationSize: d.u64()} })
	case "DHnQ", "DHnC", "DH2C", "RqLs", "QFid", "SecD", "ExtA", "TWrp", "MxAc",
		"\x45\xbc\xa6\x6a\xef\xa7\xf7\x4a\x90\x08\xfa\x46\x2e\x14\x4d\x74", // AppInstanceId
		"\xb9\x82\xd0\xb7\x3b\x56\x07\x4f\xa0\x7b\x52\x4a\x81\x16\xa0\x10": // AppInstanceVersion
		r.bad = r.bad || len(data) > 1024
		c.Data = data
	default:
		if len(data) <= 1024 {
			c.Data = data
		}
	}
	return c
}

// negotiateContexts reads 2.2.3.1's contexts: count of them, at most 16, the
// first at offset, a multiple of 8, past every byte taken before them, each
// one after at the next position a multiple of 8.
func negotiateContexts(r *sr, offset, count int) []NegotiateContext {
	if r.bad || count == 0 {
		return nil
	}
	p := offset - r.base
	if count > 16 || offset%8 != 0 || p < r.end || p > len(r.b) {
		r.bad = true
		return nil
	}
	var cs []NegotiateContext
	for range count {
		for (r.base+p)%8 != 0 {
			p++
		}
		if p > len(r.b) {
			r.bad = true
			return cs
		}
		s := &sr{b: r.b[p:]}
		cs = append(cs, readNegotiateContext(s))
		r.bad = r.bad || s.bad
		if r.bad {
			return cs
		}
		p += s.end
		r.end = max(r.end, p)
	}
	return cs
}

// readNegotiateContext reads 2.2.3.1: its data, DataLength bytes, read by its
// type, 1's (2.2.3.1.1) and 2's (2.2.3.1.2) into their structs, the netname
// and signing contexts' (5, 8) kept as bytes up to 1024 and refused past them,
// any other's kept as bytes up to 1024 and ignored past them (3.3.5.4).
func readNegotiateContext(r *sr) NegotiateContext {
	c := NegotiateContext{ContextType: r.u16()}
	n := int(r.u16())
	r.next(4)
	data := r.next(n)
	switch c.ContextType {
	case 1:
		c.Preauth = inner(r, data, func(d *sr) PreauthIntegrityCapabilities {
			var p PreauthIntegrityCapabilities
			algs, salt := int(d.u16()), int(d.u16())
			p.HashAlgorithms = u16s(d, algs, 8)
			if salt > 64 {
				d.bad = true
			}
			p.Salt = d.next(salt)
			return p
		})
	case 2:
		c.Encryption = inner(r, data, func(d *sr) EncryptionCapabilities {
			return EncryptionCapabilities{Ciphers: u16s(d, int(d.u16()), 8)}
		})
	case 5, 8:
		r.bad = r.bad || n > 1024
		c.Data = data
	default:
		if n <= 1024 {
			c.Data = data
		}
	}
	return c
}

// u16s reads n 16-bit integers in order, at most limit of them.
func u16s(r *sr, n, limit int) []uint16 {
	if n > limit {
		r.bad = true
	}
	var s []uint16
	for i := 0; i < n && !r.bad; i++ {
		s = append(s, r.u16())
	}
	return s
}

// readNegotiateRequest reads 2.2.3, whose negotiate contexts are on the wire
// when its dialects hold 3.1.1, their offset and count ClientStartTime,
// ignored, when they do not.
func readNegotiateRequest(r *sr, _ uint32) NegotiateRequest {
	var n NegotiateRequest
	r.size(36, 36)
	dialects := int(r.u16())
	n.SecurityMode = r.u16()
	r.next(2)
	n.Capabilities = r.u32()
	copy(n.ClientGUID[:], r.next(16))
	offset, count := int(r.u32()), int(r.u16())
	r.next(2)
	n.Dialects = u16s(r, dialects, 64)
	if slices.Contains(n.Dialects, 0x311) {
		n.Contexts = negotiateContexts(r, offset, count)
	}
	return n
}

// readNegotiateResponse reads 2.2.4, whose negotiate contexts are on the wire
// from 3.1.1, their count and offset Reserved before it.
func readNegotiateResponse(r *sr, v uint32) NegotiateResponse {
	var n NegotiateResponse
	r.size(65, 64) // a reply's Buffer may be empty
	n.SecurityMode = r.u16()
	n.DialectRevision = r.u16()
	count := int(r.u16())
	copy(n.ServerGUID[:], r.next(16))
	n.Capabilities = r.u32()
	n.MaxTransactSize = r.u32()
	n.MaxReadSize = r.u32()
	n.MaxWriteSize = r.u32()
	n.SystemTime = r.u64()
	n.ServerStartTime = r.u64()
	bufOff, bufLen := int(r.u16()), int(r.u16())
	offset := int(r.u32())
	n.SecurityBuffer = r.located(bufOff, bufLen, 4096, 1)
	if v >= 0x311 {
		n.Contexts = negotiateContexts(r, offset, count)
	}
	return n
}

// generated is what the generator writes for T.
type generated[T any] interface {
	*T
	codec
}

// against checks T's generated decoder against its second decoder on b at v:
// both accept or both refuse, and the accepted reads the same bytes into the
// same value, allocating no more than the input's size allows, which encodes,
// within MaxWire, to bytes the second decoder reads back to that value (RFC 20
// section 5.4).
func against[T any, P generated[T]](hand func(*sr, uint32) T) func(*testing.T, []byte, uint32) {
	return func(t *testing.T, b []byte, v uint32) {
		var got T
		n, err := P(&got).DecodeWire(b, v)
		r := &sr{b: b, base: 64}
		want := hand(r, v)
		if (err == nil) == r.bad {
			t.Fatalf("%T at %#x: the generated decoder says %v, the second decoder bad %v", got, v, err, r.bad)
		}
		if !prof.RaceEnabled {
			if a := allocs(func() { _, _ = P(new(T)).DecodeWire(b, v) }); a > 4<<10+16*uint64(len(b)) {
				t.Fatalf("%T at %#x: decoding %d bytes allocates %d", got, v, len(b), a)
			}
		}
		if err != nil {
			return
		}
		if n != r.end || !reflect.DeepEqual(got, want) {
			t.Fatalf("%T at %#x: the generated decoder read %d bytes into\n  %+v\nthe second %d into\n  %+v", got, v, n, got, r.end, want)
		}
		var out resources.Buf
		if err := P(&got).EncodeWire(v, &out); err != nil {
			t.Fatalf("%T at %#x: a decoded value does not encode: %v", got, v, err)
		}
		if max := P(&got).MaxWire(v); len(out.B) > max {
			t.Fatalf("%T at %#x: encoded to %d bytes, past MaxWire's %d", got, v, len(out.B), max)
		}
		r = &sr{b: out.B, base: 64}
		if again := hand(r, v); r.bad || r.end != len(out.B) || !reflect.DeepEqual(again, got) {
			t.Fatalf("%T at %#x: its encoding reads back as\n  %+v\nnot\n  %+v", got, v, again, got)
		}
	}
}

// allocs is what one call of fn allocates, in bytes: the least of three
// calls, since the count is the process's and the fuzzing engine allocates
// beside the call.
func allocs(fn func()) uint64 {
	least := uint64(1<<64 - 1)
	for range 3 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		fn()
		runtime.ReadMemStats(&after)
		least = min(least, after.TotalAlloc-before.TotalAlloc)
	}
	return least
}

// targets are FuzzCodec's structs, the first byte of an input naming one.
var targets = []func(*testing.T, []byte, uint32){
	against[CloseRequest](readClose),
	against[CreateRequest](readCreate),
	against[NegotiateRequest](readNegotiateRequest),
	against[NegotiateResponse](readNegotiateResponse),
	against[WriteRequest](readWrite),
	against[EchoRequest](readEcho),
}

// dialects are the ones an input's second byte names.
var dialects = []uint32{0x202, 0x210, 0x300, 0x302, 0x311}

// FuzzCodec decodes an input's bytes past its first two as the struct its
// first names, at the dialect its second names, with the generated decoder
// and the second decoder, and fails on any disagreement, a panic, an
// allocation out of proportion to the input, or a decoded value that does not
// encode back (RFC 20 section 5.4). Seeded with every golden vector, an empty
// CREATE and the capture's frames.
func FuzzCodec(f *testing.F) {
	kinds := map[reflect.Type]byte{}
	for i, c := range []any{CloseRequest{}, CreateRequest{}, NegotiateRequest{}, NegotiateResponse{}, WriteRequest{}, EchoRequest{}} {
		kinds[reflect.TypeOf(c)] = byte(i)
	}
	dialect := map[uint32]byte{0x210: 1, 0x311: 4}
	for _, c := range vectors(f) {
		f.Add(append([]byte{kinds[reflect.TypeOf(c.want).Elem()], dialect[c.v]}, golden(f, c.name)...))
	}
	f.Add(append([]byte{kinds[reflect.TypeFor[CreateRequest]()], 4}, patched(golden(f, "create-request")[:57], 110, 0, 0)...)) // an empty CREATE
	for _, name := range []string{"negotiate_request", "negotiate_response", "create_request", "create_response"} {
		body := wire.ReadGolden(f, name)[wire.FrameHeaderLen+wire.HeaderLen:]
		for i := range targets {
			f.Add(append([]byte{byte(i), 4}, body...))
		}
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) < 2 {
			return
		}
		targets[int(in[0])%len(targets)](t, in[2:], dialects[int(in[1])%len(dialects)])
	})
}
