package identity_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// dirPrincipals is a server over the memory KV store whose State names an
// in-memory directory, reached through a fault wrapper on an injected clock.
type dirPrincipals struct {
	s     *identity.Server
	st    *kvstore.Store
	inst  uuid.UUID
	d     *identitytest.Directory
	f     *identitytest.FaultyDirectory
	fault *switched
	clock *sim.TestClock
	reg   *metrics.Registry
	logon *logonRecorder
}

// dirStore is a memory KV store initialised with a fresh installation ID.
func dirStore(t *testing.T) (*kvstore.Store, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	inst := uuid.New()
	if err := kvstore.Init(ctx, db, inst); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	return st, inst
}

// protocolSecrets is a secrets provider over st holding a protocol-role key
// from a key file under a temporary directory, closed when the test ends.
func protocolSecrets(t *testing.T, st *kvstore.Store) *secrets.Provider {
	t.Helper()
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", "directory test passphrase")
	b, err := keyprovider.GenerateKeyFile("directory test passphrase")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "protocol.key")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	kp, err := keyprovider.NewProvider(context.Background(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kp.Close() })
	p, err := secrets.New(st.ControlPlane(), map[secrets.Role]keyprovider.KeyProvider{secrets.Protocol: kp})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// sealBindPassword seals pw through p as the directory's bind credential,
// under the reference whose first byte is n, and stores it in st.
func sealBindPassword(t *testing.T, st *kvstore.Store, p *secrets.Provider, n byte, pw []byte) entity.SecretRef {
	t.Helper()
	owner := entity.SecretOwner{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "identity.directory"}
	var ref entity.SecretRef
	ref[0] = n
	sec, err := p.Seal(context.Background(), secrets.Protocol, entity.SecretDirectoryCredential, owner, ref, pw, secrets.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ControlPlane().Write(context.Background(), entity.WriteMeta{Audit: &entity.AuditRecord{
		Target: entity.ResourceRef{Kind: entity.ResourceSecret, ID: sec.ID[:]}, Change: []byte("set")}},
		func(tx entity.ControlPlaneTx) error { return tx.PutSecret(sec, sec.Version-1) }); err != nil {
		t.Fatal(err)
	}
	return ref
}

// newDirPrincipals builds the server over st and a directory of schema,
// whose NetBIOS domain is KORP and realm CORP.EXAMPLE.
func newDirPrincipals(t *testing.T, st *kvstore.Store, inst uuid.UUID, schema string) *dirPrincipals {
	t.Helper()
	e := &dirPrincipals{st: st, inst: inst, d: identitytest.NewDirectory("DC=corp,DC=example"), fault: &switched{},
		clock: sim.NewTestClock(time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)), reg: metrics.New("test", "test"), logon: &logonRecorder{}}
	e.f = identitytest.NewFaultyDirectory(e.d.Dial, e.fault)
	name, pw := e.d.Account()
	p := protocolSecrets(t, st)
	ref := sealBindPassword(t, st, p, 1, pw)
	var err error
	if e.s, err = identity.New(identity.Deps{Store: st, Clock: e.clock, Metrics: e.reg, Secrets: p, Directory: e.f.Dial, Logons: e.logon}); err != nil {
		t.Fatal(err)
	}
	rec := directory.Record{Schema: schema, Address: "ldaps://dc.corp.example", Base: "DC=corp,DC=example", BindName: name,
		BindCredential: ref, Realm: "CORP.EXAMPLE", NetBIOSDomain: "KORP"}
	if err := e.s.Apply(identity.State{Settings: identity.DefaultSettings(), Directory: &rec}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.s.Stop(context.Background()) })
	return e
}

func newAD(t *testing.T) *dirPrincipals {
	t.Helper()
	prof.Watch(t, prof.Options{})
	st, inst := dirStore(t)
	return newDirPrincipals(t, st, inst, directory.SchemaAD)
}

