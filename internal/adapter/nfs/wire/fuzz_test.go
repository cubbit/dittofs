package wire_test

import (
	"encoding/binary"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

// FuzzRecord fuzzes a record mark, the call after it and its AUTH_SYS body: no
// panic, no fragment past the cap accepted, no call accepted whose reply does
// not begin with its transaction ID (RFC 5531 sections 9 and 11, RFC 20
// section 5.4).
func FuzzRecord(f *testing.F) {
	for _, name := range []string{"nfsv4-null-call", "nfsv4-null-reply", "nfsv4-compound-call", "mount-v3-null-call"} {
		f.Add(readGolden(f, name))
	}
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
		if len(r) < 4 || binary.BigEndian.Uint32(r) != c.XID() {
			t.Fatalf("the reply to %#x does not begin with its transaction ID", c.XID())
		}
		if fl, cred := c.Credential(); fl == wire.AuthSys {
			_, _ = wire.DecodeAuthSys(cred)
		}
	})
}
