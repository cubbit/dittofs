package vfs

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	identitysrv "github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// dirCaching is a caching filesystem whose identity server reads an
// in-memory directory, with a directory everyone may create in, on a share
// granting everyone read-write.
type dirCaching struct {
	*caching
	d   *identitytest.Directory
	dir vfs.Handle
}

func newDirCaching(t *testing.T) *dirCaching {
	t.Helper()
	d := identitytest.NewDirectory("DC=corp,DC=example")
	c := newCachingWith(t, func(fx *kvstoretest.FS) (identitysrv.Deps, *directory.Record) {
		name, pw := d.Account()
		p, ref := bindSecret(t, fx, pw)
		return identitysrv.Deps{Secrets: p, Directory: d.Dial}, &directory.Record{Address: "ldaps://dc.corp.example",
			Base: "DC=corp,DC=example", BindName: name, BindCredential: ref, Realm: "CORP.EXAMPLE", NetBIOSDomain: "KORP"}
	})
	ctx := t.Context()
	c.must(t, errOf(c.cp.SetGrant(ctx, entity.ShareGrant{Share: c.share, Principal: entity.Everyone, Access: entity.AccessReadWrite}, 0)))
	dir, err := c.Create(ctx, proven(c.fs.Admin.Principal, inLab), c.fs.Root, []byte("d"), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(0o777)})
	c.must(t, err)
	return &dirCaching{caching: c, d: d, dir: dir.Handle}
}

// bindSecret is a secrets provider over fx's store, holding a protocol-role
// key from a key file under a temporary directory, and the reference of pw
// sealed through it as the directory's bind credential.
func bindSecret(t testing.TB, fx *kvstoretest.FS, pw []byte) (*secrets.Provider, entity.SecretRef) {
	t.Helper()
	ctx := context.Background()
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", "directory test passphrase")
	b, err := keyprovider.GenerateKeyFile("directory test passphrase")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "protocol.key")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	kp, err := keyprovider.NewProvider(ctx, keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kp.Close() })
	cp := fx.Store.ControlPlane()
	p, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Protocol: kp})
	if err != nil {
		t.Fatal(err)
	}
	owner := entity.SecretOwner{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: identitysrv.DirectoryBindCredentialKey}
	var ref entity.SecretRef
	ref[0] = 1
	sec, err := p.Seal(ctx, secrets.Protocol, entity.SecretDirectoryCredential, owner, ref, pw, secrets.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Write(ctx, entity.WriteMeta{Audit: &entity.AuditRecord{
		Target: entity.ResourceRef{Kind: entity.ResourceSecret, ID: sec.ID[:]}, Change: []byte("set")}},
		func(tx entity.ControlPlaneTx) error { return tx.PutSecret(sec, sec.Version-1) }); err != nil {
		t.Fatal(err)
	}
	return p, ref
}

// numbered adds the directory user name holding uidNumber uid, member of
// each group named in groups, and answers its DN.
func (c *dirCaching) numbered(t *testing.T, name string, uid uint32) string {
	t.Helper()
	return c.d.AddUser(t, name, map[string][][]byte{"uidNumber": {[]byte(strconv.FormatUint(uint64(uid), 10))}})
}

// create creates a file named name in c.dir as call.
func (c *dirCaching) create(call vfs.Call, name string) error {
	_, err := c.Create(context.Background(), call, c.dir, []byte(name), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o644)})
	return err
}

