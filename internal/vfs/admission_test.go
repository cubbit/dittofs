package vfs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	identitysrv "github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/identitymem"
	"github.com/cubbit/dittofs/internal/vfs/authz"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// financeSID is the SID a directory's Finance group holds, as a PAC carries
// it.
const financeSID = "S-1-5-21-1-2-3-2001"

// finance stores a group holding financeSID's spelling, with a gid.
func (s *shares) finance(t *testing.T) entity.Group {
	t.Helper()
	var b bytes.Buffer
	sid.EncodeSID(&b, sid.ParseSIDMust(financeSID))
	g := entity.Group{Principal: entity.NewGroup(), Name: "finance",
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeGID, Value: "3001"}, {Scheme: entity.SchemeSID, Value: b.String()}}}
	s.must(t, errOf(s.cp.PutGroup(t.Context(), g, 0, entity.Alloc{})))
	return g
}

// withSIDs is a call by p, proven at proof, whose evidence carries sids and,
// marked deny-only, denyOnly.
func withSIDs(p entity.Principal, proof vfs.ProofKind, sids, denyOnly []string) vfs.Call {
	c := proven(p, inLab)
	c.Identity.Proof = proof
	for _, s := range sids {
		c.Identity.Evidence.SIDs = append(c.Identity.Evidence.SIDs, *sid.ParseSIDMust(s))
	}
	for _, s := range denyOnly {
		c.Identity.Evidence.DenyOnly = append(c.Identity.Evidence.DenyOnly, *sid.ParseSIDMust(s))
	}
	return c
}

// A caller whose evidence maps to Finance reads a file whose ACL grants
// Finance alone on a share that does not squash it, and is refused on one
// that squashes it: the squashed caller reaches the chokepoint as the
// anonymous principal, with none of the groups its evidence named.
func TestASquashedCallerKeepsNoGroupOfItsEvidence(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	finance := s.finance(t)
	carol := s.user(t, "carol", "", s.share, entity.AccessReadWrite)
	nobody := s.user(t, "nobody", "", s.share, entity.AccessRead)
	h := s.file(t, "ledger", s.fs.Admin.Principal, 0o600)
	_, err := s.SetACL(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, entity.ACL{Entries: []entity.ACE{
		{Type: entity.ACEAllow, Mask: entity.ReadData | entity.ReadAttributes, Who: finance.Principal},
	}})
	s.must(t, err)
	call := withSIDs(carol.Principal, vfs.ProofTicket, []string{financeSID}, nil)
	buf := make([]byte, 1)
	for _, c := range []struct {
		squash entity.Squash
		want   error
	}{
		{entity.SquashNone, nil},
		{entity.SquashAll, vfs.ErrAccess},
	} {
		s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: c.squash, Anonymous: nobody.Principal}, policyVersion(t, s.cp, s.share))))
		if _, _, err := s.Read(ctx, call, vfs.Anonymous(h), 0, buf); !errors.Is(err, c.want) {
			t.Errorf("a Finance member's read of a file granting Finance alone, squash %v: %v, want %v", c.squash, err, c.want)
		}
	}
}

// A user signed in over NTLM is refused a share no grant names at
// admission, so neither its Root nor its listing reaches it, though the user
// holds a grant on another; once a grant names it, it is admitted: a new
// share is reachable by nobody until an operator grants it. The refusal is
// admission's, so a share auditing every decision records none of it.
func TestANewShareAdmitsNobodyUntilGranted(t *testing.T) {
	rec := &recorder{}
	s := newShares(t, rec, func(vfs.ShareID) authz.Policy { return authz.AuditAll })
	ctx := t.Context()
	fresh := s.addShare(t, "fresh", "/fresh")
	ned := s.user(t, "ned", "", s.share, entity.AccessReadWrite)
	call := withSIDs(ned.Principal, vfs.ProofSecret, nil, nil)
	reached := func() (admitted, rooted, listed error) {
		a, release, admitted := s.admit(ctx, call, fresh, true, false)
		if admitted == nil {
			release()
			if a.caller.Principal != ned.Principal {
				t.Errorf("ned admitted to the share acts as %v", a.caller.Principal)
			}
		}
		_, _, rooted = s.Root(ctx, call, vfs.ShareRef{Path: "/fresh"})
		list, err := s.ListShares(ctx, call)
		s.must(t, err)
		listed = vfs.ErrAccess
		if slices.ContainsFunc(list, func(i vfs.ShareInfo) bool { return i.Name == "fresh" }) {
			listed = nil
		}
		return admitted, rooted, listed
	}
	admitted, rooted, listed := reached()
	for what, err := range map[string]error{"admission": admitted, "Root": rooted, "ListShares": listed} {
		if !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("an NTLM user and a share with no grant, %s: %v, want ErrAccess", what, err)
		}
	}
	rec.mu.Lock()
	audited := rec.got
	rec.mu.Unlock()
	if len(audited) != 0 {
		t.Errorf("a grant of none refused at admission was audited: %+v; want no event, as the decision: at acting says, so update that decision if admission now audits its refusals", audited)
	}
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: fresh, Principal: ned.Principal, Access: entity.AccessRead}, 0)))
	admitted, rooted, listed = reached()
	for what, err := range map[string]error{"admission": admitted, "Root": rooted, "ListShares": listed} {
		if err != nil {
			t.Errorf("an NTLM user and the share once granted, %s: %v", what, err)
		}
	}
}

