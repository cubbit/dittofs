package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"

	"github.com/cubbit/dittofs/internal/identity"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// owners is a naming fixture under the domain corp.example holding a user
// alice with uid 1500, a group alice with gid 1500, a user and a group minted
// without a name, an API-only user and a deleted user.
type owners struct {
	*resolving
	alice, aliceGroup, unnamedUser, unnamedGroup, deleted entity.Principal
}

func newOwners(t *testing.T) *owners {
	t.Helper()
	r := newResolving(t)
	st := state(1)
	st.Settings.NFS4Domain = "corp.example"
	if err := r.srv.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	o := &owners{resolving: r}
	ctx := context.Background()
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.user(t, alice, 0)
	group := entity.Group{Principal: entity.NewGroup(), Name: "alice", IDs: entity.ProtocolIDs{
		{Scheme: entity.SchemeGID, Value: "1500"}, sidID("S-1-5-21-9-9-8-1500")}}
	r.putGroup(t, group)
	api := entity.User{Principal: entity.NewUser(), Name: "deployer", Role: entity.RoleOperator}
	r.user(t, api, 0)
	// Each named alice, which the user and the group already hold, so each
	// is minted unnamed, as a directory principal is when its name is held.
	unnamedUser := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("3000")}
	unnamedGroup := entity.Group{Principal: entity.NewGroup(), Name: "alice", IDs: entity.ProtocolIDs{
		{Scheme: entity.SchemeGID, Value: "3001"}, sidID("S-1-5-21-9-9-8-3001")}}
	if err := r.st.Minter().Create(ctx, entity.MintBatch{
		Users:  []entity.MintUser{{User: unnamedUser, Unnamed: true}},
		Groups: []entity.MintGroup{{Group: unnamedGroup, Unnamed: true}},
	}); err != nil {
		t.Fatalf("Create unnamed: %v", err)
	}
	bob := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: fileIDs("1600")}
	gen := r.user(t, bob, 0)
	if err := r.cp.DeleteUser(ctx, bob.Principal, gen); err != nil {
		t.Fatalf("DeleteUser bob: %v", err)
	}
	o.alice, o.aliceGroup, o.unnamedUser, o.unnamedGroup, o.deleted = alice.Principal, group.Principal, unnamedUser.Principal, unnamedGroup.Principal, bob.Principal
	return o
}

// The owner scheme spells a user, and owner_group a group, by name in the
// domain, or by number when it has no name; root and anonymous by their
// reserved numbers. Each spelling finds its principal back.
func TestOwnerSpell(t *testing.T) {
	o := newOwners(t)
	ctx := context.Background()
	for _, c := range []struct {
		p      entity.Principal
		scheme pid.Scheme
		want   string // "" is ErrUnknownPrincipal
	}{
		{o.alice, pid.SchemeOwner, "alice@corp.example"},
		{o.aliceGroup, pid.SchemeOwnerGroup, "alice@corp.example"},
		{o.unnamedUser, pid.SchemeOwner, "3000"},
		{o.unnamedGroup, pid.SchemeOwnerGroup, "3001"},
		{entity.RootPrincipal, pid.SchemeOwner, "0"},
		{entity.RootPrincipal, pid.SchemeOwnerGroup, "0"},
		{entity.AnonymousPrincipal, pid.SchemeOwner, "65534"},
		{entity.AnonymousPrincipal, pid.SchemeOwnerGroup, "65534"},
		{o.alice, pid.SchemeOwnerGroup, ""},
		{o.aliceGroup, pid.SchemeOwner, ""},
		{o.deleted, pid.SchemeOwner, ""},
		{entity.Everyone, pid.SchemeOwner, ""},
		{entity.AuthenticatedUsers, pid.SchemeOwnerGroup, ""},
	} {
		sp, err := o.srv.Spell(ctx, c.p, c.scheme)
		if c.want == "" {
			if !errors.Is(err, vfserr.ErrUnknownPrincipal) {
				t.Errorf("Spell(%v, %d) = %+v, %v; want ErrUnknownPrincipal", c.p, c.scheme, sp, err)
			}
			continue
		}
		if err != nil || sp.Scheme != c.scheme || sp.Text != c.want {
			t.Errorf("Spell(%v, %d) = %+v, %v; want %q", c.p, c.scheme, sp, err, c.want)
			continue
		}
		if p, err := o.srv.Find(ctx, sp, pid.FindOpts{}); err != nil || p != c.p {
			t.Errorf("Find(%d %q) = %v, %v; want %v back", c.scheme, sp.Text, p, err, c.p)
		}
	}
}

