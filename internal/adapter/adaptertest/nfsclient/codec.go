package nfsclient

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The numbers the codec writes and reads, from RFC 5531 section 9 and Appendix
// A, written here and not taken from the adapter's wire package, so a mistake
// of that package is not repeated by the client that tests it.
const (
	msgCall     = 0
	msgReply    = 1
	rpcVers     = 2
	accepted    = 0 // reply_stat MSG_ACCEPTED
	denied      = 1 // reply_stat MSG_DENIED
	success     = 0 // accept_stat SUCCESS
	progUnavail = 1 // accept_stat PROG_UNAVAIL
	progMis     = 2 // accept_stat PROG_MISMATCH
	procUnavail = 3 // accept_stat PROC_UNAVAIL
	garbageArgs = 4 // accept_stat GARBAGE_ARGS
	systemErr   = 5 // accept_stat SYSTEM_ERR
	rpcMis      = 0 // reject_stat RPC_MISMATCH
	authError   = 1 // reject_stat AUTH_ERROR
	authSys     = 1 // the AUTH_SYS flavour
)

// errShort is a reply that ends before a field it must hold.
var errShort = errors.New("nfsclient: the reply is cut short")

// pad is the bytes that bring n to a multiple of four (RFC 4506 section 4.10).
func pad(n int) int {
	return (4 - n%4) % 4
}

// appendWord appends w as XDR's four big-endian bytes.
func appendWord(b []byte, w uint32) []byte {
	return binary.BigEndian.AppendUint32(b, w)
}

// appendAuth appends an opaque_auth: its flavour, then its body as an XDR
// opaque.
func appendAuth(b []byte, a Auth) []byte {
	b = appendWord(appendWord(b, a.Flavour), uint32(len(a.Body)))
	b = append(b, a.Body...)

	return append(b, make([]byte, pad(len(a.Body)))...)
}

// encodeCall is c as an RPC call message with the transaction ID xid: the
// fields in order, the arguments as given.
func encodeCall(xid uint32, c Call) []byte {
	var b []byte
	for _, w := range []uint32{xid, msgCall, rpcVers, c.Prog, c.Vers, c.Proc} {
		b = appendWord(b, w)
	}

	b = appendAuth(b, c.Cred)
	b = appendAuth(b, c.Verf)

	return append(b, c.Args...)
}

// reader reads XDR words and opaques off a reply.
type reader struct {
	b   []byte
	err error
}

func (r *reader) word() uint32 {
	if len(r.b) < 4 {
		r.err = errShort
		r.b = nil

		return 0
	}

	w := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]

	return w
}

func (r *reader) auth() Auth {
	flavour, n := r.word(), int(r.word())
	if r.err != nil || n+pad(n) > len(r.b) {
		r.err = errShort
		return Auth{}
	}

	body := append([]byte(nil), r.b[:n]...)
	r.b = r.b[n+pad(n):]

	return Auth{Flavour: flavour, Body: body}
}

// decodeReply reads a reply message (RFC 5531 section 9): an accepted one's
// verifier, accept state, the versions of PROG_MISMATCH and a success's
// results; a denied one's RPC_MISMATCH versions or AUTH_ERROR's auth_stat.
// A reply_stat, accept_stat or reject_stat no RPC defines, or bytes left
// past a reply that names its every field, is an error, never a reply.
func decodeReply(b []byte) (Reply, error) {
	r := reader{b: b}

	rep := Reply{XID: r.word()}
	if kind := r.word(); r.err == nil && kind != msgReply {
		return Reply{}, fmt.Errorf("nfsclient: message type %d, not a reply", kind)
	}

	switch stat := r.word(); stat {
	case accepted:
		rep.Verf = r.auth()

		rep.Stat = r.word()
		switch rep.Stat {
		case progMis:
			rep.Low, rep.High = r.word(), r.word()
		case success:
			rep.Body = append([]byte(nil), r.b...)
			r.b = nil
		case progUnavail, procUnavail, garbageArgs, systemErr:
		default:
			return Reply{}, fmt.Errorf(
				"nfsclient: accept_stat %d, not one an accepted reply gives",
				rep.Stat,
			)
		}

		if rep.Stat != success && r.err == nil && len(r.b) > 0 {
			return Reply{}, fmt.Errorf(
				"nfsclient: %d bytes after an accepted reply of state %d",
				len(r.b), rep.Stat,
			)
		}
	case denied:
		rep.Denied = true

		switch rej := r.word(); rej {
		case rpcMis:
			rep.Low, rep.High = r.word(), r.word()
		case authError:
			rep.Stat = r.word()
		default:
			return Reply{}, fmt.Errorf("nfsclient: reject_stat %d, not one a denial gives", rej)
		}

		if r.err == nil && len(r.b) > 0 {
			return Reply{}, fmt.Errorf("nfsclient: %d bytes after a denied reply", len(r.b))
		}
	default:
		return Reply{}, fmt.Errorf("nfsclient: reply_stat %d, not one a reply gives", stat)
	}

	if r.err != nil {
		return Reply{}, r.err
	}

	return rep, nil
}

// authSysBody is an authsys_parms of stamp 0 and no machine name (RFC 5531
// Appendix A).
func authSysBody(uid, gid uint32, gids []uint32) []byte {
	b := appendWord(appendWord(nil, 0), 0) // the stamp, a machine name of no bytes
	b = appendWord(appendWord(b, uid), gid)

	b = appendWord(b, uint32(len(gids)))
	for _, g := range gids {
		b = appendWord(b, g)
	}

	return b
}
