package wire_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/internal/prof"
)

// capturedGSSCall is what tshark's RPCSEC_GSS dissector reads in one of the
// nfs-gss capture's calls (test/pcap-corpus/nfs-gss/CAPTURE.md): the
// credential's procedure, sequence number, service and handle, an INIT's token
// length, an integrity body's length and checksum, and a privacy body's
// length.
type capturedGSSCall struct {
	file           string
	proc, seq, svc uint32
	handle         string
	token          int
	integ          int
	checksum       string
	priv           int
	compound       bool
}

// The checksums are tshark's rpc.authgss.checksum less its length word.
var capturedGSSCalls = []capturedGSSCall{
	{file: "knfsd-nfs3-gss3-krb5-init-call", proc: 1, svc: 1, token: 753},
	{file: "knfsd-nfs3-gss3-krb5-write-call", seq: 4, svc: 1, handle: "02000000"},
	{file: "knfsd-nfs3-gss3-krb5i-init-call", proc: 1, svc: 1, token: 753},
	{file: "knfsd-nfs3-gss3-krb5i-write-call", seq: 4, svc: 2, handle: "04000000", integ: 56, checksum: "040400ffffffffff0000000023b726bd02502661d38fc84d59225117"},
	{file: "knfsd-nfs3-gss3-krb5p-init-call", proc: 1, svc: 1, token: 753},
	{file: "knfsd-nfs3-gss3-krb5p-write-call", seq: 4, svc: 3, handle: "06000000", priv: 116},
	{file: "knfsd-nfs4-gss40-krb5-init-call", proc: 1, svc: 1, token: 753},
	{file: "knfsd-nfs4-gss40-krb5-write-call", seq: 4, svc: 1, handle: "0a000000", compound: true},
	{file: "knfsd-nfs4-gss40-krb5i-init-call", proc: 1, svc: 1, token: 753},
	{file: "knfsd-nfs4-gss40-krb5i-write-call", seq: 4, svc: 2, handle: "0d000000", integ: 104, checksum: "040400ffffffffff00000000364da856dec799046a0abd86d9356e2b", compound: true},
	{file: "knfsd-nfs4-gss40-krb5p-init-call", proc: 1, svc: 1, token: 753},
	{file: "knfsd-nfs4-gss40-krb5p-write-call", seq: 4, svc: 3, handle: "11000000", priv: 164, compound: true},
	{file: "knfsd-nfs4-gss40-krb5p-destroy-call", proc: 3, seq: 13, svc: 3, handle: "11000000"},
	{file: "knfsd-nfs4-gss41-krb5-init-call", proc: 1, svc: 1, token: 753},
	{file: "knfsd-nfs4-gss41-krb5-write-call", seq: 3, svc: 1, handle: "14000000", compound: true},
	{file: "knfsd-nfs4-gss41-krb5-destroy-call", proc: 3, seq: 10, svc: 1, handle: "14000000"},
	{file: "knfsd-nfs4-gss41-krb5i-init-call", proc: 1, svc: 1, token: 753},
	{file: "knfsd-nfs4-gss41-krb5i-write-call", seq: 3, svc: 2, handle: "17000000", integ: 140, checksum: "040400ffffffffff000000003f58a5214273418bea4f58c65e73f925", compound: true},
	{file: "knfsd-nfs4-gss41-krb5p-init-call", proc: 1, svc: 1, token: 753},
	{file: "knfsd-nfs4-gss41-krb5p-write-call", seq: 3, svc: 3, handle: "1a000000", priv: 200, compound: true},
}