// Find reads a name only in the applied domain, ASCII letters folded, and
// in the scheme's own kind; a canonical decimal through the uid or gid index;
// anything else, and a miss, is ErrUnknownPrincipal counted under owner, never
// under uid or gid.
func TestOwnerFind(t *testing.T) {
	o := newOwners(t)
	ctx := context.Background()
	misses := 0.0
	for _, c := range []struct {
		scheme pid.Scheme
		text   string
		want   entity.Principal // zero is ErrUnknownPrincipal
	}{
		{pid.SchemeOwner, "alice@corp.example", o.alice},
		{pid.SchemeOwner, "ALICE@CORP.EXAMPLE", o.alice},
		{pid.SchemeOwnerGroup, "alice@corp.example", o.aliceGroup},
		{pid.SchemeOwnerGroup, "Alice@Corp.Example", o.aliceGroup},
		{pid.SchemeOwner, "1500", o.alice},
		{pid.SchemeOwnerGroup, "1500", o.aliceGroup},
		{pid.SchemeOwner, "0", entity.RootPrincipal},
		{pid.SchemeOwnerGroup, "65534", entity.AnonymousPrincipal},
		{pid.SchemeOwner, "alice@other.example", entity.Principal{}},
		{pid.SchemeOwnerGroup, "alice@other.example", entity.Principal{}},
		{pid.SchemeOwner, "alice", entity.Principal{}},
		{pid.SchemeOwner, "alice@corp.example.", entity.Principal{}},
		{pid.SchemeOwner, "1500@corp.example", entity.Principal{}},
		{pid.SchemeOwner, "01500", entity.Principal{}},
		{pid.SchemeOwner, "+1500", entity.Principal{}},
		{pid.SchemeOwner, "4294967296", entity.Principal{}},
		{pid.SchemeOwner, "1600", entity.Principal{}},
		{pid.SchemeOwner, "bob@corp.example", entity.Principal{}},
		{pid.SchemeOwner, "deployer@corp.example", entity.Principal{}},
		{pid.SchemeOwnerGroup, "3000", entity.Principal{}},
		{pid.SchemeOwner, "@corp.example", entity.Principal{}},
		{pid.SchemeOwner, "", entity.Principal{}},
		// The Kelvin sign U+212A is not k: only ASCII letters fold.
		{pid.SchemeOwner, "alice@corp.exampl\u212a", entity.Principal{}},
	} {
		p, err := o.srv.Find(ctx, pid.Spelling{Scheme: c.scheme, Text: c.text}, pid.FindOpts{})
		if c.want == (entity.Principal{}) {
			misses++
			if !errors.Is(err, vfserr.ErrUnknownPrincipal) {
				t.Errorf("Find(%d %q) = %v, %v; want ErrUnknownPrincipal", c.scheme, c.text, p, err)
			}
			continue
		}
		if err != nil || p != c.want {
			t.Errorf("Find(%d %q) = %v, %v; want %v", c.scheme, c.text, p, err, c.want)
		}
	}
	for scheme, want := range map[string]float64{"owner": misses, "uid": 0, "gid": 0} {
		if got := o.counter(t, "identity_unmapped_total", "scheme", scheme); got != want {
			t.Errorf("identity_unmapped_total{scheme=%q} = %v, want %v", scheme, got, want)
		}
	}
}

// A store error on the name or the uid index reaches the caller as itself,
// never as ErrUnknownPrincipal, and is not counted as unmapped.
func TestOwnerLookupErrorPassesThrough(t *testing.T) {
	o := newOwners(t)
	ctx := context.Background()
	o.fail(t, kvtest.OpGet, "NX*", "PX*")
	for _, sp := range []pid.Spelling{
		{Scheme: pid.SchemeOwner, Text: "alice@corp.example"},
		{Scheme: pid.SchemeOwnerGroup, Text: "alice@corp.example"},
		{Scheme: pid.SchemeOwner, Text: "1500"},
	} {
		if p, err := o.srv.Find(ctx, sp, pid.FindOpts{}); err == nil || errors.Is(err, vfserr.ErrUnknownPrincipal) {
			t.Errorf("Find(%d %q) with the index failing = %v, %v; want the store's error", sp.Scheme, sp.Text, p, err)
		}
	}
	if got := o.counter(t, "identity_unmapped_total", "scheme", "owner"); got != 0 {
		t.Errorf("identity_unmapped_total{scheme=\"owner\"} after store errors = %v, want 0", got)
	}
}

