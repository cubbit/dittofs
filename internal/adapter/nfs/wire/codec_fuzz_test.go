package wire_test

import (
	"reflect"
	"testing"

	"github.com/cubbit/dittofs/pkg/resources"
)

// xr is the second decoder's cursor: RFC 4506's encodings read by hand, from
// the specification, sharing no code with the generator or wire's runtime. A
// read that does not fit sets bad, and every read after it returns zero.
type xr struct {
	b   []byte
	at  int
	bad bool
}

// next is the next n bytes, which with their padding to a multiple of four
// must lie inside b (RFC 4506 sections 4.9, 4.10).
func (r *xr) next(n int) []byte {
	padded := (n + 3) / 4 * 4
	if r.bad || padded > len(r.b)-r.at {
		r.bad = true
		return nil
	}
	p := r.b[r.at : r.at+n : r.at+n]
	r.at += padded
	return p
}

func (r *xr) word() uint32 {
	p := r.next(4)
	if p == nil {
		return 0
	}
	return uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
}

func (r *xr) hyper() uint64 { return uint64(r.word())<<32 | uint64(r.word()) }

// flag is a bool: 0 or 1, and nothing else (RFC 4506 section 4.4).
func (r *xr) flag() bool {
	switch r.word() {
	case 0:
		return false
	case 1:
		return true
	}
	r.bad = true
	return false
}

// opaque is a variable-length opaque of at most max bytes.
func (r *xr) opaque(max int) []byte {
	n := r.word()
	if r.bad || int64(n) > int64(max) {
		r.bad = true
		return nil
	}
	return r.next(int(n))
}

// count is a variable-length array's count, at most max.
func (r *xr) count(max int) int {
	n := r.word()
	if r.bad || int64(n) > int64(max) {
		r.bad = true
		return 0
	}
	return int(n)
}

// The second decoder of each struct, written from its XDR definition.

func readStateID(r *xr) StateID {
	s := StateID{Seqid: r.word()}
	copy(s.Other[:], r.next(12))
	return s
}

func readSetClientIDArgs(r *xr, _ uint32) SetClientIDArgs {
	var a SetClientIDArgs
	copy(a.Client.Verifier[:], r.next(8))
	a.Client.ID = r.opaque(1024)
	a.Callback.Program = r.word()
	a.Callback.Location.NetID = string(r.opaque(16))
	a.Callback.Location.Addr = string(r.opaque(64))
	a.CallbackIdent = r.word()
	return a
}

func readSetattrArgs(r *xr, _ uint32) SetattrArgs {
	var a SetattrArgs
	a.StateID = readStateID(r)
	for i, n := 0, r.count(4); i < n && !r.bad; i++ {
		a.Attrs.Mask = append(a.Attrs.Mask, r.word())
	}
	a.Attrs.List = r.opaque(4096)
	return a
}

func readACL(r *xr, _ uint32) ACL {
	var a ACL
	for i, n := 0, r.count(65536); i < n && !r.bad; i++ {
		var e ACE
		e.Type = r.word()
		e.Flag = r.word()
		e.AccessMask = r.word()
		e.Who = string(r.opaque(1024))
		a.ACEs = append(a.ACEs, e)
	}
	return a
}

// readOpenClaim reads open_claim4: CLAIM_FH, CLAIM_DELEG_CUR_FH and
// CLAIM_DELEG_PREV_FH exist from minor version 1 (RFC 8881 section 18.16.1).
func readOpenClaim(r *xr, v uint32) OpenClaim {
	c := OpenClaim{Claim: OpenClaimType(r.word())}
	if r.bad {
		return c
	}
	switch c.Claim {
	case 0:
		c.File = string(r.opaque(255))
	case 1:
		c.DelegateType = r.word()
	case 2:
		c.DelegateCur.StateID = readStateID(r)
		c.DelegateCur.File = string(r.opaque(255))
	case 3:
		c.FileDelegatePrev = string(r.opaque(255))
	case 4, 6:
		r.bad = r.bad || v < 1
	case 5:
		r.bad = r.bad || v < 1
		if !r.bad {
			c.DelegateStateID = readStateID(r)
		}
	default:
		r.bad = true
	}
	return c
}

func readSetTime(r *xr, _ uint32) SetTime {
	s := SetTime{How: r.word()}
	if s.How == 1 {
		s.Time.Seconds = int64(r.hyper())
		s.Time.NSeconds = r.word()
	}
	return s
}

