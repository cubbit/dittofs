package vfs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity/identitymem"
	"github.com/cubbit/dittofs/internal/vfs/authz"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
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
		if _, _, err := s.Read(ctx, call, vfs.Anonymous(h), 0, buf); c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
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

// A caller holding Finance deny-only is refused at admission, though the
// file's ACL allows everyone and denies only Finance: the chokepoint cannot
// match a deny-only principal, and letting the call through would let the
// deny entry miss.
func TestADenyOnlyPrincipalIsRefusedAtAdmission(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	finance := s.finance(t)
	dora := s.user(t, "dora", "", s.share, entity.AccessReadWrite)
	h := s.file(t, "ledger", s.fs.Admin.Principal, 0o600)
	_, err := s.SetACL(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, entity.ACL{Entries: []entity.ACE{
		{Type: entity.ACEDeny, Mask: entity.ReadData, Who: finance.Principal},
		{Type: entity.ACEAllow, Mask: entity.ReadData | entity.ReadAttributes, Who: entity.Everyone},
	}})
	s.must(t, err)
	buf := make([]byte, 1)
	if _, _, err := s.Read(ctx, withSIDs(dora.Principal, vfs.ProofSecret, nil, nil), vfs.Anonymous(h), 0, buf); err != nil {
		t.Fatalf("dora's read with no deny-only SID: %v", err)
	}
	if _, _, err := s.Read(ctx, withSIDs(dora.Principal, vfs.ProofSecret, nil, []string{financeSID}), vfs.Anonymous(h), 0, buf); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a read by a caller holding Finance deny-only, the file denying Finance: %v, want ErrAccess", err)
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