// fail sets the rules every later dial, bind and search runs under; none
// lets every call through.
func (e *dirPrincipals) fail(t *testing.T, rules ...sim.Rule) {
	t.Helper()
	if len(rules) == 0 {
		e.fault.cur.Store(nil)
		return
	}
	s, err := sim.NewSchedule(1, e.clock, rules...)
	if err != nil {
		t.Fatal(err)
	}
	e.fault.cur.Store(s)
}

// pgen is the principals generation.
func (e *dirPrincipals) pgen(t *testing.T) uint64 {
	t.Helper()
	c, err := e.st.Nodes().Counters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c.Principals
}

// counter is name{label=value} read from the registry.
func (e *dirPrincipals) counter(t *testing.T, name, label, value string) float64 {
	t.Helper()
	mfs, err := e.reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != metrics.Namespace+"_"+name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// entry is the directory's answer for the user or group name.
func (e *dirPrincipals) entry(t *testing.T, by directory.Key, name string) directory.Answer {
	t.Helper()
	a, ok, err := e.s.DirectoryLookup(context.Background(), by, []byte(name))
	if err != nil || !ok {
		t.Fatalf("directory lookup of %q: found %v, %v", name, ok, err)
	}
	return a
}

// sidOf is a's SID as Find takes it.
func sidOf(t *testing.T, a directory.Answer) pid.Spelling {
	t.Helper()
	v, _, err := sid.DecodeSID(a.SID)
	if err != nil {
		t.Fatalf("decode %q's SID: %v", a.Name, err)
	}
	return pid.Spelling{Scheme: pid.SchemeSID, SID: *v}
}

func (e *dirPrincipals) find(t *testing.T, sp pid.Spelling, mint bool) (entity.Principal, error) {
	t.Helper()
	return e.s.Find(context.Background(), sp, pid.FindOpts{Mint: mint})
}

// mustFind finds sp, minting with mint, and fails the test on an error.
func (e *dirPrincipals) mustFind(t *testing.T, sp pid.Spelling, mint bool) entity.Principal {
	t.Helper()
	p, err := e.find(t, sp, mint)
	if err != nil {
		t.Fatalf("Find(%+v, mint %v): %v", sp, mint, err)
	}
	return p
}

func (e *dirPrincipals) spell(t *testing.T, p entity.Principal, scheme pid.Scheme) (pid.Spelling, error) {
	t.Helper()
	return e.s.Spell(context.Background(), p, scheme)
}

// num is p's uid or gid.
func (e *dirPrincipals) num(t *testing.T, p entity.Principal, scheme pid.Scheme) uint32 {
	t.Helper()
	sp, err := e.spell(t, p, scheme)
	if err != nil {
		t.Fatalf("Spell(%v, %d): %v", p, scheme, err)
	}
	return sp.Num
}

func attr(v string) [][]byte { return [][]byte{[]byte(v)} }

// A SID of the directory's domain is asked of the directory and minted only
// with Mint: without it Find answers ErrUnknownPrincipal, sends no query and
// writes nothing; with it alice and her three groups, two of hers and Domain
// Users, are minted in one commit, and a second Find binds the same
// principal, writing nothing.
func TestFindDirectorySIDMintsOnlyWithMint(t *testing.T) {
	e := newAD(t)
	alice := e.d.AddUser(t, "alice", nil)
	e.d.AddGroup(t, "eng", map[string][][]byte{"member": attr(alice)})
	e.d.AddGroup(t, "ops", map[string][][]byte{"member": attr(alice)})
	a := e.entry(t, directory.ByUserName, "alice")
	sent := len(e.f.Queries())
	before := e.pgen(t)

	if _, err := e.find(t, sidOf(t, a), false); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Fatalf("Find without Mint = %v, want ErrUnknownPrincipal", err)
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN after Find without Mint %d, want %d unchanged", got, before)
	}
	if got := len(e.f.Queries()); got != sent {
		t.Errorf("searches after Find without Mint %d, want %d: it asked the directory", got, sent)
	}

	p := e.mustFind(t, sidOf(t, a), true)
	if p.Kind != entity.PrincipalUser {
		t.Errorf("minted %v, want a user", p)
	}
	if got := e.pgen(t); got != before+1 {
		t.Errorf("PGEN after the mint %d, want %d: one commit", got, before+1)
	}
	for kind, want := range map[string]float64{"user": 1, "group": 3} {
		if got := e.counter(t, "identity_mints_total", "kind", kind); got != want {
			t.Errorf("identity_mints_total{kind=%s} %v, want %v", kind, got, want)
		}
	}
	for _, g := range []string{"eng", "ops", "Domain Users"} {
		gp := e.mustFind(t, sidOf(t, e.entry(t, directory.ByGroupName, g)), false)
		if gp.Kind != entity.PrincipalGroup {
			t.Errorf("group %s found as %v, want a group", g, gp)
		}
	}
	if again := e.mustFind(t, sidOf(t, a), true); again != p {
		t.Errorf("second Find = %v, want the minted %v", again, p)
	}
	if got := e.pgen(t); got != before+1 {
		t.Errorf("PGEN after the second Find %d, want %d", got, before+1)
	}
	sent = len(e.f.Queries())
	other := pid.Spelling{Scheme: pid.SchemeSID, SID: *sid.ParseSIDMust("S-1-5-21-1-2-3-1104")}
	if _, err := e.find(t, other, true); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("Find with Mint of another domain's SID = %v, want ErrUnknownPrincipal", err)
	}
	if got := len(e.f.Queries()); got != sent {
		t.Errorf("searches after another domain's SID %d, want %d: it was asked of the directory", got, sent)
	}
}