// Every captured RPCSEC_GSS call parses whole, its credential and its
// arguments decoding to the fields tshark reads in them: an INIT's token, an
// integrity body led by the credential's sequence number with its checksum,
// a privacy body's wrap token. The credential re-encodes to the bytes knfsd
// was sent. A COMPOUND in the clear has its tag and minor version read with
// the header; one under integrity or privacy has neither, its arguments left
// whole for the GSS stage.
func TestGSSCaptured(t *testing.T) {
	for _, w := range capturedGSSCalls {
		t.Run(w.file, func(t *testing.T) {
			c, err := wire.ParseCall(readGolden(t, w.file))
			if err != nil {
				t.Fatalf("ParseCall: %v", err)
			}
			fl, body := c.Credential()
			if fl != wire.AuthGSS {
				t.Fatalf("credential flavour %d, want %d", fl, wire.AuthGSS)
			}
			cred, err := wire.DecodeGSSCred(body)
			if err != nil {
				t.Fatalf("DecodeGSSCred: %v", err)
			}
			if cred.Proc != w.proc || cred.Seq != w.seq || cred.Service != w.svc || hex.EncodeToString(cred.Handle) != w.handle {
				t.Errorf("credential proc %d seq %d service %d handle %x, want %d %d %d %s",
					cred.Proc, cred.Seq, cred.Service, cred.Handle, w.proc, w.seq, w.svc, w.handle)
			}
			if re := wire.AppendGSSCred(nil, cred); !bytes.Equal(re, body) {
				t.Errorf("the credential re-encodes to %x, want the captured %x", re, body)
			}
			args := c.Args()
			switch {
			case w.token > 0:
				tok, err := wire.DecodeGSSInitArg(args)
				if err != nil || len(tok) != w.token {
					t.Errorf("INIT token of %d bytes (%v), want %d", len(tok), err, w.token)
				}
			case w.integ > 0:
				b, sum, err := wire.GSSIntegBody(args)
				if err != nil || len(b) != w.integ || hex.EncodeToString(sum) != w.checksum {
					t.Errorf("integrity body of %d bytes, checksum %x (%v), want %d, %s", len(b), sum, err, w.integ, w.checksum)
				}
				if len(b) < 4 || binary.BigEndian.Uint32(b) != cred.Seq {
					t.Errorf("the integrity body does not open with the credential's sequence number %d", cred.Seq)
				}
			case w.priv > 0:
				if p, err := wire.GSSPrivBody(args); err != nil || len(p) != w.priv {
					t.Errorf("privacy body of %d bytes (%v), want %d", len(p), err, w.priv)
				}
			}
			minor, ok := c.MinorVersion()
			if clear := w.compound && w.svc == wire.GSSSvcNone; ok != clear || (c.Tag() != nil) != clear {
				t.Errorf("minor version %d (%v), tag %x: a COMPOUND prefix read %v, want %v", minor, ok, c.Tag(), ok, clear)
			}
		})
	}
}

// Each captured INIT reply's rpc_gss_init_res is what AppendGSSInitRes writes
// for the handle, major and minor status, window and token tshark reads in it:
// past the accepted reply's header, its 28-byte MIC verifier and its accept
// state.
func TestGSSInitResCaptured(t *testing.T) {
	for file, handle := range map[string]string{
		"knfsd-nfs3-gss3-krb5-init-reply":   "02000000",
		"knfsd-nfs3-gss3-krb5i-init-reply":  "04000000",
		"knfsd-nfs3-gss3-krb5p-init-reply":  "06000000",
		"knfsd-nfs4-gss40-krb5-init-reply":  "0a000000",
		"knfsd-nfs4-gss40-krb5i-init-reply": "0d000000",
		"knfsd-nfs4-gss40-krb5p-init-reply": "11000000",
		"knfsd-nfs4-gss41-krb5-init-reply":  "14000000",
		"knfsd-nfs4-gss41-krb5i-init-reply": "17000000",
		"knfsd-nfs4-gss41-krb5p-init-reply": "1a000000",
	} {
		rec := readGolden(t, file)
		const resAt = 5*4 + 28 + 4
		h, _ := hex.DecodeString(handle)
		if want := wire.AppendGSSInitRes(nil, h, 0, 0, 128, nil); len(rec) < resAt || !bytes.Equal(rec[resAt:], want) {
			t.Errorf("%s: rpc_gss_init_res %x, want %x", file, rec[min(resAt, len(rec)):], want)
		}
	}
}

// The control procedures the capture lacks, a CONTINUE_INIT, and a credential
// written from RFC 2203 section 5 with a handle not a multiple of four: each
// decodes to its fields and re-encodes to its bytes.
func TestGSSCredHandWritten(t *testing.T) {
	for _, w := range []struct {
		name string
		hex  string
		want wire.GSSCred
	}{
		{"CONTINUE_INIT", "00000001" + "00000002" + "00000000" + "00000001" + "00000004" + "0a0b0c0d",
			wire.GSSCred{Proc: wire.GSSProcContinueInit, Service: wire.GSSSvcNone, Handle: []byte{10, 11, 12, 13}}},
		{"a five-byte handle", "00000001" + "00000000" + "00000007" + "00000002" + "00000005" + "0102030405000000",
			wire.GSSCred{Proc: wire.GSSProcData, Seq: 7, Service: wire.GSSSvcIntegrity, Handle: []byte{1, 2, 3, 4, 5}}},
	} {
		b, _ := hex.DecodeString(w.hex)
		got, err := wire.DecodeGSSCred(b)
		if err != nil || got.Proc != w.want.Proc || got.Seq != w.want.Seq || got.Service != w.want.Service || !bytes.Equal(got.Handle, w.want.Handle) {
			t.Errorf("%s: %+v (%v), want %+v", w.name, got, err, w.want)
		}
		if re := wire.AppendGSSCred(nil, w.want); !bytes.Equal(re, b) {
			t.Errorf("%s: encodes to %x, want %x", w.name, re, b)
		}
	}
}

