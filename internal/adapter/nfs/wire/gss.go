package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// RPCSEC_GSS's control and data procedures, its services, and the sequence
// number at which a context must be dropped (RFC 2203 sections 5 and 5.3.3.1).
const (
	GSSProcData         uint32 = 0 // RPCSEC_GSS_DATA
	GSSProcInit         uint32 = 1 // RPCSEC_GSS_INIT
	GSSProcContinueInit uint32 = 2 // RPCSEC_GSS_CONTINUE_INIT
	GSSProcDestroy      uint32 = 3 // RPCSEC_GSS_DESTROY

	GSSSvcNone      uint32 = 1 // rpc_gss_svc_none: the header's checksum alone
	GSSSvcIntegrity uint32 = 2 // rpc_gss_svc_integrity: the arguments checksummed
	GSSSvcPrivacy   uint32 = 3 // rpc_gss_svc_privacy: the arguments encrypted

	// GSSMaxSeq is MAXSEQ: a sequence number at or above it ends its context.
	GSSMaxSeq uint32 = 0x80000000
)

// RPCSEC_GSS's auth_stat values (RFC 5531 section 9, RFC 2203 section 5.3.3.3):
// a credential the server cannot match to a context, and a context it holds no
// longer.
const (
	AuthCredProblem uint32 = 13 // RPCSEC_GSS_CREDPROBLEM
	AuthCtxProblem  uint32 = 14 // RPCSEC_GSS_CTXPROBLEM
)

// gssVersion is RPCSEC_GSS_VERS_1, the only version.
const gssVersion uint32 = 1

var errTrailing = errors.New("bytes left after the value")

// GSSCred is an RPCSEC_GSS credential's body, rpc_gss_cred_vers_1_t (RFC 2203
// section 5).
type GSSCred struct {
	Proc    uint32 // RPCSEC_GSS_DATA, _INIT, _CONTINUE_INIT, _DESTROY
	Seq     uint32
	Service uint32 // rpc_gss_svc_none, _integrity, _privacy
	Handle  []byte // into the record, at most 400 bytes with the rest of the body
}

// DecodeGSSCred reads an RPCSEC_GSS credential's body: version 1, then the
// procedure, sequence number, service and context handle. It refuses another
// version, a procedure or service RFC 2203 does not define, a handle running
// past the body, bytes left after the handle, and an INIT naming a handle,
// which only the server hands out. The handle is a slice of body, never a copy.
func DecodeGSSCred(body []byte) (GSSCred, error) {
	var c GSSCred
	r := reader{b: body}
	v, ok := r.u32()
	if !ok {
		return c, fmt.Errorf("nfs/wire: %w: RPCSEC_GSS's version", errPastRecord)
	}
	if v != gssVersion {
		return c, fmt.Errorf("nfs/wire: RPCSEC_GSS version %d", v)
	}
	for i, p := range []*uint32{&c.Proc, &c.Seq, &c.Service} {
		if *p, ok = r.u32(); !ok {
			return GSSCred{}, fmt.Errorf("nfs/wire: %w: RPCSEC_GSS's field %d", errPastRecord, i+1)
		}
	}
	if c.Proc > GSSProcDestroy {
		return GSSCred{}, fmt.Errorf("nfs/wire: RPCSEC_GSS procedure %d", c.Proc)
	}
	if c.Service < GSSSvcNone || c.Service > GSSSvcPrivacy {
		return GSSCred{}, fmt.Errorf("nfs/wire: RPCSEC_GSS service %d", c.Service)
	}
	at, n, err := r.opaque()
	if err != nil {
		return GSSCred{}, fmt.Errorf("nfs/wire: RPCSEC_GSS's handle: %w", err)
	}
	if r.off != len(body) {
		return GSSCred{}, fmt.Errorf("nfs/wire: %w: %d after RPCSEC_GSS's handle", errTrailing, len(body)-r.off)
	}
	if c.Proc == GSSProcInit && n != 0 {
		return GSSCred{}, fmt.Errorf("nfs/wire: an RPCSEC_GSS INIT naming a %d-byte handle", n)
	}
	c.Handle = body[at : at+n]
	return c, nil
}

// AppendGSSCred appends c's body, as DecodeGSSCred reads it, version 1.
func AppendGSSCred(dst []byte, c GSSCred) []byte {
	dst = binary.BigEndian.AppendUint32(dst, gssVersion)
	dst = binary.BigEndian.AppendUint32(dst, c.Proc)
	dst = binary.BigEndian.AppendUint32(dst, c.Seq)
	dst = binary.BigEndian.AppendUint32(dst, c.Service)
	return appendOpaque(dst, c.Handle)
}