// A directory principal's uid and gid are the entry's, but 0 and 65534,
// which root and anonymous hold, are replaced by allocated ones and counted.
func TestDirectoryMintRemapsReservedIDs(t *testing.T) {
	e := newAD(t)
	e.d.AddUser(t, "u2000", map[string][][]byte{"uidNumber": attr("2000")})
	e.d.AddUser(t, "zero", map[string][][]byte{"uidNumber": attr("0")})
	e.d.AddUser(t, "anon", map[string][][]byte{"uidNumber": attr("65534")})
	dave := e.d.AddUser(t, "dave", map[string][][]byte{"uidNumber": attr("3000")})
	e.d.AddGroup(t, "nogroup", map[string][][]byte{"gidNumber": attr("65534"), "member": attr(dave)})

	p := e.mustFind(t, sidOf(t, e.entry(t, directory.ByUserName, "u2000")), true)
	if got := e.num(t, p, pid.SchemeUID); got != 2000 {
		t.Errorf("u2000's uid %d, want the entry's 2000", got)
	}
	for _, kind := range []string{"user", "group"} {
		if got := e.counter(t, "identity_mints_remapped_total", "kind", kind); got != 0 {
			t.Errorf("identity_mints_remapped_total{kind=%s} after the 2000 mint %v, want 0", kind, got)
		}
	}
	for _, name := range []string{"zero", "anon"} {
		p := e.mustFind(t, sidOf(t, e.entry(t, directory.ByUserName, name)), true)
		if got := e.num(t, p, pid.SchemeUID); got == 0 || got == 65534 {
			t.Errorf("%s's uid %d, want an allocated one", name, got)
		}
	}
	e.mustFind(t, sidOf(t, e.entry(t, directory.ByUserName, "dave")), true) // mints nogroup
	g := e.mustFind(t, sidOf(t, e.entry(t, directory.ByGroupName, "nogroup")), false)
	if got := e.num(t, g, pid.SchemeGID); got == 65534 {
		t.Errorf("nogroup's gid %d, want an allocated one", got)
	}
	for kind, want := range map[string]float64{"user": 2, "group": 1} {
		if got := e.counter(t, "identity_mints_remapped_total", "kind", kind); got != want {
			t.Errorf("identity_mints_remapped_total{kind=%s} %v, want %v", kind, got, want)
		}
	}
}

