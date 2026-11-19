package netlogon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf16"

	"github.com/oiweiwei/go-msrpc/dcerpc"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/crypto"
	nlssp "github.com/oiweiwei/go-msrpc/ssp/netlogon"
)

// offered is the capabilities NetrServerAuthenticate3 asks for: AES, the
// strong key and secure RPC. go-msrpc's own secure-channel client adds RC4,
// which is why the channel is built here from ssp/netlogon's parts.
const offered nlssp.Cap = nlssp.CapAES_SHA2 | nlssp.CapStrongKey | nlssp.CapSecureRPC

// errNoAES is a controller that negotiated the channel without AES.
var errNoAES = errors.New("netlogon: the domain controller negotiated no AES")

// errServerCredential is a controller whose server credential does not
// verify: it does not hold the password the client computed the session key
// from, or something altered its reply.
var errServerCredential = errors.New("netlogon: the domain controller's server credential does not verify")

// errServerChallenge is a NetrServerReqChallenge reply whose server challenge
// is missing, not 8 bytes, or one byte repeated.
var errServerChallenge = errors.New("netlogon: NetrServerReqChallenge answered no usable server challenge")

// checkServerChallenge refuses a server challenge that is not 8 bytes or is
// one byte repeated, all zero among them. The session-key negotiation
// (MS-NRPC §3.1.4.1) computes the session key (§3.1.4.3) and both
// credentials (§3.1.4.4) from it, so the client refuses one before computing
// any of them.
//
// decision: the server credential is the server challenge under AES-CFB8
// with a zero IV (§3.1.4.4.1), which encrypts 8 bytes to all zero only when
// they are 8 copies of the first byte of the session key's encryption of
// zero, so about 1 repeated-byte challenge in 256 has an all-zero
// credential, which matches an all-zero reply without the session key.
// Refusing every repeated-byte challenge leaves no such case; a controller's
// 8 random bytes are one byte repeated once in 2^56 set-ups.
func checkServerChallenge(c *logon.Credential) error {
	if c == nil || len(c.Data) != 8 || bytes.Count(c.Data, c.Data[:1]) == 8 {
		return errServerChallenge
	}
	return nil
}

// checkServerCredential refuses a server credential that is not want, the
// one the client computed (MS-NRPC §3.1.4.4), and refuses an all-zero one
// even when want is all zero too.
func checkServerCredential(want []byte, got *logon.Credential) error {
	// decision: checkServerChallenge leaves want never all zero, and the
	// all-zero refusal stays so this check does not rest on that one.
	if got == nil || allZero(got.Data) || !hmac.Equal(want, got.Data) {
		return errServerCredential
	}
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// checkAuthenticated checks a NetrServerAuthenticate3 reply to the client
// cred is, for serverChallenge: the negotiated AES first, as a controller
// without it computes no credential the client's AES one matches, then the
// server credential (MS-NRPC §3.1.4.4).
func checkAuthenticated(ctx context.Context, cred *nlssp.SecureCredential, serverChallenge []byte, reply *logon.Authenticate3Response) error {
	if err := checkNegotiated(reply.NegotiateFlags); err != nil {
		return err
	}
	want, err := cred.Encrypt(ctx, serverChallenge)
	if err != nil {
		return err
	}
	return checkServerCredential(want, reply.ServerCredential)
}

// checkNegotiated refuses the flags a NetrServerAuthenticate3 reply
// negotiated unless they hold AES.
func checkNegotiated(flags uint32) error {
	if !nlssp.Cap(flags).IsSet(nlssp.CapAES_SHA2) {
		return fmt.Errorf("%w: flags 0x%08x", errNoAES, flags)
	}
	return nil
}

// Channel sets up a secure channel as account: the challenge and
// NetrServerAuthenticate3 on an unauthenticated association, the server
// credential and the negotiated AES checked before anything else is sent,
// then a second association bound sealed with the Netlogon credential.
//
// ponytail: the channel reaches only the one directory's domain; RFC 18
// §5.1's ponytail says when to follow trusts.
func (d *dc) Channel(ctx context.Context, account string, password []byte) (Channel, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errClosed
	}
	name := strings.ToUpper(account)
	machine, err := machineCredential(name, d.realm, password)
	if err != nil {
		return nil, err
	}
	cfg, cred, err := d.authenticate(ctx, name, machine)
	if err != nil {
		return nil, err
	}
	as, err := d.seal(ctx, cfg, machine)
	if err != nil {
		return nil, err
	}
	return &channel{dc: d, name: name, cfg: cfg, machine: machine, cred: cred, as: as}, nil
}

// machineCredential is the computer account name$ keyed by its NT hash, so no
// string holds the password.
func machineCredential(name, realm string, password []byte) (credential.Credential, error) {
	codes := utf16.Encode([]rune(string(password)))
	b := make([]byte, 0, 2*len(codes))
	for _, c := range codes {
		b = append(b, byte(c), byte(c>>8))
	}
	hash, err := crypto.MD4(b)
	clear(b)
	if err != nil {
		return nil, err
	}
	// The schannel bind carries the domain, which Samba requires.
	return credential.NewFromNTHashBytes(name+"$", hash, credential.Workstation(name), credential.Domain(realm)), nil
}