// RFC 18 §10's "client in the key": hosts A, B and C, which a share admits
// read-write, read-only and not at all, send identical claims of a
// directory principal's uid, whose caller is cached. A writes, B's write is
// refused ErrReadOnly and C is refused admission, and no call is served a
// caller another host's call built; A's second call is. Catches a cache key
// without the client address.
func TestClaimedClientInTheKey(t *testing.T) {
	c := newDirCaching(t)
	ctx := t.Context()
	c.numbered(t, "alice", 5000)
	a, b, cc := netip.MustParseAddr("10.1.0.1"), netip.MustParseAddr("10.2.0.1"), netip.MustParseAddr("10.3.0.1")
	rules := []entity.ClientRule{
		{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.1.0.0/16")}, Access: entity.AccessReadWrite},
		{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.2.0.0/16")}, Access: entity.AccessRead},
		{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.3.0.0/16")}, Access: entity.AccessNone},
	}
	c.must(t, errOf(c.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: c.share, Clients: rules}, policyVersion(t, c.cp, c.share))))
	hits := testutil.ToFloat64(c.callers.hits)
	for _, row := range []struct {
		name string
		at   netip.Addr
		want error
	}{{"A", a, nil}, {"B", b, vfs.ErrReadOnly}, {"C", cc, vfs.ErrAccess}} {
		if err := c.create(claimed(row.at, 5000, 5000), "f-"+row.name); row.want == nil && err != nil || row.want != nil && !errors.Is(err, row.want) {
			t.Errorf("host %s's create claiming alice's uid: %v, want %v", row.name, err, row.want)
		}
	}
	if got := testutil.ToFloat64(c.callers.hits) - hits; got != 0 {
		t.Errorf("caller cache hits across the three hosts %v, want 0", got)
	}
	if err := c.create(claimed(a, 5000, 5000), "f-A2"); err != nil {
		t.Fatalf("host A's second create: %v", err)
	}
	if got := testutil.ToFloat64(c.callers.hits) - hits; got != 1 {
		t.Errorf("caller cache hits after host A's second call %v, want 1: alice's claimed caller was not cached", got)
	}
}

// A host at its lookup cap never refuses another host's claim: host A, past
// its 100 lookups, sends the claim of a directory user's uid no record
// holds, and host B sends the same claim while A's resolution is held before
// its uid read. B resolves on its own, minting the user, and is admitted,
// never served A's refusal. Catches a claimed caller's key without its
// address, through which B would join A's resolution and share its refusal.
func TestClaimedLookupCapRefusalIsPerAddress(t *testing.T) {
	c := newDirCaching(t)
	c.numbered(t, "bob", 5000)
	a, b := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	for i := range 100 {
		if err := c.create(claimed(a, uint32(10000+i), 0), "u"+strconv.Itoa(i)); err != nil {
			t.Fatalf("host A's unknown uid %d: %v", i, err)
		}
	}
	hold := sim.NewTestClock(time.Unix(0, 0))
	s, err := sim.NewSchedule(1, hold, sim.Rule{Op: kvtest.OpGet, Key: "PX‖01*", Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	c.must(t, err)
	c.fault.SetFault(s)
	t.Cleanup(func() { hold.Advance(time.Hour) })
	aDone := make(chan error, 1)
	go func() { aDone <- c.create(claimed(a, 5000, 0), "fa") }()
	until(t, "host A's resolution held on its uid read", func() bool { return hold.Pending() > 0 })
	var joined atomic.Bool
	c.callers.waiting = func() { joined.Store(true); hold.Advance(time.Hour) }
	if err := c.create(claimed(b, 5000, 0), "fb"); err != nil {
		t.Errorf("host B's claim while host A's is held: %v, want admitted", err)
	}
	if joined.Load() {
		t.Error("host B's call joined host A's resolution, want one of its own")
	}
	// A's uid read, released after B minted bob, finds him, so A needs no
	// lookup and is admitted too: only a call that joined A's resolution
	// could have been refused at A's cap.
	hold.Advance(time.Hour)
	if err := <-aDone; err != nil {
		t.Errorf("host A's claim after host B minted bob: %v, want admitted", err)
	}
}

// RFC 18 §10's "one uid, two proofs": alice, a directory user in Finance,
// reads a file whose ACL grants Finance alone as herself, proven; her uid
// claimed from another host, with a gid no principal holds, is refused,
// unauthenticated, its call a caller-cache miss. Catches a claimed caller
// served the proven one's caller, or given the directory's groups.
func TestClaimedOneUIDTwoProofs(t *testing.T) {
	c := newDirCaching(t)
	ctx := t.Context()
	aliceDN := c.numbered(t, "alice", 5000)
	c.d.AddGroup(t, "Finance", map[string][][]byte{"member": {[]byte(aliceDN)}})
	finance, err := c.srv.Find(ctx, identity.Spelling{Scheme: identity.SchemeQualified, Text: `KORP\Finance`}, identity.FindOpts{Mint: true})
	c.must(t, err)
	alice, err := c.srv.Find(ctx, identity.Spelling{Scheme: identity.SchemeQualified, Text: `KORP\alice`}, identity.FindOpts{Mint: true})
	c.must(t, err)
	admin := proven(c.fs.Admin.Principal, inLab)
	f, err := c.Create(ctx, admin, c.dir, []byte("ledger"), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o600)})
	c.must(t, err)
	_, err = c.SetACL(ctx, admin, vfs.Target{H: f.Handle}, entity.ACL{Entries: []entity.ACE{{Type: entity.ACEAllow, Mask: entity.ReadData | entity.ReadAttributes, Who: finance}}})
	c.must(t, err)
	read := func(call vfs.Call) error {
		_, _, err := c.Read(ctx, call, vfs.Anonymous(f.Handle), 0, make([]byte, 1))
		return err
	}
	if err := read(c.ticket(alice, inLab, 0)); err != nil {
		t.Errorf("alice's read signed in, through Finance: %v, want granted", err)
	}
	misses := testutil.ToFloat64(c.callers.misses)
	if err := read(claimed(netip.MustParseAddr("192.0.2.9"), 5000, 4999)); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("alice's uid claimed from another host: %v, want ErrAccess", err)
	}
	if got := testutil.ToFloat64(c.callers.misses) - misses; got != 1 {
		t.Errorf("caller cache misses for the claimed read %v, want 1", got)
	}
}

// A claimed caller of a directory principal is served from the cache until
// its Fresh, the directory's TTL after it was built, and rebuilt after.
// Catches a caller that used a directory answer served past its bound.
func TestClaimedDirectoryCallerFreshBound(t *testing.T) {
	c := newDirCaching(t)
	c.numbered(t, "bob", 5000)
	call := claimed(netip.MustParseAddr("192.0.2.1"), 5000, 0)
	c.twice(t, "bob's claim", call, true)
	c.clock.Advance(5*time.Minute - time.Second)
	c.twice(t, "bob's claim within its Fresh", call, false)
	c.clock.Advance(time.Second)
	c.twice(t, "bob's claim at its Fresh", call, true)
}