// RFC 18 §10's "SID-less directory" row, its mint: an rfc2307 entry with no
// SID is minted with a SID of the installation's domain, and the krb link's
// SID for it is the zero one, minting nothing.
func TestDirectoryMintSIDless(t *testing.T) {
	prof.Watch(t, prof.Options{})
	st, inst := dirStore(t)
	e := newDirPrincipals(t, st, inst, directory.SchemaRFC2307)
	e.d.AddUser(t, "erin", map[string][][]byte{
		"objectClass": {[]byte("top"), []byte("posixAccount")}, "uid": attr("erin"),
		"entryUUID": attr("6f1c7a52-0d1e-4b55-9a57-1b9f0c1d2e3f"), "objectSid": nil, "uidNumber": attr("4000"),
	})
	before := e.pgen(t)
	link, err := e.s.KerberosLinkSID(context.Background(), "erin@CORP.EXAMPLE")
	if err != nil || link.SubAuthorityCount != 0 {
		t.Errorf("KerberosLinkSID(erin) = %v, %v; want the zero SID", sid.FormatSID(&link), err)
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN after the link SID %d, want %d: it minted", got, before)
	}

	p := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\erin`}, true)
	sp, err := e.spell(t, p, pid.SchemeSID)
	if err != nil {
		t.Fatalf("erin's SID: %v", err)
	}
	want := fmt.Sprintf("S-1-5-21-%d-%d-%d-", binary.LittleEndian.Uint32(inst[0:]), binary.LittleEndian.Uint32(inst[4:]), binary.LittleEndian.Uint32(inst[8:]))
	if got := sid.FormatSID(&sp.SID); !strings.HasPrefix(got, want) || sp.SID.SubAuthorityCount != 5 {
		t.Errorf("erin's SID %s, want one of the installation's domain, %sRID", got, want)
	}
	if got := e.num(t, p, pid.SchemeUID); got != 4000 {
		t.Errorf("erin's uid %d, want the entry's 4000", got)
	}
}

// RFC 18 §10's "name held at mint" row, its mint: a local group finance
// holds the name, so the directory group Finance alice is in is minted
// unnamed, found by SID, and has no qualified spelling.
func TestDirectoryMintNameHeld(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	local := entity.NewGroup()
	if err := e.st.Minter().Create(ctx, entity.MintBatch{Groups: []entity.MintGroup{{
		Group: entity.Group{Principal: local, Name: "finance"},
		Alloc: entity.Alloc{GID: true, RID: true, Range: entity.DefaultIDRange},
	}}}); err != nil {
		t.Fatal(err)
	}
	alice := e.d.AddUser(t, "alice", nil)
	e.d.AddGroup(t, "Finance", map[string][][]byte{"member": attr(alice)})

	e.mustFind(t, sidOf(t, e.entry(t, directory.ByUserName, "alice")), true)
	g := e.mustFind(t, sidOf(t, e.entry(t, directory.ByGroupName, "Finance")), false)
	if g == local {
		t.Fatal("the directory's Finance found as the local finance")
	}
	if _, err := e.spell(t, g, pid.SchemeQualified); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("Spell(Finance, qualified) = %v, want ErrUnknownPrincipal: it was minted unnamed", err)
	}
	if got := e.counter(t, "identity_mints_unnamed_total", "kind", "group"); got != 1 {
		t.Errorf("identity_mints_unnamed_total{kind=group} %v, want 1", got)
	}
}

// The qualified scheme spells a directory principal under the directory's
// NetBIOS domain, and finds it under any ASCII case of it, never under a
// domain that only Unicode folding matches.
func TestDirectoryQualifiedName(t *testing.T) {
	e := newAD(t)
	e.d.AddUser(t, "bob", nil)
	p := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\bob`}, true)
	if again := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `korp\BOB`}, false); again != p {
		t.Errorf(`Find(korp\BOB) = %v, want %v`, again, p)
	}
	sp, err := e.spell(t, p, pid.SchemeQualified)
	if err != nil || sp.Text != `KORP\bob` {
		t.Errorf(`Spell(bob, qualified) = %q, %v; want KORP\bob`, sp.Text, err)
	}
	if _, err := e.find(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: "KORP\\bob"}, true); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("Find with the Kelvin sign for K = %v, want ErrUnknownPrincipal", err)
	}
}

