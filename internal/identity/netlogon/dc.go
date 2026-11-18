package netlogon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dcetypes"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	epm "github.com/oiweiwei/go-msrpc/msrpc/epm/epm/v3"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/identity"
)

// dialFunc opens a TCP connection, as net.Dialer's DialContext does; it is
// go-msrpc's Dialer.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f dialFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

// NewDialer is the domain controller's client. dial opens every connection
// it makes, to the endpoint mapper and to NETLOGON; production passes
// (&net.Dialer{}).DialContext.
func NewDialer(dial func(ctx context.Context, network, addr string) (net.Conn, error)) Dialer {
	return &dialer{dial: dial}
}

type dialer struct{ dial dialFunc }

// Lookup is not built yet: it answers errors.ErrUnsupported.
func (d *dialer) Lookup(ctx context.Context, realm, dnsServer string) ([]string, error) {
	return nil, errors.ErrUnsupported
}

// Dial asks the endpoint mapper on addr's port 135 for NETLOGON's
// ncacn_ip_tcp port; every later connection goes to that port.
//
// decision: NETLOGON runs on ncacn_ip_tcp sealed with schannel, never over
// an SMB named pipe, so no SMB session and no Kerberos client key is needed,
// and an offline join can set its first closing password over a channel the
// handed-over password authenticates. Samba accepts it; v1 moved to the named
// pipe after a refused alter context that no longer reproduces. Withdraw it
// for a controller that refuses schannel on ncacn_ip_tcp.
func (d *dialer) Dial(ctx context.Context, rec directory.Record, addr string) (DC, error) {
	port, err := netlogonPort(ctx, d.dial, addr)
	if err != nil {
		return nil, fmt.Errorf("netlogon: endpoint mapper on %s: %w", addr, err)
	}
	return &dc{
		dial:    d.dial,
		binding: "ncacn_ip_tcp:" + addr + "[" + port + "]",
		server:  logonServer(addr),
		realm:   rec.Realm,
	}, nil
}

// netlogonPort is the ncacn_ip_tcp port addr's endpoint mapper names for
// NETLOGON.
func netlogonPort(ctx context.Context, dial dialFunc, addr string) (string, error) {
	cc, err := dcerpc.Dial(ctx, "ncacn_ip_tcp:"+addr+"[135]", dcerpc.WithDialer(dial))
	if err != nil {
		return "", err
	}
	defer func() { _ = cc.Close(ctx) }()
	cli, err := epm.NewEpmClient(ctx, cc, dcerpc.WithInsecure())
	if err != nil {
		return "", err
	}
	resp, err := cli.Lookup(ctx, &epm.LookupRequest{
		InquiryType: 1, // RPC_C_EP_MATCH_BY_IF
		InterfaceID: &dcetypes.InterfaceID{UUID: dtyp.GUIDFromUUID(logon.LogonSyntaxUUID), VersMajor: 1},
		MaxEntries:  500,
	})
	if err != nil {
		return "", err
	}
	// Samba answers every endpoint it holds, so NETLOGON's is picked out by
	// the interface its tower names.
	for _, e := range resp.Entries {
		b := e.Tower.Binding()
		if b.SyntaxID.IfUUID.String() == logon.LogonSyntaxUUID.String() &&
			b.StringBinding.ProtocolSequence == dcerpc.ProtocolSequenceIPTCP {
			return b.StringBinding.Endpoint, nil
		}
	}
	return "", errors.New("no ncacn_ip_tcp endpoint for NETLOGON")
}

// logonServer is the controller's name as NETLOGON's calls carry it: the
// first label of a host name in upper case, or an address as given. The
// controller authenticates the caller by the channel's credential, not by
// this name.
func logonServer(addr string) string {
	if net.ParseIP(addr) != nil {
		return `\\` + addr
	}
	host, _, _ := strings.Cut(addr, ".")
	return `\\` + strings.ToUpper(host)
}

// dc is one domain controller reached through its NETLOGON port.
type dc struct {
	dial    dialFunc
	binding string
	server  string
	realm   string

	mu     sync.Mutex // serialises channel set-up: the controller keeps one credential per account
	closed bool
}

var errClosed = errors.New("netlogon: domain controller closed")

// Account is not built yet: it answers errors.ErrUnsupported.
func (d *dc) Account(ctx context.Context, bind identity.JoinCredential, name string) (Account, error) {
	return Account{}, errors.ErrUnsupported
}

// Create is not built yet: it answers errors.ErrUnsupported.
func (d *dc) Create(ctx context.Context, admin identity.JoinCredential, c Computer, password []byte) error {
	return errors.ErrUnsupported
}

// Reset is not built yet: it answers errors.ErrUnsupported.
func (d *dc) Reset(ctx context.Context, admin identity.JoinCredential, name string, password []byte) error {
	return errors.ErrUnsupported
}

// Close refuses every later Channel; a channel already set up keeps working
// until its own Close.
func (d *dc) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}
