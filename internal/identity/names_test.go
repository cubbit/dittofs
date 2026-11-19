package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// naming is a resolving fixture with the default settings applied.
func naming(t *testing.T) *resolving {
	t.Helper()
	r := newResolving(t)
	if err := r.srv.Apply(state(1)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return r
}

// putGroup stores g, with the IDs it names, and returns its generation.
func (r *resolving) putGroup(t *testing.T, g entity.Group) uint64 {
	t.Helper()
	gen, err := r.cp.PutGroup(context.Background(), g, 0, entity.Alloc{})
	if err != nil {
		t.Fatalf("PutGroup %s: %v", g.Name, err)
	}
	return gen
}

func sidSpelling(s string) pid.Spelling {
	return pid.Spelling{Scheme: pid.SchemeSID, SID: *sid.ParseSIDMust(s)}
}

func spellingsEqual(a, b pid.Spelling) bool {
	return a.Scheme == b.Scheme && a.Num == b.Num && a.Text == b.Text && a.SID.Equal(&b.SID)
}

// Each stored spelling of a user and a group is spelled off its record and
// found back through its index row.
func TestSpellFindRoundTrip(t *testing.T) {
	r := naming(t)
	ctx := context.Background()
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: append(fileIDs("1500"),
		entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: "alice.admin@CORP.EXAMPLE"})}
	r.user(t, alice, 0)
	staff := entity.Group{Principal: entity.NewGroup(), Name: "staff", IDs: entity.ProtocolIDs{
		{Scheme: entity.SchemeGID, Value: "2500"}, sidID("S-1-5-21-9-9-9-2500")}}
	r.putGroup(t, staff)
	m := entity.User{Principal: entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()},
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "host/build01.corp.example@CORP.EXAMPLE"}}}
	r.user(t, m, 0)

	for _, c := range []struct {
		p  entity.Principal
		sp pid.Spelling
	}{
		{alice.Principal, pid.Spelling{Scheme: pid.SchemeUID, Num: 1500}},
		{alice.Principal, sidSpelling("S-1-5-21-9-9-9-1500")},
		{alice.Principal, pid.Spelling{Scheme: pid.SchemeKrb, Text: "alice.admin@CORP.EXAMPLE"}},
		{alice.Principal, pid.Spelling{Scheme: pid.SchemeUserName, Text: "alice"}},
		{staff.Principal, pid.Spelling{Scheme: pid.SchemeGID, Num: 2500}},
		{staff.Principal, sidSpelling("S-1-5-21-9-9-9-2500")},
		{staff.Principal, pid.Spelling{Scheme: pid.SchemeGroupName, Text: "staff"}},
		{m.Principal, pid.Spelling{Scheme: pid.SchemeKrb, Text: "host/build01.corp.example@CORP.EXAMPLE"}},
	} {
		got, err := r.srv.Spell(ctx, c.p, c.sp.Scheme)
		if err != nil || !spellingsEqual(got, c.sp) {
			t.Errorf("Spell(%v, %d) = %+v, %v; want %+v", c.p, c.sp.Scheme, got, err, c.sp)
		}
		if p, err := r.srv.Find(ctx, c.sp, pid.FindOpts{}); err != nil || p != c.p {
			t.Errorf("Find(%+v) = %v, %v; want %v", c.sp, p, err, c.p)
		}
	}

	// A spelling of the other kind's scheme names no one.
	for _, c := range []struct {
		p      entity.Principal
		scheme pid.Scheme
	}{
		{alice.Principal, pid.SchemeGID}, {alice.Principal, pid.SchemeGroupName},
		{staff.Principal, pid.SchemeUID}, {staff.Principal, pid.SchemeUserName}, {staff.Principal, pid.SchemeKrb},
		{m.Principal, pid.SchemeUserName}, {m.Principal, pid.SchemeSID},
	} {
		if got, err := r.srv.Spell(ctx, c.p, c.scheme); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
			t.Errorf("Spell(%v, %d) = %+v, %v; want ErrUnknownPrincipal", c.p, c.scheme, got, err)
		}
	}
	// A machine is never found by name: it has none.
	if p, err := r.srv.Find(ctx, pid.Spelling{Scheme: pid.SchemeUserName, Text: ""}, pid.FindOpts{}); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("Find of the empty user name = %v, %v; want ErrUnknownPrincipal", p, err)
	}
}