// A minted directory principal holds its dir spelling, so a local password
// for it is refused.
func TestDirectoryPrincipalHasNoLocalPassword(t *testing.T) {
	e := newAD(t)
	e.d.AddUser(t, "alice", nil)
	p := e.mustFind(t, sidOf(t, e.entry(t, directory.ByUserName, "alice")), true)
	u, err := e.st.Principals().User(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pid.NewHashes(u, []byte("a password"), pid.NTLMSetting{On: true}); !errors.Is(err, vfserr.ErrInvalid) {
		t.Errorf("NewHashes for a directory principal = %v, want ErrInvalid", err)
	}
}

// RFC 18 §4.2's step 3 reaches the principal holding the entry's dir
// spelling, never a local user found by the same name, and a name with a
// service part, or a realm in another case, matches nothing.
func TestByKerberosNameReachesTheEntry(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	local := entity.NewUser()
	if err := e.st.Minter().Create(ctx, entity.MintBatch{Users: []entity.MintUser{{
		User:  entity.User{Principal: local, Name: "alice"},
		Alloc: entity.Alloc{UID: true, RID: true, Range: entity.DefaultIDRange},
	}}}); err != nil {
		t.Fatal(err)
	}
	e.d.AddUser(t, "alice", nil)
	e.d.AddUser(t, "cifs/alice", nil) // so only the service-part rule refuses it
	a := e.entry(t, directory.ByUserName, "alice")

	p, err := e.s.ByKerberosName(ctx, "alice", "CORP.EXAMPLE", true)
	if err != nil {
		t.Fatalf("ByKerberosName(alice): %v", err)
	}
	if p == local {
		t.Fatal("alice@CORP.EXAMPLE bound the local alice")
	}
	if sp, err := e.spell(t, p, pid.SchemeDir); err != nil || sp.Text != string(a.EntryID) {
		t.Errorf("the bound principal's dir spelling %q, %v; want alice's entry ID", sp.Text, err)
	}
	for _, tc := range []struct{ user, realm string }{{"cifs/alice", "CORP.EXAMPLE"}, {"alice", "corp.example"}} {
		if _, err := e.s.ByKerberosName(ctx, tc.user, tc.realm, true); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
			t.Errorf("ByKerberosName(%s, %s) = %v, want ErrUnknownPrincipal", tc.user, tc.realm, err)
		}
	}
}

// The krb link's SID is asked fresh only: a cached answer is never used
// while the directory fails, a missing entry, a machine account's name,
// ending in $, and a computer entry under a name with no $ are
// ErrUnknownPrincipal, and a
// realm other than the directory's, in any case, the zero SID. Nothing is
// minted.
func TestKerberosLinkSIDIsFreshOnly(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	e.d.AddUser(t, "alice", nil)
	a := e.entry(t, directory.ByUserName, "alice") // cached
	want := sidOf(t, a).SID
	before := e.pgen(t)

	e.clock.Advance(5*time.Minute + time.Second) // past the TTL, within the grace
	e.fail(t, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
	if _, err := e.s.KerberosLinkSID(ctx, "alice@CORP.EXAMPLE"); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("KerberosLinkSID with the directory failing = %v, want ErrUnavailable", err)
	}
	if _, _, err := e.s.DirectoryLookup(ctx, directory.ByUserName, []byte("alice")); err != nil {
		t.Fatalf("a lookup within the grace = %v, want the stale answer: the test's premise", err)
	}

	e.fail(t)
	e.clock.Advance(10 * time.Second) // the outage's next probe is due
	got, err := e.s.KerberosLinkSID(ctx, "alice@CORP.EXAMPLE")
	if err != nil || !got.Equal(&want) {
		t.Errorf("KerberosLinkSID(alice) = %s, %v; want %s", sid.FormatSID(&got), err, sid.FormatSID(&want))
	}
	if _, err := e.s.KerberosLinkSID(ctx, "nobody@CORP.EXAMPLE"); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("KerberosLinkSID(nobody) = %v, want ErrUnknownPrincipal", err)
	}
	e.d.AddUser(t, "build01$", nil) // a computer account, whose SID no user is linked to
	if got, err := e.s.KerberosLinkSID(ctx, "build01$@CORP.EXAMPLE"); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("KerberosLinkSID(build01$) = %s, %v; want ErrUnknownPrincipal", sid.FormatSID(&got), err)
	}
	e.d.AddUser(t, "build02", map[string][][]byte{"objectClass": {
		[]byte("top"), []byte("person"), []byte("organizationalPerson"), []byte("user"), []byte("computer")}})
	if got, err := e.s.KerberosLinkSID(ctx, "build02@CORP.EXAMPLE"); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("KerberosLinkSID(build02), a computer entry under a name with no $, = %s, %v; want ErrUnknownPrincipal", sid.FormatSID(&got), err)
	}
	for _, name := range []string{"alice@OTHER.REALM", "alice@corp.example"} {
		got, err := e.s.KerberosLinkSID(ctx, name)
		if err != nil || got.SubAuthorityCount != 0 {
			t.Errorf("KerberosLinkSID(%s) = %s, %v; want the zero SID", name, sid.FormatSID(&got), err)
		}
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN %d, want %d: a link SID minted", got, before)
	}
}

