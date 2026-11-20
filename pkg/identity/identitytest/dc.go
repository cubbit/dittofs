package identitytest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
)

// hostMappings is the services Active Directory's default sPNMappings maps to
// host, as a domain is provisioned with them.
var hostMappings = strings.Split("alerter,appmgmt,cisvc,clipsrv,browser,dhcp,dnscache,replicator,"+
	"eventlog,eventsystem,policyagent,oakley,dmserver,dns,mcsvc,fax,msiserver,ias,messenger,netlogon,"+
	"netman,netdde,netddedsm,nmagent,plugplay,protectedstorage,rasman,rpclocator,rpc,rpcss,"+
	"remoteaccess,rsvp,samss,scardsvr,scesrv,seclogon,scm,dcom,cifs,spooler,snmp,schedule,tapisrv,"+
	"trksvr,trkwks,ups,time,wins,www,http,w3svc,iisadmin,msdtc", ",")

// The account flags and group attributes the controller answers with.
const (
	workstationTrust = 0x1000 // userAccountControl's WORKSTATION_TRUST_ACCOUNT
	groupAttrs       = 0x7    // SE_GROUP_MANDATORY | ENABLED_BY_DEFAULT | ENABLED
	ridDomainAdmins  = 512
	ridDomainUsers   = 513
)

// DomainController is an in-memory Active Directory domain of one or more
// domain controllers, answering netlogon's seam with results:
// DomainControllerConformance holds it to a real controller. It holds users
// with their passwords, computer accounts with their password, names, owner,
// delegation value and msDS-KeyVersionNumber, 1 for an account created with
// its password and raised by one per set, and a badPwdCount per user and
// controller, which no controller replicates. It checks NTLMv2 itself
// (MS-NLMP 3.3.2). A secure channel is set up with an account's current
// password only, as Samba sets up a workstation's; a controller that has not
// replicated a set still holds the old password as current. Each controller holds its own copy
// of the accounts; a write at one reaches the others at once, except a
// controller whose replication is held, which sees it only once released.
type DomainController struct {
	netbios, realm, base string
	domainSID            sid.SID

	mu        sync.Mutex
	dcs       []*controller // in the order Lookup answers them
	dialled   *controller   // the one the seeder acts at: the last dialled, the first before any dial
	users     map[string]*dcUser
	computers string // the Computers container wellKnownObjects names
	ous       map[string]bool
	nextRID   uint32
}

type dcUser struct {
	dn       string
	rid      uint32
	password []byte
}

type controller struct {
	addr     string
	accounts map[string]*dcAccount // by name, without the $, lower-cased
	badPwd   map[string]int        // by user name, lower-cased
	held     bool
	pending  []func(map[string]*dcAccount)
}

type dcAccount struct {
	netlogon.Account
	rid      uint32
	spns     []string
	password []byte
}

func (a *dcAccount) clone() *dcAccount {
	c := *a
	c.spns = slices.Clone(a.spns)
	c.password = slices.Clone(a.password)
	c.Delegation = slices.Clone(a.Delegation)
	return &c
}

// adminPasswordDC is the in-memory domain's administrator's password.
const adminPasswordDC = "in-memory domain administrator"

// NewDomainController returns a domain named netbios and realm, whose naming
// context is base, with a random domain SID, an Administrator, and one
// controller at each of addrs, which Lookup answers in that order.
func NewDomainController(netbios, realm, base string, addrs ...string) *DomainController {
	d := &DomainController{
		netbios: netbios, realm: realm, base: base,
		domainSID: sid.SID{Revision: 1, SubAuthorityCount: 4, IdentifierAuthority: [6]byte{5: 5},
			SubAuthorities: []uint32{21, randUint32(), randUint32(), randUint32()}},
		users:     map[string]*dcUser{},
		computers: "CN=Computers," + base,
		nextRID:   1100,
	}
	d.ous = map[string]bool{strings.ToLower(d.computers): true}
	for _, a := range addrs {
		d.dcs = append(d.dcs, &controller{addr: a, accounts: map[string]*dcAccount{}, badPwd: map[string]int{}})
	}
	d.dialled = d.dcs[0]
	d.users["administrator"] = &dcUser{dn: "CN=Administrator,CN=Users," + base, rid: 500, password: []byte(adminPasswordDC)}
	return d
}

func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint32(b[:])
}

func (d *DomainController) sidOf(rid uint32) sid.SID {
	s := d.domainSID
	s.SubAuthorityCount++
	s.SubAuthorities = append(slices.Clone(s.SubAuthorities), rid)
	return s
}

