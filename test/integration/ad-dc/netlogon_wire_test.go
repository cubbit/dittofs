//go:build ad_dc

package ad_dc_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/ndr"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	nlssp "github.com/oiweiwei/go-msrpc/ssp/netlogon"
	nlcrypto "github.com/oiweiwei/go-msrpc/ssp/netlogon/crypto"

	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The NETLOGON calls the secure channel makes, by opnum (MS-NRPC 3.5.4).
const (
	opRequestChallenge = 4
	opAuthenticate3    = 26
	opPasswordSet2     = 30
	opSamLogonEx       = 39
)

var opNames = map[uint16]string{
	opRequestChallenge: "RequestChallenge",
	opAuthenticate3:    "Authenticate3",
	opPasswordSet2:     "PasswordSet2",
	opSamLogonEx:       "SamLogonEx",
}

func opName(op uint16) string {
	if n, ok := opNames[op]; ok {
		return n
	}
	return fmt.Sprintf("op%d", op)
}

// DCE/RPC's connection-oriented PDU types and the Netlogon authentication
// (C706 12.6.4; MS-RPCE 2.2.1.1.7; MS-NRPC 3.3.4.2).
const (
	ptypeRequest  = 0
	ptypeResponse = 2
	ptypeBind     = 11

	authNetlogon     = 68
	authLevelPrivacy = 6

	pduHeader = 24 // the common header and a request's or response's fields
)

// errTear is the fault that tears a connection: a request's before any byte
// of it is written, or a reply's once the controller has answered it.
var errTear = errors.New("wire: connection torn")

// flip is the fault that XORs mask into byte off of a reply's stub, as the
// controller would have to send it: under the seal, the reply is unsealed,
// flipped and sealed again with the channel's session key, so only the
// client's own checks of the stub can see it.
type flip struct {
	off  int
	mask byte
}

func (f flip) Error() string {
	return fmt.Sprintf("wire: flip byte %d of the reply's stub by %#02x", f.off, f.mask)
}

// sentPDU is one PDU the client wrote.
type sentPDU struct {
	conn      int // which connection, numbered from 1 in the order dialled
	ptype     byte
	opnum     uint16 // a request's
	authType  byte   // 0 when the PDU carries no authentication
	authLevel byte
	stub      []byte // a request's stub as written: plain text unless sealed
}

// wire is a fault wrapper (kind 3) around every connection a
// netlogon.Dialer makes to the real controller. It reads each DCE/RPC PDU
// as it passes, records what the client writes, and on its fault's cue
// tears a connection or flips a byte of a reply. The fault is asked "request"
// before a request is written and "reply" once a reply has been read, keyed by
// the call's name in opNames. The machine account's password lets it reseal a
// sealed reply, as only the controller could.
type wire struct {
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	fault   sim.Fault
	machine credential.Credential

	mu         sync.Mutex
	conns      int
	sent       []sentPDU
	clientChal []byte // the last NetrServerReqChallenge's, both ways
	serverChal []byte
}

func newWire(t *testing.T, account string, password []byte, rules ...sim.Rule) *wire {
	t.Helper()
	s, err := sim.NewSchedule(1, nil, rules...)
	if err != nil {
		t.Fatal(err)
	}
	return &wire{dial: (&net.Dialer{}).DialContext, fault: s,
		machine: credential.NewFromPassword(strings.ToUpper(account)+"$", string(password))}
}

func (w *wire) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := w.dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.conns++
	return &wireConn{Conn: c, w: w, id: w.conns, calls: map[uint32]uint16{}}, nil
}

// requests is every request the client wrote of opnum.
func (w *wire) requests(opnum uint16) []sentPDU {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []sentPDU
	for _, p := range w.sent {
		if p.ptype == ptypeRequest && p.opnum == opnum {
			out = append(out, p)
		}
	}
	return out
}

// sealed is every PDU the client wrote under the Netlogon authentication.
func (w *wire) sealed() []sentPDU {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []sentPDU
	for _, p := range w.sent {
		if p.authType == authNetlogon {
			out = append(out, p)
		}
	}
	return out
}

// wireConn is one connection under the wire.
type wireConn struct {
	net.Conn
	w  *wire
	id int

	mu    sync.Mutex
	calls map[uint32]uint16 // each request's call ID to its opnum
	in    []byte            // the rest of a reply handed to Read
}

// auth is a PDU's authentication trailer, if any (C706 13.2.6.1).
func auth(pdu []byte) (authType, authLevel byte, stubEnd int) {
	fl, al := int(binary.LittleEndian.Uint16(pdu[8:])), int(binary.LittleEndian.Uint16(pdu[10:]))
	if al == 0 || fl < pduHeader+8+al {
		return 0, 0, fl
	}
	t := fl - al - 8
	return pdu[t], pdu[t+1], t
}