// authenticate runs the challenge and NetrServerAuthenticate3 as name on an
// association of its own, closed before it returns.
func (d *dc) authenticate(ctx context.Context, name string, machine credential.Credential) (*nlssp.Config, *nlssp.SecureCredential, error) {
	cc, err := dcerpc.Dial(ctx, d.binding, dcerpc.WithDialer(d.dial))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = cc.Close(ctx) }()
	plain, err := logon.NewLogonClient(ctx, cc, dcerpc.WithInsecure())
	if err != nil {
		return nil, nil, err
	}
	cfg := &nlssp.Config{Capabilities: offered, Credential: machine, ClientChallenge: make([]byte, 8)}
	if _, err := rand.Read(cfg.ClientChallenge); err != nil {
		return nil, nil, err
	}
	chal := &logon.RequestChallengeResponse{}
	if err := invoke(ctx, plain.Conn(), &logon.RequestChallengeRequest{
		PrimaryName: d.server, ComputerName: name,
		ClientChallenge: &logon.Credential{Data: cfg.ClientChallenge},
	}, chal); err != nil {
		return nil, nil, err
	}
	if chal.Return != 0 {
		return nil, nil, Status(uint32(chal.Return))
	}
	if err := checkServerChallenge(chal.ServerChallenge); err != nil {
		return nil, nil, err
	}
	cfg.ServerChallenge = chal.ServerChallenge.Data
	cred, err := nlssp.NewSecureCredential(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	clientCred, err := cred.Encrypt(ctx, cfg.ClientChallenge)
	if err != nil {
		return nil, nil, err
	}
	auth3 := &logon.Authenticate3Response{}
	if err := invoke(ctx, plain.Conn(), &logon.Authenticate3Request{
		PrimaryName: d.server, AccountName: name + "$",
		SecureChannelType: logon.SecureChannelTypeWorkstationSecureChannel,
		ComputerName:      name,
		ClientCredential:  &logon.Credential{Data: clientCred},
		NegotiateFlags:    uint32(offered),
	}, auth3); err != nil {
		return nil, nil, err
	}
	if auth3.Return != 0 {
		return nil, nil, Status(uint32(auth3.Return))
	}
	if err := checkAuthenticated(ctx, cred, cfg.ServerChallenge, auth3); err != nil {
		return nil, nil, err
	}
	return cfg, cred, nil
}

// assoc is one sealed association and every byte written under it, so a
// failed call can tell whether its request reached the wire.
type assoc struct {
	cc      dcerpc.Conn
	cli     logon.LogonClient
	written atomic.Int64

	mu      sync.Mutex
	raw     []net.Conn
	aborted bool
}

// countConn counts the bytes written on a connection into its association.
type countConn struct {
	net.Conn
	written *atomic.Int64
}

func (c countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.written.Add(int64(n))
	return n, err
}

// dialer is dial with each connection counted and recorded.
func (a *assoc) dialer(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.aborted {
			_ = c.Close()
			return nil, net.ErrClosed
		}
		a.raw = append(a.raw, c)
		return countConn{Conn: c, written: &a.written}, nil
	}
}

// abort closes every connection the association dialed, which fails whatever
// waits on them.
func (a *assoc) abort() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.aborted = true
	for _, c := range a.raw {
		_ = c.Close()
	}
}

func (a *assoc) close() {
	_ = a.cc.Close(context.Background())
	a.abort()
}

// seal binds a new association sealed with the channel's Netlogon credential.
// go-msrpc ties an association's lifetime to the context it was bound under,
// so it is bound under one ctx's cancellation does not reach, and ctx bounds
// the bind by closing the connection.
func (d *dc) seal(ctx context.Context, cfg *nlssp.Config, machine credential.Credential) (*assoc, error) {
	a := &assoc{}
	stop := context.AfterFunc(ctx, a.abort)
	defer stop()
	life := context.WithoutCancel(ctx)
	cc, err := dcerpc.Dial(life, d.binding, dcerpc.WithDialer(a.dialer(d.dial)))
	if err != nil {
		a.abort()
		return nil, err
	}
	cli, err := logon.NewLogonClient(life, cc, dcerpc.WithSeal(), dcerpc.WithSecurityConfig(cfg),
		dcerpc.WithMechanism(nlssp.Mechanism{}), dcerpc.WithCredentials(machine))
	if err != nil {
		_ = cc.Close(life)
		a.abort()
		return nil, err
	}
	a.cc, a.cli = cc, cli
	if err := ctx.Err(); err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

// channel is a secure channel set up as one computer account. Its calls run
// one at a time.
//
// ponytail: one call in flight per channel, since go-msrpc's connection
// shares one buffer between concurrent calls. Upgrade to a pool of sealed
// associations under the channel's session key when logons queue behind each
// other in a profile.
type channel struct {
	dc      *dc
	name    string
	cfg     *nlssp.Config
	machine credential.Credential

	mu     sync.Mutex
	cred   *nlssp.SecureCredential // the authenticator chain NetrServerPasswordSet2 advances
	as     *assoc                  // nil once torn, until the next call binds another
	closed bool                    // set by Close: no association is bound again
}

// errChannelClosed is a call on a closed channel.
var errChannelClosed = errors.New("netlogon: secure channel closed")

// association is the channel's sealed association, binding a new one when
// the last was torn, none once the channel is closed. Nothing of a call is
// written before it returns.
func (c *channel) association(ctx context.Context) (*assoc, error) {
	if c.closed {
		return nil, errChannelClosed
	}
	if c.as == nil {
		as, err := c.dc.seal(ctx, c.cfg, c.machine)
		if err != nil {
			return nil, err
		}
		c.as = as
	}
	return c.as, nil
}

// drop closes the channel's association after a failed call.
func (c *channel) drop() {
	if c.as != nil {
		c.as.close()
		c.as = nil
	}
}

// Close closes the channel's association, and a call queued behind Close
// answers errChannelClosed rather than binding another.
func (c *channel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.drop()
	return nil
}
