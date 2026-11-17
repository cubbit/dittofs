package wire_test

import (
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