// qualified is the applied NetBIOS name, a backslash and the name, and maps
// back by name with the domain's ASCII letters folded.
func TestQualified(t *testing.T) {
	r := naming(t)
	ctx := context.Background()
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.user(t, alice, 0)
	finance := entity.Group{Principal: entity.NewGroup(), Name: "finance", IDs: entity.ProtocolIDs{
		{Scheme: entity.SchemeGID, Value: "2500"}, sidID("S-1-5-21-9-9-9-2500")}}
	r.putGroup(t, finance)

	spell := func(p entity.Principal) string {
		t.Helper()
		sp, err := r.srv.Spell(ctx, p, pid.SchemeQualified)
		if err != nil {
			t.Fatalf("Spell(%v, qualified): %v", p, err)
		}
		return sp.Text
	}
	if got := spell(alice.Principal); got != `DITTOFS\alice` {
		t.Errorf("qualified alice under the defaults = %q, want DITTOFS\\alice", got)
	}
	st := state(2)
	st.Settings.NetBIOSName = "NAS01"
	if err := r.srv.Apply(st); err != nil {
		t.Fatalf("Apply NAS01: %v", err)
	}
	if got := spell(alice.Principal); got != `NAS01\alice` {
		t.Errorf("qualified alice after NAS01 applied = %q, want NAS01\\alice", got)
	}

	for _, c := range []struct {
		text string
		want entity.Principal
		err  error
	}{
		{`nas01\ALICE`, alice.Principal, nil},
		{`NAS01\Finance`, finance.Principal, nil},
		{`DITTOFS\alice`, entity.Principal{}, vfserr.ErrUnknownPrincipal},
		{`NAS01\bob`, entity.Principal{}, vfserr.ErrUnknownPrincipal},
		{`alice`, entity.Principal{}, vfserr.ErrInvalid},
	} {
		p, err := r.srv.Find(ctx, pid.Spelling{Scheme: pid.SchemeQualified, Text: c.text}, pid.FindOpts{})
		if p != c.want || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("Find(qualified %q) = %v, %v; want %v, %v", c.text, p, err, c.want, c.err)
		}
	}

	// Only ASCII letters fold: the Kelvin sign U+212A is not k.
	st = state(3)
	st.Settings.NetBIOSName = "K-NAS"
	if err := r.srv.Apply(st); err != nil {
		t.Fatalf("Apply K-NAS: %v", err)
	}
	for text, want := range map[string]entity.Principal{`k-nas\alice`: alice.Principal, "K-NAS\\alice": {}} {
		if p, err := r.srv.Find(ctx, pid.Spelling{Scheme: pid.SchemeQualified, Text: text}, pid.FindOpts{}); p != want {
			t.Errorf("Find(qualified %q) under K-NAS = %v, %v; want %v", text, p, err, want)
		}
	}

	// A group minted without its name has no name spelling.
	unnamed := entity.Group{Principal: entity.NewGroup(), Name: "finance", IDs: entity.ProtocolIDs{
		{Scheme: entity.SchemeGID, Value: "2501"}, sidID("S-1-5-21-9-9-9-2501")}}
	if err := r.st.Minter().Create(ctx, entity.MintBatch{Groups: []entity.MintGroup{{Group: unnamed, Unnamed: true}}}); err != nil {
		t.Fatalf("Create unnamed: %v", err)
	}
	if sp, err := r.srv.Spell(ctx, unnamed.Principal, pid.SchemeQualified); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("Spell(unnamed group, qualified) = %+v, %v; want ErrUnknownPrincipal", sp, err)
	}
}

// The well-known principals map to their SIDs both ways, and root and
// anonymous to their reserved uid and gid.
func TestWellKnownSpellings(t *testing.T) {
	r := naming(t)
	ctx := context.Background()
	for _, c := range []struct {
		p  entity.Principal
		sp pid.Spelling
	}{
		{entity.Everyone, sidSpelling("S-1-1-0")},
		{entity.OwnerRights, sidSpelling("S-1-3-4")},
		{entity.CreatorOwner, sidSpelling("S-1-3-0")},
		{entity.CreatorGroup, sidSpelling("S-1-3-1")},
		{entity.AuthenticatedUsers, sidSpelling("S-1-5-11")},
		{entity.BuiltinUsers, sidSpelling("S-1-5-32-545")},
		{entity.Administrators, sidSpelling("S-1-5-32-544")},
		{entity.Network, sidSpelling("S-1-5-2")},
		{entity.System, sidSpelling("S-1-5-18")},
		{entity.AnonymousPrincipal, sidSpelling("S-1-5-7")},
		{entity.RootPrincipal, sidSpelling("S-1-22-1-0")},
		{entity.RootPrincipal, pid.Spelling{Scheme: pid.SchemeUID, Num: 0}},
		{entity.RootPrincipal, pid.Spelling{Scheme: pid.SchemeGID, Num: 0}},
		{entity.AnonymousPrincipal, pid.Spelling{Scheme: pid.SchemeUID, Num: 65534}},
		{entity.AnonymousPrincipal, pid.Spelling{Scheme: pid.SchemeGID, Num: 65534}},
	} {
		got, err := r.srv.Spell(ctx, c.p, c.sp.Scheme)
		if err != nil || !spellingsEqual(got, c.sp) {
			t.Errorf("Spell(%v, %d) = %+v, %v; want %+v", c.p, c.sp.Scheme, got, err, c.sp)
		}
		if p, err := r.srv.Find(ctx, c.sp, pid.FindOpts{}); err != nil || p != c.p {
			t.Errorf("Find(%+v) = %v, %v; want %v", c.sp, p, err, c.p)
		}
	}
	// OWNER@ and GROUP@ have no SID of their own, and no well-known principal
	// but root and anonymous a uid.
	for _, c := range []struct {
		p      entity.Principal
		scheme pid.Scheme
	}{{entity.Owner, pid.SchemeSID}, {entity.OwnerGroup, pid.SchemeSID}, {entity.Everyone, pid.SchemeUID}, {entity.RootPrincipal, pid.SchemeUserName}} {
		if sp, err := r.srv.Spell(ctx, c.p, c.scheme); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
			t.Errorf("Spell(%v, %d) = %+v, %v; want ErrUnknownPrincipal", c.p, c.scheme, sp, err)
		}
	}
}

