package wire_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

// replyType is an RPC reply, the word after the transaction ID.
const replyType = 1

// roundTo4 pads a length to the four bytes XDR aligns it to.
func roundTo4(n int) int { return n + (4-n%4)%4 }

// overTheCapCall is an AUTH_SYS NULL call whose group list is 17, past RFC
// 5531's 16: its seed runs the fuzz's own assertion on every go test, with the
// caps refused where they are checked.
func overTheCapCall() []byte {
	parms := binary.BigEndian.AppendUint32(nil, 0)  // the stamp
	parms = append(parms, 0, 0, 0, 0)               // a machine name of no bytes
	parms = binary.BigEndian.AppendUint32(parms, 0) // the uid
	parms = binary.BigEndian.AppendUint32(parms, 0) // the gid
	parms = binary.BigEndian.AppendUint32(parms, 17)
	for range 17 {
		parms = binary.BigEndian.AppendUint32(parms, 0)
	}
	call := binary.BigEndian.AppendUint32(nil, 0x4d2)  // the transaction ID
	call = binary.BigEndian.AppendUint32(call, 0)      // a call
	call = binary.BigEndian.AppendUint32(call, 2)      // RPC version 2
	call = binary.BigEndian.AppendUint32(call, 100003) // the NFS program
	call = binary.BigEndian.AppendUint32(call, 3)      // version 3
	call = binary.BigEndian.AppendUint32(call, 0)      // the NULL procedure
	call = binary.BigEndian.AppendUint32(call, wire.AuthSys)
	call = binary.BigEndian.AppendUint32(call, uint32(len(parms)))
	call = append(call, parms...)
	call = binary.BigEndian.AppendUint32(call, 0) // no verifier
	call = binary.BigEndian.AppendUint32(call, 0)
	rec := binary.BigEndian.AppendUint32(nil, uint32(len(call)))
	return append(rec, call...)
}

// FuzzRecord fuzzes a record mark, the call after it and its AUTH_SYS body: no
// panic, no fragment past the cap accepted, no call accepted whose reply does
// not begin with its transaction ID, and no AUTH_SYS credential past RFC
// 5531's limits decoded (RFC 5531 sections 9 and 11, RFC 20 section 5.4).
func FuzzRecord(f *testing.F) {
	for _, name := range []string{"nfsv4-null-call", "nfsv4-null-reply", "nfsv4-compound-call", "mount-v3-null-call"} {
		f.Add(readGolden(f, name))
	}
	f.Add(overTheCapCall())
	f.Fuzz(func(t *testing.T, b []byte) {
		n, _, err := wire.Fragment(b)
		if err != nil {
			return
		}
		if n > wire.MaxRecord {
			t.Fatalf("a fragment of %d bytes is accepted, past %d", n, wire.MaxRecord)
		}
		rec := b[min(4, len(b)):]
		if n < len(rec) {
			rec = rec[:n]
		}
		c, _ := wire.ParseCall(rec)
		if c == nil {
			return
		}
		r := wire.AppendAccepted(nil, c.XID(), wire.AcceptProgUnavail)
		if len(r) < 8 || binary.BigEndian.Uint32(r) != c.XID() || binary.BigEndian.Uint32(r[4:]) != replyType {
			t.Fatalf("the reply to %#x does not begin with the call and a reply", c.XID())
		}
		if fl, cred := c.Credential(); fl == wire.AuthSys {
			a, err := wire.DecodeAuthSys(cred)
			if err == nil && (a.NameLen > 255 || a.NGIDs > 16) {
				t.Fatalf("an AUTH_SYS credential past RFC 5531's limits is decoded: %+v", a)
			}
			// A decode that succeeds consumes the whole body: the stamp, the
			// name and its padding, the uid, gid, count and groups (RFC 5531
			// Appendix A).
			if err == nil && len(cred) != 20+roundTo4(a.NameLen)+4*a.NGIDs {
				t.Fatalf("a decoded AUTH_SYS body of %d bytes holds a name of %d and %d groups", len(cred), a.NameLen, a.NGIDs)
			}
		}
	})
}

// handOpaque reads an XDR opaque at off in b by hand from RFC 4506 section
// 4.10, its length and padding counted in 64 bits so no length wraps, and no
// length past the record's cap: the body and the offset past its padding.
func handOpaque(b []byte, off int) (body []byte, next int, ok bool) {
	if off+4 > len(b) {
		return nil, 0, false
	}
	n := int64(binary.BigEndian.Uint32(b[off:]))
	if n > wire.MaxRecord {
		return nil, 0, false
	}
	end := int64(off) + 4 + (n+3)/4*4
	if end > int64(len(b)) {
		return nil, 0, false
	}
	return b[off+4 : off+4+int(n)], int(end), true
}