// DecodeGSSInitArg reads an INIT's or CONTINUE_INIT's arguments,
// rpc_gss_init_arg: the context token, a slice of args.
func DecodeGSSInitArg(args []byte) (token []byte, err error) {
	return onlyOpaque(args, "rpc_gss_init_arg")
}

// AppendGSSInitRes appends rpc_gss_init_res: the context handle, the major
// and minor status, the sequence window and the reply token (RFC 2203 section
// 5.2.3.1).
func AppendGSSInitRes(dst, handle []byte, major, minor, window uint32, token []byte) []byte {
	dst = appendOpaque(dst, handle)
	dst = binary.BigEndian.AppendUint32(dst, major)
	dst = binary.BigEndian.AppendUint32(dst, minor)
	dst = binary.BigEndian.AppendUint32(dst, window)
	return appendOpaque(dst, token)
}

// GSSIntegBody reads a call's arguments at rpc_gss_svc_integrity,
// rpc_gss_integ_data: body is databody_integ whole, the sequence number and
// the procedure's arguments the checksum covers, and checksum the MIC over it,
// both slices of args (RFC 2203 section 5.3.2.2).
func GSSIntegBody(args []byte) (body, checksum []byte, err error) {
	r := reader{b: args}
	at, n, err := r.opaque()
	if err != nil {
		return nil, nil, fmt.Errorf("nfs/wire: rpc_gss_integ_data's body: %w", err)
	}
	cat, cn, err := r.opaque()
	if err != nil {
		return nil, nil, fmt.Errorf("nfs/wire: rpc_gss_integ_data's checksum: %w", err)
	}
	if r.off != len(args) {
		return nil, nil, fmt.Errorf("nfs/wire: %w: %d after rpc_gss_integ_data", errTrailing, len(args)-r.off)
	}
	return args[at : at+n], args[cat : cat+cn], nil
}

// GSSPrivBody reads a call's arguments at rpc_gss_svc_privacy,
// rpc_gss_priv_data: the wrap token sealing the sequence number and the
// procedure's arguments, a slice of args (RFC 2203 section 5.3.2.3).
func GSSPrivBody(args []byte) (wrapped []byte, err error) {
	return onlyOpaque(args, "rpc_gss_priv_data")
}

// onlyOpaque reads b as exactly one XDR opaque and returns its body.
func onlyOpaque(b []byte, what string) ([]byte, error) {
	r := reader{b: b}
	at, n, err := r.opaque()
	if err != nil {
		return nil, fmt.Errorf("nfs/wire: %s: %w", what, err)
	}
	if r.off != len(b) {
		return nil, fmt.Errorf("nfs/wire: %w: %d after %s", errTrailing, len(b)-r.off, what)
	}
	return b[at : at+n], nil
}

// appendOpaque appends b as an XDR opaque: its length, its bytes, and zeros
// padding it to four.
func appendOpaque(dst, b []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(b)))
	dst = append(dst, b...)
	for i := len(b); i%4 != 0; i++ {
		dst = append(dst, 0)
	}
	return dst
}

// clearArgs reports whether a call whose credential is flavour and body
// carries its program's arguments in the clear: any call but an RPCSEC_GSS
// one, and of those only a DATA call at rpc_gss_svc_none. Under integrity or
// privacy the arguments are a protected body (RFC 2203 section 5.3.2), an
// INIT's or CONTINUE_INIT's are a context token, and a credential that does
// not decode says nothing of them.
//
// decision: the header reads an RPCSEC_GSS credential's procedure and service,
// though the credential is judged only by the GSS stage at unwrap (RFC 21
// section 11.1), because whether a COMPOUND's tag and minor version can be read
// with the header depends on them. It judges nothing: a call whose credential
// does not decode, or whose arguments are not in the clear, is parsed up to its
// verifier with no COMPOUND prefix, so it reaches the stage, which refuses or
// unwraps it, rather than failing to decode on its ciphertext. Its minor
// version is therefore not known until the arguments are unwrapped. Overturned
// if the header must route such a call by minor version before unwrap.
func clearArgs(flavour uint32, body []byte) bool {
	if flavour != AuthGSS {
		return true
	}
	c, err := DecodeGSSCred(body)
	return err == nil && c.Proc == GSSProcData && c.Service == GSSSvcNone
}