// No principal behind a SID, an API-only account and a deleted user are
// each ErrUnknownPrincipal; an unmapped uid, gid or SID is counted by scheme.
func TestFindUnknown(t *testing.T) {
	r := naming(t)
	ctx := context.Background()
	api := entity.User{Principal: entity.NewUser(), Name: "deployer", Role: entity.RoleOperator,
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "deployer@CORP.EXAMPLE"}}}
	r.user(t, api, 0)
	for _, sp := range []pid.Spelling{
		sidSpelling("S-1-5-21-1-2-3-1107"),
		{Scheme: pid.SchemeUserName, Text: "deployer"},
		{Scheme: pid.SchemeKrb, Text: "deployer@CORP.EXAMPLE"},
		{Scheme: pid.SchemeQualified, Text: `DITTOFS\deployer`},
	} {
		if p, err := r.srv.Find(ctx, sp, pid.FindOpts{}); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
			t.Errorf("Find(%+v) = %v, %v; want ErrUnknownPrincipal", sp, p, err)
		}
	}

	bob := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: fileIDs("1600")}
	gen := r.user(t, bob, 0)
	if err := r.cp.DeleteUser(ctx, bob.Principal, gen); err != nil {
		t.Fatalf("DeleteUser bob: %v", err)
	}
	if sp, err := r.srv.Spell(ctx, bob.Principal, pid.SchemeSID); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("Spell(deleted bob, sid) = %+v, %v; want ErrUnknownPrincipal", sp, err)
	}

	if _, err := r.srv.Find(ctx, pid.Spelling{Scheme: pid.SchemeUID, Num: 4242}, pid.FindOpts{}); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Fatalf("Find(uid 4242): %v, want ErrUnknownPrincipal", err)
	}
	for scheme, want := range map[string]float64{"uid": 1, "gid": 0, "sid": 1} {
		if got := r.counter(t, "identity_unmapped_total", "scheme", scheme); got != want {
			t.Errorf("identity_unmapped_total{scheme=%q} = %v, want %v", scheme, got, want)
		}
	}
}

// A store error reaches the caller as itself, never as ErrUnknownPrincipal,
// and is not counted as unmapped.
func TestLookupErrorPassesThrough(t *testing.T) {
	r := naming(t)
	ctx := context.Background()
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.user(t, alice, 0)
	r.fail(t, kvtest.OpGet, "PX*", "U‖*")
	if p, err := r.srv.Find(ctx, pid.Spelling{Scheme: pid.SchemeUID, Num: 1500}, pid.FindOpts{}); err == nil || errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("Find(uid 1500) with the index failing = %v, %v; want the store's error", p, err)
	}
	if sp, err := r.srv.Spell(ctx, alice.Principal, pid.SchemeUID); err == nil || errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("Spell(alice, uid) with the record failing = %+v, %v; want the store's error", sp, err)
	}
	if got := r.counter(t, "identity_unmapped_total", "scheme", "uid"); got != 0 {
		t.Errorf("identity_unmapped_total{scheme=\"uid\"} after a store error = %v, want 0", got)
	}
}

// owner and oidc are not served yet, and a scheme outside the enum never is.
func TestDeferredSchemes(t *testing.T) {
	r := naming(t)
	ctx := context.Background()
	for _, scheme := range []pid.Scheme{pid.SchemeOwner, pid.SchemeOIDC, 0, pid.SchemeQualified + 1} {
		if p, err := r.srv.Find(ctx, pid.Spelling{Scheme: scheme, Text: "alice@corp.example"}, pid.FindOpts{}); !errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("Find in scheme %d = %v, %v; want ErrInvalid", scheme, p, err)
		}
		if sp, err := r.srv.Spell(ctx, entity.Everyone, scheme); !errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("Spell in scheme %d = %+v, %v; want ErrInvalid", scheme, sp, err)
		}
	}
}