// A cold Find with Mint of a user in 300 groups, every search delayed 4
// seconds, fails ErrUnavailable at 20 seconds on the injected clock, having
// written nothing, whether the user is spelled by qualified name or by SID.
func TestFindDirectoryStopsAtTwentySeconds(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, by := range []string{"qualified", "sid"} {
		t.Run(by, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, inst := dirStore(t)
				e := newDirPrincipals(t, st, inst, directory.SchemaAD)
				alice := e.d.AddUser(t, "alice", nil)
				for i := range 300 {
					e.d.AddGroup(t, fmt.Sprintf("g%03d", i), map[string][][]byte{"member": attr(alice)})
				}
				sp := pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\alice`}
				if by == "sid" {
					sp = sidOf(t, e.entry(t, directory.ByUserName, "alice")) // her SID lookup stays cold
				}
				before := e.pgen(t)
				e.fail(t, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Delay: 4 * time.Second}})
				start := e.clock.Now()
				done := make(chan error, 1)
				go func() {
					_, err := e.s.Find(context.Background(), sp, pid.FindOpts{Mint: true})
					done <- err
				}()
				var err error
			wait:
				for {
					synctest.Wait()
					select {
					case err = <-done:
						break wait
					default:
					}
					if e.clock.Now().Sub(start) > time.Minute {
						t.Fatal("Find still running a minute in")
					}
					e.clock.Advance(time.Second)
				}
				if took := e.clock.Now().Sub(start); took != 20*time.Second {
					t.Errorf("Find returned at %v, want 20s", took)
				}
				if !errors.Is(err, vfserr.ErrUnavailable) {
					t.Errorf("Find = %v, want ErrUnavailable", err)
				}
				if got := e.pgen(t); got != before {
					t.Errorf("PGEN %d, want %d: the bounded Find wrote", got, before)
				}
				// The searches the bound left behind finish on the clock.
				for range 60 {
					e.clock.Advance(time.Second)
					synctest.Wait()
				}
			})
		})
	}
}

// A directory that refuses every search, with nothing cached, makes Find of a
// directory name ErrUnavailable with and without Mint, never
// ErrUnknownPrincipal, which would read as "no such account".
func TestFindDirectoryDownIsUnavailable(t *testing.T) {
	e := newAD(t)
	e.d.AddUser(t, "alice", nil)
	e.fail(t, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
	for _, mint := range []bool{false, true} {
		if _, err := e.find(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\alice`}, mint); !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf(`Find(KORP\alice, mint %v) with the directory down = %v, want ErrUnavailable`, mint, err)
		}
	}
}