// A caller holding Finance deny-only is admitted and then held to each
// file's ACL as RFC 19 §2.3 has it: it reads a file allowing everyone, is
// refused one denying Finance though everyone is allowed after, and one
// denying Corp, Finance's parent in the store, the same way, and is refused
// one allowing Finance alone, which a caller holding Finance as an ordinary
// SID reads.
func TestADenyOnlyPrincipalMatchesDenyEntriesOnly(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	finance := s.finance(t)
	var corpSID bytes.Buffer
	sid.EncodeSID(&corpSID, sid.ParseSIDMust("S-1-5-21-1-2-3-2002"))
	corp := entity.Group{Principal: entity.NewGroup(), Name: "corp",
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeGID, Value: "3002"}, {Scheme: entity.SchemeSID, Value: corpSID.String()}}}
	s.must(t, errOf(s.cp.PutGroup(ctx, corp, 0, entity.Alloc{})))
	s.must(t, s.cp.AddMember(ctx, entity.Membership{Group: corp.Principal, Member: finance.Principal}))
	dora := s.user(t, "dora", "", s.share, entity.AccessReadWrite)
	read := entity.ReadData | entity.ReadAttributes
	file := func(name string, es ...entity.ACE) vfs.Handle {
		h := s.file(t, name, s.fs.Admin.Principal, 0o600)
		_, err := s.SetACL(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, entity.ACL{Entries: es})
		s.must(t, err)
		return h
	}
	open := file("open", entity.ACE{Type: entity.ACEAllow, Mask: read, Who: entity.Everyone})
	denied := file("denied",
		entity.ACE{Type: entity.ACEDeny, Mask: entity.ReadData, Who: finance.Principal},
		entity.ACE{Type: entity.ACEAllow, Mask: read, Who: entity.Everyone})
	corpDenied := file("corp-denied",
		entity.ACE{Type: entity.ACEDeny, Mask: entity.ReadData, Who: corp.Principal},
		entity.ACE{Type: entity.ACEAllow, Mask: read, Who: entity.Everyone})
	granted := file("granted", entity.ACE{Type: entity.ACEAllow, Mask: read, Who: finance.Principal})
	denyOnly := withSIDs(dora.Principal, vfs.ProofSecret, nil, []string{financeSID})
	member := withSIDs(dora.Principal, vfs.ProofSecret, []string{financeSID}, nil)
	buf := make([]byte, 1)
	for _, c := range []struct {
		name string
		call vfs.Call
		h    vfs.Handle
		want error
	}{
		{"a file allowing everyone, read holding Finance deny-only", denyOnly, open, nil},
		{"a file denying Finance, read holding Finance deny-only", denyOnly, denied, vfs.ErrAccess},
		{"a file denying Corp, read holding Finance deny-only", denyOnly, corpDenied, vfs.ErrAccess},
		{"a file allowing Finance alone, read holding Finance deny-only", denyOnly, granted, vfs.ErrAccess},
		{"a file allowing Finance alone, read holding Finance", member, granted, nil},
	} {
		if _, _, err := s.Read(ctx, c.call, vfs.Anonymous(c.h), 0, buf); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// New refuses a Deps with no identity resolver, as admission resolves every
// call through it.
func TestNewRefusesADepsWithoutIdentity(t *testing.T) {
	fs := kvstoretest.NewFS(t, memKV)
	clock := sim.NewTestClock(time.Unix(0, 0))
	tab, err := openstate.New(openstate.Deps{Records: fs.Store, Node: entity.NodeID{15: 1}, Clock: clock, Rand: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Meta: fs.Store, Open: tab, Engine: contentOver(clock, fs.Store, fs.Share.ID)}
	if _, err := New(d); err == nil {
		t.Error("New of a Deps without Identity succeeded")
	}
	d.Identity = identitymem.New(t, fs.Store)
	if _, err := New(d); err != nil {
		t.Errorf("New of the same Deps with Identity: %v", err)
	}
}

// squash sets the caching share's export policy to squash, its Anonymous
// left as the store holds it.
func (c *caching) squash(t *testing.T, squash entity.Squash) {
	t.Helper()
	p, err := c.cp.ExportPolicy(t.Context(), c.share)
	c.must(t, err)
	p.Squash = squash
	c.must(t, errOf(c.cp.PutExportPolicy(t.Context(), p, p.Version)))
}

// A Kerberos caller bound to the well-known root acts as the share's
// Anonymous on a root-squashing share, unprivileged, and as root itself,
// privileged, on a share that squashes nothing.
func TestACallerBoundToRootIsSquashedOrPrivilegedByTheShare(t *testing.T) {
	c := newCaching(t)
	// No grant names root itself; it reaches a share as a proven caller.
	c.must(t, errOf(c.cp.SetGrant(t.Context(), entity.ShareGrant{Share: c.share, Principal: entity.AuthenticatedUsers, Access: entity.AccessReadWrite}, 0)))
	call := c.ticket(entity.RootPrincipal, inLab, 0)
	for _, w := range []struct {
		squash     entity.Squash
		principal  entity.Principal
		privileged bool
	}{
		{entity.SquashRoot, entity.AnonymousPrincipal, false},
		{entity.SquashNone, entity.RootPrincipal, true},
	} {
		c.squash(t, w.squash)
		id, err := c.admitCall(call)
		if err != nil {
			t.Fatalf("root's call, squash %v: %v", w.squash, err)
		}
		if id.Principal != w.principal || id.Privileged != w.privileged {
			t.Errorf("root's call, squash %v: acts as %v, privileged %v; want %v, privileged %v", w.squash, id.Principal, id.Privileged, w.principal, w.privileged)
		}
	}
}

// identity.ntlm turned off by a State differing only in it raises the
// serial and refuses an admitted NTLM caller's next call, counted as
// ntlm_off, while a cached Kerberos caller resolves afresh and is served.
func TestNTLMTurnedOffRefusesAnNTLMCallersNextCall(t *testing.T) {
	c := newCaching(t)
	bob := c.user(t, "bob", "", c.share, entity.AccessReadWrite)
	ntlm := withSIDs(bob.Principal, vfs.ProofSecret, nil, nil)
	krb := c.ticket(bob.Principal, inLab, 0)
	if _, err := c.admitCall(ntlm); err != nil {
		t.Fatalf("bob's NTLM call with NTLM on: %v", err)
	}
	c.twice(t, "bob's Kerberos call", krb, true)

	serial := c.srv.Current()
	off := c.ids
	off.NTLM = false
	c.must(t, c.srv.Apply(identitysrv.State{Generation: c.gen, Settings: off}))
	if c.srv.Current() == serial {
		t.Fatalf("Current() = %d after NTLM was turned off, want it raised", serial)
	}
	if _, err := c.admitCall(ntlm); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's NTLM call once NTLM is off: %v, want ErrAccess", err)
	}
	if err := testutil.GatherAndCompare(c.reg.Registry(), strings.NewReader(`
# HELP dittofs_identity_resolution_refusals_total Calls refused at resolution, by reason.
# TYPE dittofs_identity_resolution_refusals_total counter
dittofs_identity_resolution_refusals_total{reason="ntlm_off"} 1
`), "dittofs_identity_resolution_refusals_total"); err != nil {
		t.Errorf("refusals once NTLM is off: %v", err)
	}
	before := c.resolutions()
	if _, err := c.admitCall(krb); err != nil {
		t.Errorf("bob's Kerberos call once NTLM is off: %v, want served", err)
	}
	if got := c.resolutions() - before; got != 1 {
		t.Errorf("bob's Kerberos call once NTLM is off resolved %d times, want 1", got)
	}
}

// A well-known principal has no user record, so resolution reads none for
// it: with every user read failing, a call proving nothing squashed to the
// well-known anonymous and a Kerberos caller bound to root are served.
func TestAWellKnownPrincipalIsResolvedWithNoUserRead(t *testing.T) {
	c := newCaching(t)
	ctx := t.Context()
	c.must(t, errOf(c.cp.SetGrant(ctx, entity.ShareGrant{Share: c.share, Principal: entity.Everyone, Access: entity.AccessRead}, 0)))
	c.squash(t, entity.SquashNone)
	s, err := sim.NewSchedule(1, sim.NewTestClock(time.Unix(0, 0)), sim.Rule{Op: kvtest.OpGet, Key: "U‖*", Action: sim.FaultAction{Err: errors.New("injected user read fault")}})
	c.must(t, err)
	c.fault.SetFault(s)
	anon := vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofNone, Client: inLab}}
	if id, err := c.admitCall(anon); err != nil || id.Principal != entity.AnonymousPrincipal {
		t.Errorf("a call proving nothing, every user read failing: acts as %v, %v; want %v, served", id.Principal, err, entity.AnonymousPrincipal)
	}
	if id, err := c.admitCall(c.ticket(entity.RootPrincipal, inLab, 0)); err != nil || id.Principal != entity.RootPrincipal {
		t.Errorf("root's call, every user read failing: acts as %v, %v; want %v, served", id.Principal, err, entity.RootPrincipal)
	}
}