// u32s writes XDR words.
func u32s(v ...uint32) []byte {
	var b []byte
	for _, x := range v {
		b = binary.BigEndian.AppendUint32(b, x)
	}
	return b
}

// Each decoder refuses what RFC 2203 does not allow, and every length that
// runs past its bytes, the largest included, which it would have to allocate
// or read beyond the buffer to accept.
func TestGSSRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"version 2", u32s(2, 0, 0, 1, 0)},
		{"version 0", u32s(0, 0, 0, 1, 0)},
		{"cut before the service", u32s(1, 0, 0)},
		{"no handle length", u32s(1, 0, 0, 1)},
		{"procedure 4", u32s(1, 4, 0, 1, 0)},
		{"service 0", u32s(1, 0, 0, 0, 0)},
		{"service 4", u32s(1, 0, 0, 4, 0)},
		{"a handle past its opaque", u32s(1, 0, 0, 1, 8, 0)},
		{"the largest handle", u32s(1, 0, 0, 1, 0xffffffff)},
		{"a handle's padding cut", append(u32s(1, 0, 0, 1, 5), 1, 2, 3, 4, 5)},
		{"bytes after the handle", u32s(1, 0, 0, 1, 4, 0, 0)},
		{"an INIT naming a handle", u32s(1, 1, 0, 1, 4, 0)},
	} {
		if got, err := wire.DecodeGSSCred(c.b); err == nil {
			t.Errorf("credential %s: decoded %+v", c.name, got)
		}
	}
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"a body past the arguments", u32s(8, 0)},
		{"the largest body", u32s(0xffffffff)},
		{"bytes after the body", u32s(4, 0, 0)},
	} {
		if tok, err := wire.DecodeGSSInitArg(c.b); err == nil {
			t.Errorf("rpc_gss_init_arg %s: decoded %x", c.name, tok)
		}
		if p, err := wire.GSSPrivBody(c.b); err == nil {
			t.Errorf("rpc_gss_priv_data %s: decoded %x", c.name, p)
		}
	}
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"no checksum", u32s(4, 0)},
		{"a body past the arguments", u32s(8, 0, 0)},
		{"the largest body", u32s(0xffffffff, 0)},
		{"a checksum past the arguments", u32s(4, 0, 8, 0)},
		{"the largest checksum", u32s(4, 0, 0xffffffff)},
		{"bytes after the checksum", u32s(4, 0, 4, 0, 0)},
	} {
		if b, sum, err := wire.GSSIntegBody(c.b); err == nil {
			t.Errorf("rpc_gss_integ_data %s: decoded %x, %x", c.name, b, sum)
		}
	}
}

// A COMPOUND whose arguments are not a program's in the clear is parsed up to
// its verifier, with no prefix read from them: under a credential that does
// not decode, so the stage refuses the credential rather than the header the
// arguments, and under a control procedure, whose arguments are a context
// token or nothing.
func TestParseCallGSSArgsNotInTheClear(t *testing.T) {
	for _, c := range []struct {
		name string
		cred []byte
	}{
		{"service 9", u32s(1, wire.GSSProcData, 1, 9, 0)},
		{"INIT", u32s(1, wire.GSSProcInit, 0, wire.GSSSvcNone, 0)},
		{"DESTROY", u32s(1, wire.GSSProcDestroy, 1, wire.GSSSvcNone, 0)},
	} {
		call := u32s(0x1234, 0, 2, wire.ProgNFS, wire.VersNFS4, wire.ProcCompound, wire.AuthGSS, uint32(len(c.cred)))
		call = append(call, c.cred...)
		call = append(call, u32s(wire.AuthNone, 0, 0xffffffff)...)
		cl, err := wire.ParseCall(call)
		if err != nil {
			t.Errorf("%s: ParseCall: %v", c.name, err)
			continue
		}
		if _, ok := cl.MinorVersion(); ok || cl.Tag() != nil || len(cl.Args()) != 4 {
			t.Errorf("%s: a COMPOUND prefix read: tag %x, args %x", c.name, cl.Tag(), cl.Args())
		}
	}
}

// The decoders hand out slices of their input and allocate nothing (RFC 20
// section 5).
func TestGSSDecodersAllocateNothing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the allocation count says nothing under the detector")
	}
	cred := u32s(1, 0, 4, 2, 4, 7)
	integ := u32s(8, 4, 0, 4, 9)
	priv := u32s(4, 9)
	if n := testing.AllocsPerRun(1000, func() {
		_, _ = wire.DecodeGSSCred(cred)
		_, _, _ = wire.GSSIntegBody(integ)
		_, _ = wire.GSSPrivBody(priv)
		_, _ = wire.DecodeGSSInitArg(priv)
	}); n != 0 {
		t.Errorf("the decoders allocate %v per run", n)
	}
}