// A user's mint mints only the groups of the directory's own domain: a
// builtin group and another domain's group in her closure are skipped and
// counted, and a builtin group found by name is not minted either, so no
// record holds a well-known SID or the builtin names.
func TestDirectoryMintSkipsGroupsOfOtherDomains(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	alice := e.d.AddUser(t, "alice", nil)
	e.d.AddGroup(t, "eng", map[string][][]byte{"member": attr(alice)})
	e.d.AddGroup(t, "Users", map[string][][]byte{"member": attr(alice), "objectSid": {sidBytes("S-1-5-32-545")}})
	e.d.AddGroup(t, "Partners", map[string][][]byte{"member": attr(alice), "objectSid": {sidBytes("S-1-5-21-9-9-9-1200")}})

	e.mustFind(t, sidOf(t, e.entry(t, directory.ByUserName, "alice")), true)
	if got := e.counter(t, "identity_mints_total", "kind", "group"); got != 2 {
		t.Errorf("identity_mints_total{kind=group} %v, want 2: eng and Domain Users", got)
	}
	for _, name := range []string{"Users", "Partners"} {
		if p, err := e.st.Principals().ByName(ctx, entity.PrincipalGroup, name); err == nil {
			t.Errorf("group %s minted as %v, want none: its SID is not of the directory's domain", name, p)
		}
	}
	for reason, want := range map[string]float64{"builtin": 1, "other_domain": 1} {
		if got := e.counter(t, "identity_directory_groups_skipped_total", "reason", reason); got != want {
			t.Errorf("identity_directory_groups_skipped_total{reason=%s} %v, want %v", reason, got, want)
		}
	}
	before := e.pgen(t)
	if _, err := e.find(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\Users`}, true); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf(`Find(KORP\Users, mint) = %v, want ErrUnknownPrincipal`, err)
	}
	if got := e.pgen(t); got != before {
		t.Errorf(`PGEN after Find(KORP\Users) %d, want %d: it minted the builtin group`, got, before)
	}
}

// sidBytes is the binary form of the SID s.
func sidBytes(s string) []byte {
	var b bytes.Buffer
	sid.EncodeSID(&b, sid.ParseSIDMust(s))
	return b.Bytes()
}

// A machine account is never minted as a file user (RFC 18 §2.4): not by its
// SID, not by its qualified name, and a Kerberos name ending in $ is refused
// before the directory is asked.
func TestDirectoryMachineAccountIsNotMinted(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	e.d.AddUser(t, "BUILD01$", map[string][][]byte{"objectClass": {
		[]byte("top"), []byte("person"), []byte("organizationalPerson"), []byte("user"), []byte("computer")}})
	a := e.entry(t, directory.ByUserName, "BUILD01$")
	before := e.pgen(t)
	for _, sp := range []pid.Spelling{sidOf(t, a), {Scheme: pid.SchemeQualified, Text: `KORP\BUILD01$`}} {
		if p, err := e.find(t, sp, true); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
			t.Errorf("Find(%+v, mint) of a computer = %v, %v; want ErrUnknownPrincipal", sp, p, err)
		}
	}
	sent := len(e.f.Queries())
	if _, err := e.s.ByKerberosName(ctx, "BUILD02$", "CORP.EXAMPLE", true); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("ByKerberosName(BUILD02$) = %v, want ErrUnknownPrincipal", err)
	}
	if got := len(e.f.Queries()); got != sent {
		t.Errorf("searches after ByKerberosName(BUILD02$) %d, want %d: a machine name was asked of the directory", got, sent)
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN %d, want %d: a machine account was minted", got, before)
	}
}

// A uidNumber or gidNumber another principal holds, in the store or earlier
// in the same batch, is replaced by an allocated one and counted, so one
// clashing group does not refuse its members' mint.
func TestDirectoryMintRemapsHeldIDs(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	if err := e.st.Minter().Create(ctx, entity.MintBatch{
		Users: []entity.MintUser{{User: entity.User{Principal: entity.NewUser(), Name: "local",
			IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "2000"}}}, Alloc: entity.Alloc{RID: true}}},
		Groups: []entity.MintGroup{{Group: entity.Group{Principal: entity.NewGroup(), Name: "localg",
			IDs: entity.ProtocolIDs{{Scheme: entity.SchemeGID, Value: "5000"}}}, Alloc: entity.Alloc{RID: true}}},
	}); err != nil {
		t.Fatal(err)
	}
	alice := e.d.AddUser(t, "alice", map[string][][]byte{"uidNumber": attr("2000")})
	e.d.AddGroup(t, "eng", map[string][][]byte{"gidNumber": attr("5000"), "member": attr(alice)})
	e.d.AddGroup(t, "ops", map[string][][]byte{"gidNumber": attr("6000"), "member": attr(alice)})
	e.d.AddGroup(t, "dev", map[string][][]byte{"gidNumber": attr("6000"), "member": attr(alice)})

	p := e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\alice`}, true)
	if got := e.num(t, p, pid.SchemeUID); got == 2000 {
		t.Errorf("alice's uid %d, want an allocated one: the local user holds 2000", got)
	}
	gid := func(name string) uint32 {
		return e.num(t, e.mustFind(t, sidOf(t, e.entry(t, directory.ByGroupName, name)), false), pid.SchemeGID)
	}
	if got := gid("eng"); got == 5000 {
		t.Errorf("eng's gid %d, want an allocated one: the local group holds 5000", got)
	}
	if ops, dev := gid("ops"), gid("dev"); (ops == 6000) == (dev == 6000) {
		t.Errorf("ops's gid %d and dev's %d, want exactly one of them 6000", ops, dev)
	}
	for kind, want := range map[string]float64{"user": 1, "group": 2} {
		if got := e.counter(t, "identity_mints_remapped_total", "kind", kind); got != want {
			t.Errorf("identity_mints_remapped_total{kind=%s} %v, want %v", kind, got, want)
		}
	}
}