func readPostOpFH(r *xr, _ uint32) PostOpFH {
	var p PostOpFH
	if r.flag() {
		p.Handle = &FH3{Data: r.opaque(64)}
	}
	return p
}

func readWrite4Args(r *xr, _ uint32) Write4Args {
	var w Write4Args
	w.StateID = readStateID(r)
	w.Offset = r.hyper()
	w.Stable = StableHow(r.word())
	w.Data = r.opaque(1 << 20)
	return w
}

func readSample(r *xr, v uint32) Sample {
	var s Sample
	for i := range s.Words {
		s.Words[i] = r.word()
	}
	s.Flag = r.flag()
	s.Name = string(r.opaque(8))
	for i, n := 0, r.count(3); i < n && !r.bad; i++ {
		s.Mask = append(s.Mask, r.word())
	}
	if v >= 1 {
		s.Extra = r.hyper()
	}
	return s
}

// generated is what the generator writes for T.
type generated[T any] interface {
	*T
	DecodeWire(b []byte, v uint32) (int, error)
	EncodeWire(v uint32, out *resources.Buf) error
	MaxWire(v uint32) int
}

// against checks T's generated decoder against its second decoder on b at v:
// both accept or both refuse, and the accepted reads the same bytes into the
// same value, which encodes, within MaxWire, to bytes the second decoder reads
// back to that value (RFC 20 section 5.4).
func against[T any, P generated[T]](hand func(*xr, uint32) T) func(*testing.T, []byte, uint32) {
	return func(t *testing.T, b []byte, v uint32) {
		var got T
		n, err := P(&got).DecodeWire(b, v)
		r := &xr{b: b}
		want := hand(r, v)
		if (err == nil) == r.bad {
			t.Fatalf("%T at %d: the generated decoder says %v, the second decoder bad %v", got, v, err, r.bad)
		}
		if err != nil {
			return
		}
		if n != r.at || !reflect.DeepEqual(got, want) {
			t.Fatalf("%T at %d: the generated decoder read %d bytes into\n  %+v\nthe second %d into\n  %+v", got, v, n, got, r.at, want)
		}
		var out resources.Buf
		if err := P(&got).EncodeWire(v, &out); err != nil {
			t.Fatalf("%T at %d: a decoded value does not encode: %v", got, v, err)
		}
		if max := P(&got).MaxWire(v); len(out.B) > max {
			t.Fatalf("%T at %d: encoded to %d bytes, past MaxWire's %d", got, v, len(out.B), max)
		}
		r = &xr{b: out.B}
		if again := hand(r, v); r.bad || r.at != len(out.B) || !reflect.DeepEqual(again, got) {
			t.Fatalf("%T at %d: its encoding reads back as\n  %+v\nnot\n  %+v", got, v, again, got)
		}
	}
}

// targets are FuzzCodec's structs, the first byte of an input naming one.
var targets = []func(*testing.T, []byte, uint32){
	against[SetClientIDArgs](readSetClientIDArgs),
	against[SetattrArgs](readSetattrArgs),
	against[ACL](readACL),
	against[OpenClaim](readOpenClaim),
	against[SetTime](readSetTime),
	against[PostOpFH](readPostOpFH),
	against[Write4Args](readWrite4Args),
	against[Sample](readSample),
}

// FuzzCodec decodes an input's bytes past its first two as the struct its
// first names, at the minor version its second names, with the generated
// decoder and the second decoder, and fails on any disagreement, a panic, or a
// decoded value that does not encode back (RFC 20 section 5.4). Seeded with
// every golden vector and the capture's calls.
func FuzzCodec(f *testing.F) {
	kinds := map[reflect.Type]byte{}
	for i, c := range []any{SetClientIDArgs{}, SetattrArgs{}, ACL{}, OpenClaim{}, SetTime{}, PostOpFH{}, Write4Args{}, Sample{}} {
		kinds[reflect.TypeOf(c)] = byte(i)
	}
	for _, c := range vectors(f) {
		f.Add(append([]byte{kinds[reflect.TypeOf(c.want).Elem()], byte(c.v)}, readGolden(f, c.name)...))
	}
	for _, name := range []string{"nfsv4-compound-call", "nfsv4-null-call", "mount-v3-null-call"} {
		for i := range targets {
			f.Add(append([]byte{byte(i), 0}, readGolden(f, name)...))
		}
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) < 2 {
			return
		}
		targets[int(in[0])%len(targets)](t, in[2:], uint32(in[1]%3))
	})
}