// handGSSCred decodes rpc_gss_cred_vers_1_t by hand from RFC 2203 section 5:
// version 1, a procedure of 0 to 3, a service of 1 to 3, and a handle filling
// the rest of the body, empty under INIT.
func handGSSCred(b []byte) (c wire.GSSCred, ok bool) {
	if len(b) < 16 || binary.BigEndian.Uint32(b) != 1 {
		return c, false
	}
	c.Proc, c.Seq, c.Service = binary.BigEndian.Uint32(b[4:]), binary.BigEndian.Uint32(b[8:]), binary.BigEndian.Uint32(b[12:])
	h, next, ok := handOpaque(b, 16)
	if !ok || next != len(b) || c.Proc > 3 || c.Service < 1 || c.Service > 3 || c.Proc == 1 && len(h) != 0 {
		return wire.GSSCred{}, false
	}
	c.Handle = h
	return c, true
}

// appendHandOpaque writes an XDR opaque by hand, zero-padded.
func appendHandOpaque(dst, b []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(b)))
	dst = append(dst, b...)
	return append(dst, make([]byte, roundTo4(len(b))-len(b))...)
}

// FuzzGSSCred fuzzes the RPCSEC_GSS credential and the INIT, integrity and
// privacy bodies: no panic; each decoder accepts exactly what a second decoder
// written by hand from RFC 2203 sections 5 and 5.3.2 accepts, to the same
// fields; every slice it hands out lies inside its input; and each decoded
// value re-encodes to bytes that decode to the same value (RFC 20 section
// 5.4).
func FuzzGSSCred(f *testing.F) {
	for _, w := range capturedGSSCalls {
		c, err := wire.ParseCall(readGolden(f, w.file))
		if err != nil {
			f.Fatalf("%s: %v", w.file, err)
		}
		_, cred := c.Credential()
		f.Add(cred)
		f.Add(c.Args())
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		inside := func(what string, s []byte) {
			if len(s) > len(b) {
				t.Fatalf("%s: %d bytes from an input of %d", what, len(s), len(b))
			}
		}

		got, err := wire.DecodeGSSCred(b)
		want, ok := handGSSCred(b)
		if (err == nil) != ok {
			t.Fatalf("DecodeGSSCred accepts %v, the hand decoder %v: %x", err == nil, ok, b)
		}
		if ok {
			inside("the handle", got.Handle)
			if got.Proc != want.Proc || got.Seq != want.Seq || got.Service != want.Service || !bytes.Equal(got.Handle, want.Handle) {
				t.Fatalf("DecodeGSSCred reads %+v, the hand decoder %+v", got, want)
			}
			again, err := wire.DecodeGSSCred(wire.AppendGSSCred(nil, got))
			if err != nil || again.Proc != got.Proc || again.Seq != got.Seq || again.Service != got.Service || !bytes.Equal(again.Handle, got.Handle) {
				t.Fatalf("%+v re-encodes to %+v (%v)", got, again, err)
			}
		}

		// rpc_gss_init_arg and rpc_gss_priv_data are each one opaque.
		hb, next, first := handOpaque(b, 0)
		ok = first && next == len(b)
		for _, d := range []struct {
			name   string
			decode func([]byte) ([]byte, error)
		}{{"DecodeGSSInitArg", wire.DecodeGSSInitArg}, {"GSSPrivBody", wire.GSSPrivBody}} {
			got, err := d.decode(b)
			if (err == nil) != ok || ok && !bytes.Equal(got, hb) {
				t.Fatalf("%s reads %x (%v), the hand decoder %x (%v)", d.name, got, err, hb, ok)
			}
			if ok {
				inside(d.name, got)
				if again, err := d.decode(appendHandOpaque(nil, got)); err != nil || !bytes.Equal(again, got) {
					t.Fatalf("%s: %x re-encodes to %x (%v)", d.name, got, again, err)
				}
			}
		}

		// rpc_gss_integ_data is two.
		body, sum, err := wire.GSSIntegBody(b)
		var hsum []byte
		ok2 := false
		if first {
			var end int
			hsum, end, ok2 = handOpaque(b, next)
			ok2 = ok2 && end == len(b)
		}
		if (err == nil) != ok2 || ok2 && (!bytes.Equal(body, hb) || !bytes.Equal(sum, hsum)) {
			t.Fatalf("GSSIntegBody reads %x, %x (%v), the hand decoder %x, %x (%v)", body, sum, err, hb, hsum, ok2)
		}
		if ok2 {
			inside("the integrity body", body)
			inside("the checksum", sum)
			b2, s2, err := wire.GSSIntegBody(appendHandOpaque(appendHandOpaque(nil, body), sum))
			if err != nil || !bytes.Equal(b2, body) || !bytes.Equal(s2, sum) {
				t.Fatalf("%x, %x re-encode to %x, %x (%v)", body, sum, b2, s2, err)
			}
		}
	})
}
