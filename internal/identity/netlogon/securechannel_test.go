package netlogon

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"

	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
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

// The server challenge is refused when missing, short or all zero, before
// any session key is computed from it.
func TestCheckServerChallenge(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *logon.Credential
		want error
	}{
		{"nonzero", &logon.Credential{Data: []byte{0, 0, 0, 0, 0, 0, 0, 1}}, nil},
		{"all zero", &logon.Credential{Data: make([]byte, 8)}, errServerChallenge},
		{"short", &logon.Credential{Data: []byte{1, 2, 3, 4, 5, 6, 7}}, errServerChallenge},
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
