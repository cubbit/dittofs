package netlogon

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"testing"

	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	nlssp "github.com/oiweiwei/go-msrpc/ssp/netlogon"
)

// A call that reaches a channel after its Close, as one queued on the
// channel's lock behind Close does, answers errChannelClosed and binds no new
// association: the controller's port here is closed, so a bind would answer
// the refused connection instead.
func TestChannelClosedBindsNoAssociation(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().(*net.TCPAddr)
	_ = l.Close()
	d := &dc{dial: (&net.Dialer{}).DialContext, binding: "ncacn_ip_tcp:127.0.0.1[" + strconv.Itoa(addr.Port) + "]", server: `\\DC1`}
	machine, err := machineCredential("NAS", "CORP.EXAMPLE", []byte("Nas-Passw0rd-2"))
	if err != nil {
		t.Fatal(err)
	}
	c := &channel{dc: d, name: "NAS", cfg: &nlssp.Config{}, machine: machine}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Logon(ctx, LogonRequest{User: "alice"}); !errors.Is(err, errChannelClosed) {
		t.Errorf("Logon after Close: %v, want errChannelClosed", err)
	}
	if err := c.SetPassword(ctx, []byte("Nas-Passw0rd-3")); !errors.Is(err, errChannelClosed) {
		t.Errorf("SetPassword after Close: %v, want errChannelClosed", err)
	}
	if c.as != nil {
		t.Errorf("association after Close: bound, want none")
	}
}

// The server challenge is refused when missing, not 8 bytes, or one byte
// repeated, before any session key is computed from it.
func TestCheckServerChallenge(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *logon.Credential
		want error
	}{
		{"nonzero", &logon.Credential{Data: []byte{0, 0, 0, 0, 0, 0, 0, 1}}, nil},
		{"one byte repeated but the last", &logon.Credential{Data: []byte{7, 7, 7, 7, 7, 7, 7, 8}}, nil},
		{"all zero", &logon.Credential{Data: make([]byte, 8)}, errServerChallenge},
		{"one byte repeated", &logon.Credential{Data: bytes.Repeat([]byte{7}, 8)}, errServerChallenge},
		{"short", &logon.Credential{Data: []byte{1, 2, 3, 4, 5, 6, 7}}, errServerChallenge},
		{"long", &logon.Credential{Data: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}}, errServerChallenge},
		{"missing", nil, errServerChallenge},
	} {
		if err := checkServerChallenge(tc.c); !errors.Is(err, tc.want) {
			t.Errorf("%s: checkServerChallenge = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// The server credential must equal the one the client computed, and an
// all-zero one is refused even when the computed one is all zero too.
func TestCheckServerCredential(t *testing.T) {
	cred := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	zero := make([]byte, 8)
	for _, tc := range []struct {
		name string
		want []byte
		got  *logon.Credential
		err  error
	}{
		{"matches", cred, &logon.Credential{Data: cred}, nil},
		{"differs", cred, &logon.Credential{Data: []byte{1, 2, 3, 4, 5, 6, 7, 9}}, errServerCredential},
		{"all zero, computed all zero", zero, &logon.Credential{Data: zero}, errServerCredential},
		{"all zero, computed nonzero", cred, &logon.Credential{Data: zero}, errServerCredential},
		{"missing", cred, nil, errServerCredential},
	} {
		if err := checkServerCredential(tc.want, tc.got); !errors.Is(err, tc.err) {
			t.Errorf("%s: checkServerCredential = %v, want %v", tc.name, err, tc.err)
		}
	}
}

// A NetrServerAuthenticate3 reply is checked for AES before its server
// credential, and its credential is checked as checkServerCredential checks
// it: an all-zero one is refused when the client computed all zero too.
func TestCheckAuthenticated(t *testing.T) {
	ctx := context.Background()
	zeroCfg, zeroCred := zeroComputedCredential(t)
	cfg := &nlssp.Config{Capabilities: offered, Credential: zeroCfg.Credential,
		ClientChallenge: zeroCfg.ClientChallenge, ServerChallenge: []byte{1, 2, 3, 4, 5, 6, 7, 8}}
	cred, err := nlssp.NewSecureCredential(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	server, err := cred.Encrypt(ctx, cfg.ServerChallenge)
	if err != nil {
		t.Fatal(err)
	}
	zero := &logon.Credential{Data: make([]byte, 8)}
	noAES := uint32(nlssp.CapStrongKey | nlssp.CapSecureRPC)
	for _, tc := range []struct {
		name  string
		cred  *nlssp.SecureCredential
		chal  []byte
		reply *logon.Authenticate3Response
		want  error
	}{
		{"verifies", cred, cfg.ServerChallenge, &logon.Authenticate3Response{ServerCredential: &logon.Credential{Data: server}, NegotiateFlags: uint32(offered)}, nil},
		{"all zero, computed all zero", zeroCred, zeroCfg.ServerChallenge, &logon.Authenticate3Response{ServerCredential: zero, NegotiateFlags: uint32(offered)}, errServerCredential},
		{"no AES, credential does not verify", cred, cfg.ServerChallenge, &logon.Authenticate3Response{ServerCredential: zero, NegotiateFlags: noAES}, errNoAES},
	} {
		if err := checkAuthenticated(ctx, tc.cred, tc.chal, tc.reply); !errors.Is(err, tc.want) {
			t.Errorf("%s: checkAuthenticated = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// zeroComputedCredential returns a configuration and its credential whose
// server credential the client computes as all zero: under AES-CFB8 with a
// zero IV, a server challenge of 8 copies of the first byte of the session
// key's encryption of zero encrypts to zero. It walks client challenges in
// order, so the pair it finds is the same each run.
func zeroComputedCredential(t *testing.T) (*nlssp.Config, *nlssp.SecureCredential) {
	ctx := context.Background()
	machine := credential.NewFromPassword("DITTOFS$", "password", credential.Workstation("DITTOFS"), credential.Domain("EXAMPLE"))
	for i := range 1 << 16 {
		client := []byte{byte(i), byte(i >> 8), 1, 2, 3, 4, 5, 6}
		for c := range 256 {
			cfg := &nlssp.Config{Capabilities: offered, Credential: machine,
				ClientChallenge: client, ServerChallenge: bytes.Repeat([]byte{byte(c)}, 8)}
			cred, err := nlssp.NewSecureCredential(ctx, cfg)
			if err != nil {
				t.Fatalf("NewSecureCredential: %v", err)
			}
			want, err := cred.Encrypt(ctx, cfg.ServerChallenge)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if allZero(want) {
				return cfg, cred
			}
		}
	}
	t.Fatal("no repeated-byte server challenge computed an all-zero credential")
	return nil, nil
}