// UseDomainSID makes s the domain's SID, as when the domain a Directory
// serves is the same one, before any account is staged or user added.
func (d *DomainController) UseDomainSID(_ testing.TB, s sid.SID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.domainSID = s
	d.domainSID.SubAuthorities = slices.Clone(s.SubAuthorities)
}

// HoldReplication stops the controller at addr seeing writes made at the
// others until ReleaseReplication, so it answers an account as it was.
func (d *DomainController) HoldReplication(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.controller(addr).held = true
}

// ReleaseReplication applies to the controller at addr every write it missed
// while held, in order, and resumes its replication.
func (d *DomainController) ReleaseReplication(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.controller(addr)
	for _, f := range c.pending {
		f(c.accounts)
	}
	c.pending, c.held = nil, false
}

func (d *DomainController) controller(addr string) *controller {
	for _, c := range d.dcs {
		if c.addr == addr {
			return c
		}
	}
	panic("identitytest: no controller at " + addr)
}

// write applies f at origin, then at every other controller, or keeps it for
// one whose replication is held. f must store accounts of its own, never one
// another controller's map holds. d.mu is held.
func (d *DomainController) write(origin *controller, f func(map[string]*dcAccount)) {
	f(origin.accounts)
	for _, c := range d.dcs {
		switch {
		case c == origin:
		case c.held:
			c.pending = append(c.pending, f)
		default:
			f(c.accounts)
		}
	}
}

// names is the account's servicePrincipalName with sPNMappings applied.
func names(spns []string) []string {
	var out []string
	add := func(n string) {
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, n) }) {
			out = append(out, n)
		}
	}
	for _, n := range spns {
		add(n)
		if svc, host, ok := strings.Cut(n, "/"); ok && strings.EqualFold(svc, "host") {
			for _, m := range hostMappings {
				add(m + "/" + host)
			}
		}
	}
	return out
}

// Lookup is a netlogon.Dialer's: the controllers in the order built, for
// the domain's realm; dnsServer is not read.
func (d *DomainController) Lookup(ctx context.Context, realm, _ string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !strings.EqualFold(realm, d.realm) {
		return nil, fmt.Errorf("identitytest: no domain controllers for %q", realm)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var addrs []string
	for _, c := range d.dcs {
		addrs = append(addrs, c.addr)
	}
	return addrs, nil
}

// Dial is a netlogon.Dialer's, opening the controller at addr, which the
// seeder then acts at; rec is not read. An address the domain has no
// controller at could not be asked.
func (d *DomainController) Dial(ctx context.Context, _ directory.Record, addr string) (netlogon.DC, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.dcs {
		if c.addr == addr {
			d.dialled = c
			return &memDC{d: d, c: c}, nil
		}
	}
	return nil, fmt.Errorf("identitytest: no domain controller answers at %s", addr)
}

var _ netlogon.Dialer = (*DomainController)(nil)

// memDC is one controller of a DomainController, opened.
type memDC struct {
	d *DomainController
	c *controller

	mu     sync.Mutex
	closed bool
}

var errDCClosed = errors.New("identitytest: the domain controller connection is closed")

func (m *memDC) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.d.mu.Lock()
	gone := !slices.Contains(m.d.dcs, m.c)
	m.d.mu.Unlock()
	if gone {
		return fmt.Errorf("identitytest: no domain controller answers at %s", m.c.addr)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errDCClosed
	}
	return nil
}

// bind checks cred, and with admin that it is the Administrator's bind, by DN or UPN. d.mu is held.
func (d *DomainController) bind(cred identity.JoinCredential, admin bool) error {
	for name, u := range d.users {
		if (strings.EqualFold(cred.User, u.dn) || strings.EqualFold(cred.User, name+"@"+d.realm)) &&
			len(cred.Password) > 0 && bytes.Equal(cred.Password, u.password) {
			if admin && u.rid != 500 {
				return fmt.Errorf("identitytest: %q may not write computer accounts: %w", cred.User, netlogon.ErrRejected)
			}
			return nil
		}
	}
	return fmt.Errorf("identitytest: bind as %q: invalid credentials: %w", cred.User, netlogon.ErrRejected)
}

