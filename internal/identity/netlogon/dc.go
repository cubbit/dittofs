package netlogon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"strconv"
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

// Lookup answers the controllers of realm's domain from its DC locator
// record, _ldap._tcp.dc._msdcs.<realm>, in the order RFC 2782 gives them:
// by priority, then randomised by weight. With dnsServer, a host or
// host:port, set, the query goes to it alone, through dial; a domain's
// controllers are normally its DNS servers.
func (d *dialer) Lookup(ctx context.Context, realm, dnsServer string) ([]string, error) {
	realm = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(realm)), ".")
	if realm == "" {
		return nil, errors.New("netlogon: look up controllers: empty realm")
	}
	resolver := net.DefaultResolver
	if dnsServer != "" {
		if _, _, err := net.SplitHostPort(dnsServer); err != nil {
			dnsServer = net.JoinHostPort(dnsServer, "53")
		}
		resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.dial(ctx, network, dnsServer)
		}}
	}
	_, srvs, err := resolver.LookupSRV(ctx, "ldap", "tcp", "dc._msdcs."+realm)
	if err != nil {
		return nil, fmt.Errorf("netlogon: look up _ldap._tcp.dc._msdcs.%s: %w", realm, err)
	}
	addrs := make([]string, 0, len(srvs))
	for _, s := range srvs {
		addrs = append(addrs, strings.TrimSuffix(s.Target, "."))
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("netlogon: _ldap._tcp.dc._msdcs.%s names no controller", realm)
	}
	return addrs, nil
}

// Dial opens the controller at addr: its NETLOGON calls go to the
// ncacn_ip_tcp port addr's endpoint mapper on port 135 names, asked at the
// first Channel, and its account calls over LDAP to addr, under rec's scheme,
// port and trust roots.
//
// decision: NETLOGON runs on ncacn_ip_tcp sealed with schannel, never over
// an SMB named pipe, so no SMB session and no Kerberos client key is needed,
// and an offline join can set its first closing password over a channel the
// handed-over password authenticates. Samba accepts it; v1 moved to the named
// pipe after a refused alter context that no longer reproduces. Withdraw it
// for a controller that refuses schannel on ncacn_ip_tcp.
func (d *dialer) Dial(ctx context.Context, rec directory.Record, addr string) (DC, error) {
	return &dc{
		dial:   d.dial,
		addr:   addr,
		server: logonServer(addr),
		realm:  rec.Realm,
		ldap:   ldapRecord(rec, addr),
	}, nil
}

// ldapRecord is rec with its address naming addr, the pinned controller,
// under rec's scheme and port, so the account calls reach the controller the
// channel does, verified against rec's trust roots.
func ldapRecord(rec directory.Record, addr string) directory.Record {
	u, err := url.Parse(rec.Address)
	if err != nil {
		return rec // Validate refuses it at the first account call
	}
	host := addr
	if strings.Contains(addr, ":") {
		host = "[" + addr + "]"
	}
	if port := u.Port(); port != "" {
		host = net.JoinHostPort(addr, port)
	}
	u.Host = host
	rec.Address = u.String()
	return rec
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
	resp := &epm.LookupResponse{}
	if err := invoke(ctx, cli.Conn(), &epm.LookupRequest{
		InquiryType: 1, // RPC_C_EP_MATCH_BY_IF
		InterfaceID: &dcetypes.InterfaceID{UUID: dtyp.GUIDFromUUID(logon.LogonSyntaxUUID), VersMajor: 1},
		MaxEntries:  500,
	}, resp); err != nil {
		return "", err
	}
	// Samba answers every endpoint it holds, so NETLOGON's is picked out by
	// the interface its tower names.
	for _, e := range resp.Entries {
		if e == nil {
			continue
		}
		if port, ok := netlogonEndpoint(e.Tower); ok {
			return port, nil
		}
	}
	return "", errors.New("no ncacn_ip_tcp endpoint for NETLOGON")
}

// netlogonEndpoint is the ncacn_ip_tcp port tower names for NETLOGON, and
// whether it names one: a TCP port number, or none. go-msrpc's tower parsing panics on a floor shorter
// than its protocol's data, and a nil tower is a nil dereference; the
// unauthenticated endpoint mapper's reply can carry either, so such a tower
// names none.
func netlogonEndpoint(tower *dcetypes.Tower) (port string, ok bool) {
	err := decoded(func() error {
		b := tower.Binding()
		ok = b.SyntaxID.IfUUID.String() == logon.LogonSyntaxUUID.String() &&
			b.StringBinding.ProtocolSequence == dcerpc.ProtocolSequenceIPTCP
		port = b.StringBinding.Endpoint
		return nil
	})
	if err != nil || !ok {
		return "", false
	}
	// A later floor of another protocol overwrites the TCP floor's endpoint
	// with its own string, which is no port.
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", false
	}
	return strconv.FormatUint(n, 10), true
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
	dial   dialFunc
	addr   string
	server string
	realm  string
	ldap   directory.Record

	mu      sync.Mutex // serialises channel set-up: the controller keeps one credential per account
	binding string     // NETLOGON's binding, once the endpoint mapper named it
	closed  bool
}

var errClosed = errors.New("netlogon: domain controller closed")

// netlogonBinding is NETLOGON's binding on the controller, asking the
// endpoint mapper the first time. d.mu is held.
func (d *dc) netlogonBinding(ctx context.Context) (string, error) {
	if d.binding == "" {
		port, err := netlogonPort(ctx, d.dial, d.addr)
		if err != nil {
			return "", fmt.Errorf("netlogon: endpoint mapper on %s: %w", d.addr, err)
		}
		d.binding = "ncacn_ip_tcp:" + d.addr + "[" + port + "]"
	}
	return d.binding, nil
}

func (d *dc) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// Account reads the computer account over LDAP as bind.
func (d *dc) Account(ctx context.Context, bind identity.JoinCredential, name string) (Account, error) {
	if d.isClosed() {
		return Account{}, errClosed
	}
	a, err := directory.ReadComputer(ctx, d.ldap, bind.User, bind.Password, name)
	if errors.Is(err, fs.ErrNotExist) {
		return Account{}, fmt.Errorf("%w: %w", ErrNoAccount, err)
	}
	if err != nil {
		return Account{}, err
	}
	return Account(a), nil
}

// Create creates the computer account over LDAP as admin.
func (d *dc) Create(ctx context.Context, admin identity.JoinCredential, c Computer, password []byte) error {
	if d.isClosed() {
		return errClosed
	}
	return directory.CreateComputer(ctx, d.ldap, admin.User, admin.Password, directory.NewComputer(c), password)
}

// Reset sets the computer account's password over LDAP as admin.
func (d *dc) Reset(ctx context.Context, admin identity.JoinCredential, name string, password []byte) error {
	if d.isClosed() {
		return errClosed
	}
	err := directory.ResetComputer(ctx, d.ldap, admin.User, admin.Password, name, password)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrNoAccount, err)
	}
	return err
}

// Close refuses every later call; a channel already set up keeps working
// until its own Close.
func (d *dc) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}