// realmKeytab is a one-entry keytab for svc in realm.
func realmKeytab(t *testing.T, realm string) []byte {
	t.Helper()
	kt := keytab.New()
	if err := kt.AddEntry(svc, realm, "pw", time.Unix(1, 0), 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatal(err)
	}
	b, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The domain is identity.nfs4_domain when set, else the first realm of the
// keytab's current version in lower case, else localdomain, each taking
// effect at the Apply that delivers it.
func TestOwnerDomain(t *testing.T) {
	r := newResolving(t)
	ctx := context.Background()
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.user(t, alice, 0)
	for i, c := range []struct {
		set    string
		keytab []string // realms, current version first
		want   string
	}{
		{"", nil, "localdomain"},
		{"", []string{"CORP.EXAMPLE"}, "corp.example"},
		{"", []string{"NEW.EXAMPLE", "CORP.EXAMPLE"}, "new.example"},
		{"Files.Example", []string{"NEW.EXAMPLE"}, "Files.Example"},
		{"", nil, "localdomain"},
	} {
		st := state(uint64(i + 1))
		st.Settings.NFS4Domain = c.set
		for n, realm := range c.keytab {
			st.Keytab = append(st.Keytab, secrets.Version{Number: uint64(len(c.keytab) - n), Value: realmKeytab(t, realm)})
		}
		if err := r.srv.Apply(st); err != nil {
			t.Fatalf("Apply %+v: %v", c, err)
		}
		sp, err := r.srv.Spell(ctx, alice.Principal, pid.SchemeOwner)
		if want := "alice@" + c.want; err != nil || sp.Text != want {
			t.Errorf("set %q, keytab %v: Spell(alice) = %q, %v; want %q", c.set, c.keytab, sp.Text, err, want)
		}
		if p, err := r.srv.Find(ctx, pid.Spelling{Scheme: pid.SchemeOwner, Text: "ALICE@" + strings.ToUpper(c.want)}, pid.FindOpts{}); err != nil || p != alice.Principal {
			t.Errorf("set %q, keytab %v: Find(ALICE@%s) = %v, %v; want alice", c.set, c.keytab, strings.ToUpper(c.want), p, err)
		}
		if c.want != "corp.example" {
			if p, err := r.srv.Find(ctx, pid.Spelling{Scheme: pid.SchemeOwner, Text: "alice@corp.example"}, pid.FindOpts{}); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
				t.Errorf("set %q, keytab %v: Find(alice@corp.example) = %v, %v; want ErrUnknownPrincipal", c.set, c.keytab, p, err)
			}
		}
	}
}

// A set domain holds only ASCII letters, digits, '.' and '-', at most 253
// bytes; Apply refuses any other.
func TestOwnerDomainChecked(t *testing.T) {
	r := newResolving(t)
	for _, c := range []struct {
		domain string
		ok     bool
	}{
		{"", true},
		{"corp.example", true},
		{"Corp-1.Example", true},
		{strings.Repeat("a", 253), true},
		{strings.Repeat("a", 254), false},
		{"corp@example", false},
		{"corp example", false},
		{"corp_example", false},
		{"corp.exampl\u212a", false},
	} {
		st := state(1)
		st.Settings.NFS4Domain = c.domain
		err := r.srv.Apply(st)
		if (err == nil) != c.ok {
			t.Errorf("Apply with nfs4_domain %q: %v, want accepted %v", c.domain, err, c.ok)
		}
		if err == nil {
			continue
		}
		if !strings.Contains(err.Error(), "nfs4_domain") {
			t.Errorf("Apply with nfs4_domain %q refused with %q, want the field named", c.domain, err)
		}
	}
	if err := identity.DefaultSettings().Validate(); err != nil {
		t.Errorf("the default settings: %v, want valid", err)
	}
}