func (m *memDC) Channel(ctx context.Context, account string, password []byte) (netlogon.Channel, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	d := m.d
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := m.c.accounts[strings.ToLower(account)]
	// decision: the channel takes the account's current password alone, not
	// its previous one too, because Samba refuses a workstation's previous
	// password at NetrServerAuthenticate3, after an administrator's reset and
	// after a set over the channel alike, and the suite holds this controller
	// to Samba. Accept the previous one as well if a controller the suite runs
	// against, Windows Server's included, is shown to.
	switch {
	case !ok:
		return nil, netlogon.StatusNoTrustSAMAccount
	case len(password) == 0:
		return nil, netlogon.StatusAccessDenied
	case bytes.Equal(password, a.password):
		return &memChannel{dc: m, account: strings.ToLower(account)}, nil
	}
	return nil, netlogon.StatusAccessDenied
}

func (m *memDC) Account(ctx context.Context, bind identity.JoinCredential, name string) (netlogon.Account, error) {
	if err := m.check(ctx); err != nil {
		return netlogon.Account{}, err
	}
	d := m.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.bind(bind, false); err != nil {
		return netlogon.Account{}, err
	}
	a, ok := m.c.accounts[strings.ToLower(name)]
	if !ok {
		return netlogon.Account{}, fmt.Errorf("%s$: %w", name, netlogon.ErrNoAccount)
	}
	out := a.clone().Account
	out.Names = names(a.spns)
	return out, nil
}

func (m *memDC) Create(ctx context.Context, admin identity.JoinCredential, c netlogon.Computer, password []byte) error {
	if err := m.check(ctx); err != nil {
		return err
	}
	d := m.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.bind(admin, true); err != nil {
		return err
	}
	ou := c.OU
	if ou == "" {
		ou = d.computers
	}
	if !d.ous[strings.ToLower(ou)] {
		return fmt.Errorf("identitytest: no such object %q", ou)
	}
	// ponytail: the name is checked at this controller alone, so while
	// replication is held a second Create of one name at another controller
	// succeeds and replaces the first account at every controller not held.
	// Check the held controllers' pending writes too when a test creates one
	// name at two controllers.
	if _, ok := m.c.accounts[strings.ToLower(c.Name)]; ok {
		return fmt.Errorf("%s$: %w", c.Name, fs.ErrExist)
	}
	d.putAccount(m.c, c.Name, "CN="+c.Name+","+ou, password, 0x18,
		[]string{"HOST/" + c.Name, "HOST/" + c.DNSHostName, "cifs/" + c.Name, "cifs/" + c.DNSHostName})
	return nil
}

// putAccount creates an account at origin, owned by Domain Admins, key
// version 1. d.mu is held.
func (d *DomainController) putAccount(origin *controller, name, dn string, password []byte, enc uint32, spns []string) {
	a := &dcAccount{
		Account: netlogon.Account{DN: dn, KeyVersion: 1, EncTypes: enc, Owner: d.sidOf(ridDomainAdmins)},
		rid:     d.nextRID, spns: spns, password: slices.Clone(password),
	}
	d.nextRID++
	key := strings.ToLower(name)
	d.write(origin, func(m map[string]*dcAccount) { m[key] = a.clone() })
}

// setPassword makes password the account's, raising its key version by one.
// d.mu is held.
func (d *DomainController) setPassword(origin *controller, name string, password []byte) error {
	key := strings.ToLower(name)
	if _, ok := origin.accounts[key]; !ok {
		return fmt.Errorf("%s$: %w", name, netlogon.ErrNoAccount)
	}
	pw := slices.Clone(password)
	d.write(origin, func(m map[string]*dcAccount) {
		if a, ok := m[key]; ok {
			a.password = slices.Clone(pw)
			a.KeyVersion++
		}
	})
	return nil
}

func (m *memDC) Reset(ctx context.Context, admin identity.JoinCredential, name string, password []byte) error {
	if err := m.check(ctx); err != nil {
		return err
	}
	d := m.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.bind(admin, true); err != nil {
		return err
	}
	return d.setPassword(m.c, name, password)
}

func (m *memDC) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// memChannel is a secure channel to a memDC.
type memChannel struct {
	dc      *memDC
	account string

	mu     sync.Mutex
	closed bool
}

