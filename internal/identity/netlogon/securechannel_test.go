package netlogon

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"

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