// Write takes one whole PDU, as go-msrpc writes each fragment.
func (c *wireConn) Write(b []byte) (int, error) {
	if len(b) < 16 || int(binary.LittleEndian.Uint16(b[8:])) != len(b) {
		return 0, fmt.Errorf("wire: a write of %d bytes that is not one PDU", len(b))
	}
	p := sentPDU{conn: c.id, ptype: b[2]}
	p.authType, p.authLevel, _ = auth(b)
	if p.ptype == ptypeRequest && len(b) >= pduHeader {
		p.opnum = binary.LittleEndian.Uint16(b[22:])
		_, _, end := auth(b)
		p.stub = bytes.Clone(b[pduHeader:end])
		if _, _, err := c.w.fault.At("request", []byte(opName(p.opnum))); err != nil {
			_ = c.Close()
			return 0, err
		}
		c.mu.Lock()
		c.calls[binary.LittleEndian.Uint32(b[12:])] = p.opnum
		c.mu.Unlock()
		if p.opnum == opRequestChallenge && p.authType == 0 {
			var req logon.RequestChallengeRequest
			if err := ndr.Unmarshal(p.stub, &req); err == nil && req.ClientChallenge != nil {
				c.w.mu.Lock()
				c.w.clientChal = req.ClientChallenge.Data
				c.w.mu.Unlock()
			}
		}
	}
	c.w.mu.Lock()
	c.w.sent = append(c.w.sent, p)
	c.w.mu.Unlock()
	return c.Conn.Write(b)
}

// Read hands over each reply whole, after the fault has had its say.
func (c *wireConn) Read(b []byte) (int, error) {
	if len(c.in) == 0 {
		pdu, err := c.readPDU()
		if err != nil {
			return 0, err
		}
		c.in = pdu
	}
	n := copy(b, c.in)
	c.in = c.in[n:]
	return n, nil
}

func (c *wireConn) readPDU() ([]byte, error) {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(c.Conn, hdr); err != nil {
		return nil, err
	}
	pdu := make([]byte, binary.LittleEndian.Uint16(hdr[8:]))
	copy(pdu, hdr)
	if len(pdu) < 16 {
		return nil, fmt.Errorf("wire: a PDU of %d bytes", len(pdu))
	}
	if _, err := io.ReadFull(c.Conn, pdu[16:]); err != nil {
		return nil, err
	}
	if pdu[2] != ptypeResponse || len(pdu) < pduHeader {
		return pdu, nil
	}
	c.mu.Lock()
	op := c.calls[binary.LittleEndian.Uint32(pdu[12:])]
	c.mu.Unlock()
	authType, _, end := auth(pdu)
	if op == opRequestChallenge && authType == 0 {
		var resp logon.RequestChallengeResponse
		if err := ndr.Unmarshal(pdu[pduHeader:end], &resp); err == nil && resp.ServerChallenge != nil {
			c.w.mu.Lock()
			c.w.serverChal = resp.ServerChallenge.Data
			c.w.mu.Unlock()
		}
	}
	_, _, err := c.w.fault.At("reply", []byte(opName(op)))
	var f flip
	switch {
	case errors.As(err, &f):
		if err := c.flip(pdu, f); err != nil {
			return nil, err
		}
	case err != nil:
		_ = c.Close()
		return nil, err
	}
	return pdu, nil
}

// flip applies f to the reply pdu's stub in place; a sealed one is unsealed
// with the session key the wire's challenges and password give, and sealed
// again at the sequence number and with the header signing it was sent with.
func (c *wireConn) flip(pdu []byte, f flip) error {
	authType, authLevel, end := auth(pdu)
	if authType == 0 {
		pdu[pduHeader+f.off] ^= f.mask
		return nil
	}
	if authType != authNetlogon || authLevel != authLevelPrivacy {
		return fmt.Errorf("wire: a flip of a reply under authentication %d at level %d", authType, authLevel)
	}
	ctx := context.Background()
	c.w.mu.Lock()
	key, err := nlssp.ComputeSessionKey(ctx, nlssp.CapAES_SHA2, c.w.machine, c.w.clientChal, c.w.serverChal)
	c.w.mu.Unlock()
	if err != nil {
		return err
	}
	server := nlcrypto.NewCipher(ctx, nlcrypto.EtypeAESSHA2, key, false)
	header, trailer, sig := pdu[:pduHeader], pdu[end:end+8], pdu[end+8:]
	signed := func(signHeader bool, stub []byte) [][]byte {
		if signHeader {
			return [][]byte{header, stub, trailer}
		}
		return [][]byte{stub}
	}
	for _, signHeader := range []bool{true, false} {
		for seq := uint64(0); seq < 64; seq++ {
			stub, s := bytes.Clone(pdu[pduHeader:end]), bytes.Clone(sig)
			want, err := server.Unwrap(ctx, seq, signed(signHeader, stub), [][]byte{stub}, s)
			if err != nil || !bytes.Equal(want[:24], s[:24]) {
				continue
			}
			stub[f.off] ^= f.mask
			resealed, err := server.Wrap(ctx, seq, signed(signHeader, stub), [][]byte{stub})
			if err != nil {
				return err
			}
			copy(pdu[pduHeader:end], stub)
			copy(sig, resealed)
			return nil
		}
	}
	return errors.New("wire: no sequence number unseals the reply")
}