func (ch *memChannel) check(ctx context.Context) error {
	if err := ch.dc.check(ctx); err != nil {
		return err
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed {
		return errDCClosed
	}
	return nil
}

// Logon checks the response as the controller's SAM does: the password
// first, counting a wrong one, then whether the account may sign in, so a
// computer account's right password is refused as one.
func (ch *memChannel) Logon(ctx context.Context, r netlogon.LogonRequest) (netlogon.Reply, error) {
	if err := ch.check(ctx); err != nil {
		return netlogon.Reply{}, err
	}
	d, c := ch.dc.d, ch.dc.c
	d.mu.Lock()
	defer d.mu.Unlock()
	user := strings.ToLower(r.User)
	var password []byte
	var rid uint32
	computer := false
	if u, ok := d.users[user]; ok {
		password, rid = u.password, u.rid
	} else if a, ok := c.accounts[strings.TrimSuffix(user, "$")]; ok && strings.HasSuffix(user, "$") {
		password, rid, computer = a.password, a.rid, true
	} else {
		return netlogon.Reply{}, netlogon.StatusNoSuchUser
	}
	key, ok := checkNTLMv2(password, r.User, r.Domain, r.Challenge, r.NTResponse)
	if !ok {
		c.badPwd[user]++
		return netlogon.Reply{}, netlogon.StatusWrongPassword
	}
	c.badPwd[user] = 0
	if computer {
		return netlogon.Reply{}, netlogon.StatusNologonWorkstationTrust
	}
	return netlogon.Reply{
		User:        d.sidOf(rid),
		Groups:      []netlogon.SIDAttr{{SID: d.sidOf(ridDomainUsers), Attrs: groupAttrs}},
		SessionKey:  key,
		LogonDomain: d.domain(),
	}, nil
}

func (ch *memChannel) SetPassword(ctx context.Context, password []byte) error {
	if err := ch.check(ctx); err != nil {
		return err
	}
	d := ch.dc.d
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.setPassword(ch.dc.c, ch.account, password)
}

func (ch *memChannel) Close() error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.closed = true
	return nil
}

func (d *DomainController) domain() sid.SID {
	s := d.domainSID
	s.SubAuthorities = slices.Clone(s.SubAuthorities)
	return s
}

// The DCSeeder.

var _ DCSeeder = (*DomainController)(nil)

// Domain names the domain.
func (d *DomainController) Domain() DCDomain {
	return DCDomain{NetBIOS: d.netbios, Realm: d.realm, Base: d.base, SID: d.domain()}
}

// Admin is the Administrator's DN and password.
func (d *DomainController) Admin() identity.JoinCredential {
	return identity.JoinCredential{User: "CN=Administrator,CN=Users," + d.base, Password: []byte(adminPasswordDC)}
}

// AddUser adds an enabled user with password, a member of Domain Users.
func (d *DomainController) AddUser(_ testing.TB, name string, password []byte) sid.SID {
	d.mu.Lock()
	defer d.mu.Unlock()
	u := &dcUser{dn: "CN=" + name + ",CN=Users," + d.base, rid: d.nextRID, password: slices.Clone(password)}
	d.nextRID++
	d.users[strings.ToLower(name)] = u
	return d.sidOf(u.rid)
}

// BadPwdCount is the user's badPwdCount at the controller last dialled.
func (d *DomainController) BadPwdCount(_ testing.TB, name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dialled.badPwd[strings.ToLower(name)]
}

// StageComputer creates a computer account at the controller last dialled,
// as an administrator creates one by hand, in CN=Computers.
func (d *DomainController) StageComputer(_ testing.TB, c StagedComputer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.putAccount(d.dialled, c.Name, "CN="+c.Name+",CN=Computers,"+d.base, c.Password, c.EncTypes, slices.Clone(c.SPNs))
}

// SetComputerPassword sets an account's password at the controller last
// dialled, as its creator may.
func (d *DomainController) SetComputerPassword(t testing.TB, name string, password []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.setPassword(d.dialled, name, password); err != nil {
		t.Fatalf("set %s$'s password: %v", name, err)
	}
}

// Unreachable is an opened controller that has gone from the domain, which
// every call fails as one that could not be asked.
func (d *DomainController) Unreachable(testing.TB) netlogon.DC {
	return &memDC{d: d, c: &controller{addr: "gone:135", accounts: map[string]*dcAccount{}, badPwd: map[string]int{}}}
}

// MoveComputersContainer creates the organisational unit ou and makes it the
// domain's Computers container, as redircmp does.
func (d *DomainController) MoveComputersContainer(_ testing.TB, ou string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ous[strings.ToLower(ou)] = true
	d.computers = ou
}

// Reset deletes every computer account, on every controller, resumes held
// replication, and puts the Computers container back.
func (d *DomainController) Reset(testing.TB) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.dcs {
		c.accounts, c.pending, c.held = map[string]*dcAccount{}, nil, false
	}
	d.computers = "CN=Computers," + d.base
}