// A mint's closure query and its domain SID read each take one of the
// cache's 64 query slots: with all 64 held, a Find with Mint waits for one
// rather than going around the cap. Every other answer the Find needs is
// fresh in the cache, so only the question under test can wait.
func TestDirectoryMintQueriesTakeASlot(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, tc := range []struct {
		name string
		find func(t *testing.T, e *dirPrincipals) pid.Spelling
	}{
		{"closure", func(t *testing.T, e *dirPrincipals) pid.Spelling {
			// The domain SID, read by an earlier mint, and alice's answer.
			e.d.AddUser(t, "seed", nil)
			e.mustFind(t, pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\seed`}, true)
			e.entry(t, directory.ByUserName, "alice")
			return pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\alice`}
		}},
		{"domain SID", func(t *testing.T, e *dirPrincipals) pid.Spelling {
			// eng's SID answer; a group's mint asks no closure.
			a := e.entry(t, directory.ByGroupName, "eng")
			if _, _, err := e.s.DirectoryLookup(context.Background(), directory.BySID, a.SID); err != nil {
				t.Fatal(err)
			}
			return sidOf(t, a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, inst := dirStore(t)
				e := newDirPrincipals(t, st, inst, directory.SchemaAD)
				ctx := context.Background()
				e.d.AddUser(t, "alice", nil)
				e.d.AddGroup(t, "eng", nil)
				sp := tc.find(t, e)
				e.fail(t, sim.Rule{Op: "search", Key: userSearches, Count: 64, Action: sim.FaultAction{Delay: time.Second}})
				var wg sync.WaitGroup
				for i := range 64 {
					wg.Go(func() { _, _, _ = e.s.DirectoryLookup(ctx, directory.ByUserName, []byte(fmt.Sprintf("u%d", i))) })
				}
				synctest.Wait()
				done := make(chan error, 1)
				go func() {
					_, err := e.s.Find(ctx, sp, pid.FindOpts{Mint: true})
					done <- err
				}()
				synctest.Wait()
				if got := e.s.DirectoryWaiting(); got != 1 {
					t.Errorf("queries waiting for a slot %d, want 1: the mint's", got)
				}
				e.clock.Advance(time.Second)
				wg.Wait()
				if err := <-done; err != nil {
					t.Errorf("Find(%+v, mint) once a slot freed = %v", sp, err)
				}
			})
		})
	}
}