// TestNetlogonWire holds the secure channel's own checks to the real
// controller through the wire: the server credential and the negotiated AES
// verified before anything else is sent, a logon retried only when its
// request never reached the wire, every logon sealed, and the password set's
// return authenticator verified.
func TestNetlogonWire(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	dcIP := setupADDC(t)
	waitForMachinePassword(t, dcIP, "dittofs", machineAccountPass)
	rec, ca := dcRecord(t)
	seed := &sambaDCSeeder{conn: adminLDAP(t, dcIP, ca)}

	// start stages a computer account and a user, and opens the controller
	// through a wire with rules.
	start := func(t *testing.T, rules ...sim.Rule) (netlogon.DC, *wire, string, []byte, string, []byte) {
		t.Helper()
		seed.Reset(t)
		t.Cleanup(func() { seed.Reset(t) })
		name, pw := "DCT"+randomHex(), []byte("Dc1!"+randomHex()+randomHex())
		seed.StageComputer(t, identitytest.StagedComputer{Name: name, Password: pw, EncTypes: 0x18})
		user, upw := "dcu"+randomHex(), []byte("Dc1!"+randomHex()+randomHex())
		seed.AddUser(t, user, upw)
		w := newWire(t, name, pw, rules...)
		dc, err := netlogon.NewDialer(w.DialContext).Dial(context.Background(), rec, dcIP)
		if err != nil {
			t.Fatalf("dial the DC: %v", err)
		}
		t.Cleanup(func() { _ = dc.Close() })
		return dc, w, name, pw, user, upw
	}
	ctx := context.Background()
	logonAs := func(ch netlogon.Channel, computer, user string, pw []byte) error {
		var challenge [8]byte
		_, _ = rand.Read(challenge[:])
		nt, lm := ntlmResponseFor(ctx, t, computer, user, string(pw), challenge)
		_, err := ch.Logon(ctx, netlogon.LogonRequest{Domain: adDomain, User: user, Workstation: "DCTWS",
			Challenge: challenge, NTResponse: nt, LMResponse: lm})
		return err
	}
	channel := func(t *testing.T, dc netlogon.DC, name string, pw []byte) netlogon.Channel {
		t.Helper()
		ch, err := dc.Channel(ctx, name, pw)
		if err != nil {
			t.Fatalf("secure channel as %s$: %v", name, err)
		}
		t.Cleanup(func() { _ = ch.Close() })
		return ch
	}
	// sealedAtPrivacy checks every logon and password set the client wrote
	// went under the Netlogon authentication at the privacy level.
	sealedAtPrivacy := func(t *testing.T, w *wire) {
		t.Helper()
		for _, op := range []uint16{opSamLogonEx, opPasswordSet2} {
			for _, p := range w.requests(op) {
				if p.authType != authNetlogon || p.authLevel != authLevelPrivacy {
					t.Errorf("%s on connection %d under authentication %d at level %d, want %d at %d, the seal",
						opName(op), p.conn, p.authType, p.authLevel, authNetlogon, authLevelPrivacy)
				}
			}
		}
	}

	t.Run("the channel offers no RC4 and every logon is sealed", func(t *testing.T) {
		dc, w, name, pw, user, upw := start(t)
		ch := channel(t, dc, name, pw)
		if err := logonAs(ch, name, user, upw); err != nil {
			t.Fatalf("logon of %s with its password: %v", user, err)
		}
		auths := w.requests(opAuthenticate3)
		if len(auths) != 1 {
			t.Fatalf("%d NetrServerAuthenticate3 requests, want 1", len(auths))
		}
		var req logon.Authenticate3Request
		if err := ndr.Unmarshal(auths[0].stub, &req); err != nil {
			t.Fatalf("decode the NetrServerAuthenticate3 request: %v", err)
		}
		if f := nlssp.Cap(req.NegotiateFlags); f.IsSet(nlssp.CapRC4) || !f.IsSet(nlssp.CapAES_SHA2) {
			t.Errorf("NetrServerAuthenticate3 offered flags %#08x, want AES (%#08x) without RC4 (%#08x)",
				req.NegotiateFlags, uint32(nlssp.CapAES_SHA2), uint32(nlssp.CapRC4))
		}
		if n := len(w.requests(opSamLogonEx)); n != 1 {
			t.Errorf("%d NetrLogonSamLogonEx requests for one logon, want 1", n)
		}
		sealedAtPrivacy(t, w)
	})

	// A controller that does not hold the password, or a reply altered on
	// the way, is refused before the channel sends anything else: no sealed
	// bind, so no NTLM response can follow.
	for _, tc := range []struct {
		name string
		flip flip
	}{
		// NetrServerAuthenticate3's reply: ServerCredential at 0, then
		// NegotiateFlags at 8, little-endian, AES_SHA2 being 0x01000000.
		{"a server credential that does not verify", flip{off: 0, mask: 0x01}},
		{"a reply negotiating no AES", flip{off: 8 + 3, mask: 0x01}},
	} {
		t.Run(tc.name+" refuses the channel", func(t *testing.T) {
			dc, w, name, pw, user, upw := start(t, sim.Rule{Op: "reply", Key: "Authenticate3", Count: 1,
				Action: sim.FaultAction{Err: tc.flip}})
			ch, err := dc.Channel(ctx, name, pw)
			if err == nil {
				_ = logonAs(ch, name, user, upw)
				_ = ch.Close()
				t.Errorf("secure channel after %s: set up, want refused", tc.name)
			}
			if got := w.sealed(); len(got) != 0 {
				t.Errorf("after %s the client sent %d PDUs under the Netlogon authentication, want none", tc.name, len(got))
			}
			if n := len(w.requests(opSamLogonEx)); n != 0 {
				t.Errorf("after %s %d NetrLogonSamLogonEx requests passed the wire, want none", tc.name, n)
			}
		})
	}

	t.Run("a logon torn after it was written is unavailable and counted once", func(t *testing.T) {
		dc, w, name, pw, user, _ := start(t, sim.Rule{Op: "reply", Key: "SamLogonEx", Count: 1,
			Action: sim.FaultAction{Err: errTear}})
		ch := channel(t, dc, name, pw)
		err := logonAs(ch, name, user, []byte("Wrong1!"+randomHex()))
		if !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("logon torn after it was written: err %v, want ErrUnavailable", err)
		}
		if n := len(w.requests(opSamLogonEx)); n != 1 {
			t.Errorf("%d NetrLogonSamLogonEx requests for a logon torn after it was written, want 1: never sent again", n)
		}
		if got := seed.BadPwdCount(t, user); got != 1 {
			t.Errorf("badPwdCount after one wrong password torn on the way back = %d, want 1", got)
		}
		sealedAtPrivacy(t, w)
	})

	t.Run("a logon torn before it was written is sent once on a new connection", func(t *testing.T) {
		dc, w, name, pw, user, upw := start(t, sim.Rule{Op: "request", Key: "SamLogonEx", Count: 1,
			Action: sim.FaultAction{Err: errTear}})
		ch := channel(t, dc, name, pw)
		first := w.sealed()
		if len(first) == 0 {
			t.Fatal("no sealed bind after the channel was set up")
		}
		if err := logonAs(ch, name, user, upw); err != nil {
			t.Fatalf("logon torn before it was written: %v, want it sent again and accepted", err)
		}
		logons := w.requests(opSamLogonEx)
		if len(logons) != 1 {
			t.Fatalf("%d NetrLogonSamLogonEx requests written, want 1: the torn one never reached the wire", len(logons))
		}
		if logons[0].conn == first[0].conn {
			t.Errorf("the logon went on connection %d, the torn one, want a new connection", logons[0].conn)
		}
		sealedAtPrivacy(t, w)
	})

	t.Run("a password set whose return authenticator does not verify fails", func(t *testing.T) {
		// NetrServerPasswordSet2's reply: ReturnAuthenticator's credential at 0.
		dc, w, name, pw, _, _ := start(t, sim.Rule{Op: "reply", Key: "PasswordSet2", Count: 1,
			Action: sim.FaultAction{Err: flip{off: 0, mask: 0x01}}})
		ch := channel(t, dc, name, pw)
		if err := ch.SetPassword(ctx, []byte("Dc1!"+randomHex()+randomHex())); err == nil {
			t.Error("SetPassword with a return authenticator altered under the seal: accepted, want an error")
		}
		if n := len(w.requests(opPasswordSet2)); n != 1 {
			t.Errorf("%d NetrServerPasswordSet2 requests, want 1", n)
		}
		sealedAtPrivacy(t, w)
	})
}

// randomHex is eight random hexadecimal digits.
func randomHex() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}
