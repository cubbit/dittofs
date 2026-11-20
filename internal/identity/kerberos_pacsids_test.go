package identity_test

import (
	"context"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The PAC's logon domain in these tests, and attributes a PAC sets: a
// group mandatory, enabled by default and enabled; deny-only, which Windows
// writes with the enabled bit clear; and neither enabled nor deny-only.
const (
	pacDomain = "S-1-5-21-7-7-7"
	enabled   = 7
	denyOnly  = 0x10 | 0x1
	disabled  = 0x1
)

// pacSIDs is a Kerberos fixture whose alice, a local file user, is reached
// through a krb row, holding the installation's own domain SID.
type pacSIDs struct {
	*kerberos
	alice entity.User
	own   string // the installation's domain SID
}

func pacSIDsServer(t *testing.T) *pacSIDs {
	t.Helper()
	k := kerberosServer(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: append(fileIDs("1500"), krbID("alice@TEST.REALM"))}
	k.user(t, alice, 0)
	in, err := k.st.Installation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	own, _, err := sid.DecodeSID(kvstore.DomainSID(in.ID))
	if err != nil {
		t.Fatal(err)
	}
	return &pacSIDs{kerberos: k, alice: alice, own: sid.FormatSID(own)}
}

// alicePAC is the PAC options of a ticket for alice whose logon information
// is l, under the logon domain and alice's RID unless l names them.
func (p *pacSIDs) alicePAC(l identitytest.LogonInfo) identitytest.PACOpts {
	l.Name, l.Domain = "alice", "TEST"
	if l.DomainSID == "" {
		l.DomainSID = pacDomain
	}
	if l.UserRID == 0 {
		l.UserRID = 1105
	}
	return identitytest.PACOpts{Logon: []identitytest.LogonInfo{l}}
}

// signIn signs alice in with a PAC whose logon information is l.
func (p *pacSIDs) signIn(l identitytest.LogonInfo) (pid.SignInResult, string, error) {
	return p.srv.SignInOutcome(context.Background(), p.pacStep(p.alicePAC(l)))
}

// signedIn is alice's identity from a ticket whose PAC carries l, its
// evidence's SIDs those a sign-in takes from the PAC once it verifies.
//
// decision: the SIDs are read through PACEvidence, not SignIn, because a
// sign-in refuses every PAC carrying logon information until its user SID
// is bound to the record. Overturned by that binding, when SignIn answers.
func (p *pacSIDs) signedIn(t *testing.T, l identitytest.LogonInfo) pid.Identity {
	t.Helper()
	o := p.alicePAC(l)
	o.Service, o.AuthTime = "cifs/nas.test", p.clock.Now()
	ss, deny, err := p.srv.PACEvidence(context.Background(), p.realm.PAC(o), kerberosKey("cifs/nas.test"), "alice", o.AuthTime)
	if err != nil {
		t.Fatalf("PACEvidence = %v; want alice's SIDs", err)
	}
	return pid.Identity{Proof: pid.ProofTicket, Client: krbClient,
		Evidence: pid.Evidence{Name: "alice@TEST.REALM", Principal: p.alice.Principal, SIDs: ss, DenyOnly: deny}}
}

// dropped is identity_pac_sids_dropped_total{reason}.
func (p *pacSIDs) dropped(t *testing.T, reason string) float64 {
	t.Helper()
	return p.counter(t, "identity_pac_sids_dropped_total", "reason", reason)
}

// access is what id finds on a share granting only grants.
func (p *pacSIDs) access(t *testing.T, id pid.Identity, grants ...entity.ShareGrant) entity.ShareAccess {
	t.Helper()
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody" + entity.NewShareID().String()[:6], IDs: fileIDs("60000")}
	share := p.sharing(t, grants...)
	c, err := p.srv.Resolve(context.Background(), id, entity.SharePolicy{Share: share, Squash: entity.SquashNone, Anonymous: nobody.Principal}, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return c.Access
}

func sidStrings(ss []pid.SID) []string {
	var out []string
	for i := range ss {
		out = append(out, sid.FormatSID(&ss[i]))
	}
	return out
}

// The acceptance row: a PAC carrying alice's SID, Finance, S-1-5-32-544, a
// SID of the installation's domain, one of another domain and a deny-only
// Payroll gives SIDs alice then Finance, DenyOnly Payroll, and three drops,
// one per reason.
func TestKerberosPACSIDs(t *testing.T) {
	p := pacSIDsServer(t)
	id := p.signedIn(t, identitytest.LogonInfo{
		Groups: []identitytest.GroupRID{{RID: 2001, Attributes: enabled}, {RID: 2002, Attributes: denyOnly}},
		ExtraSIDs: []identitytest.SIDAttrs{{SID: "S-1-5-32-544", Attributes: enabled}, {SID: p.own + "-1600", Attributes: enabled},
			{SID: "S-1-5-21-8-8-8-1200", Attributes: enabled}},
	})
	if got, want := sidStrings(id.Evidence.SIDs), []string{pacDomain + "-1105", pacDomain + "-2001"}; !slices.Equal(got, want) {
		t.Errorf("SIDs = %v, want %v", got, want)
	}
	if got, want := sidStrings(id.Evidence.DenyOnly), []string{pacDomain + "-2002"}; !slices.Equal(got, want) {
		t.Errorf("DenyOnly = %v, want %v", got, want)
	}
	for _, reason := range []string{"builtin", "own_domain", "other_domain"} {
		if got := p.dropped(t, reason); got != 1 {
			t.Errorf("identity_pac_sids_dropped_total{%s} = %v, want 1", reason, got)
		}
	}
}

// RFC 18 §10's "PAC" clause on the filter, proven by the grant a SID gives
// or does not: a SID of the installation's own domain naming a local group
// with a share grant gives no grant; a universal group of a second domain is
// dropped and the sign-in succeeds. Catches an unfiltered PAC granting a
// local group.
func TestKerberosPACSIDsOfOtherDomainsGrantNothing(t *testing.T) {
	p := pacSIDsServer(t)
	localSID := p.own + "-1700"
	local := p.group(t, "Local", localSID)
	id := p.signedIn(t, identitytest.LogonInfo{ExtraSIDs: []identitytest.SIDAttrs{
		{SID: localSID, Attributes: enabled}, {SID: "S-1-5-21-8-8-8-1200", Attributes: enabled}}})
	if a := p.access(t, id, grantTo(local, entity.AccessReadWrite)); a != entity.AccessNone {
		t.Errorf("a PAC carrying the own domain's %s: access %v to a share granting it, want none", localSID, a)
	}
	if got := p.dropped(t, "own_domain"); got != 1 {
		t.Errorf("identity_pac_sids_dropped_total{own_domain} = %v, want 1", got)
	}
	if got := p.dropped(t, "other_domain"); got != 1 {
		t.Errorf("identity_pac_sids_dropped_total{other_domain} = %v, want 1", got)
	}
}

// Well-known SIDs: S-1-5-32-544 is builtin and S-1-5-18 well_known, each
// counted once under its own reason, while everyone, authenticated users and
// network are kept; a SID the store holds as a user is dropped as user.
// Catches a well-known SID dropped as other_domain, a reason that depends on
// which rule ran first, and a user's SID taken for a group.
func TestKerberosPACSIDsWellKnownAndUsers(t *testing.T) {
	p := pacSIDsServer(t)
	p.user(t, entity.User{Principal: entity.NewUser(), Name: "bob", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "1600"}, sidID(pacDomain + "-1600")}}, 0)
	id := p.signedIn(t, identitytest.LogonInfo{
		Groups: []identitytest.GroupRID{{RID: 1600, Attributes: enabled}},
		ExtraSIDs: []identitytest.SIDAttrs{{SID: "S-1-5-32-544", Attributes: enabled}, {SID: "S-1-5-18", Attributes: enabled},
			{SID: "S-1-1-0", Attributes: enabled}, {SID: "S-1-5-11", Attributes: enabled}, {SID: "S-1-5-2", Attributes: enabled}}})
	if got, want := sidStrings(id.Evidence.SIDs), []string{pacDomain + "-1105", "S-1-1-0", "S-1-5-11", "S-1-5-2"}; !slices.Equal(got, want) {
		t.Errorf("SIDs = %v, want %v", got, want)
	}
	for reason, want := range map[string]float64{"builtin": 1, "well_known": 1, "user": 1, "other_domain": 0, "own_domain": 0} {
		if got := p.dropped(t, reason); got != want {
			t.Errorf("identity_pac_sids_dropped_total{%s} = %v, want %v", reason, got, want)
		}
	}
}

// A group listed among the extra SIDs and one among the resource groups,
// each with a share grant, are both granted; a group whose enabled bit is
// clear is absent; a deny-only group with that bit clear is in DenyOnly, and
// a share whose only grant names it refuses alice. Catches a decoder reading
// the memberships alone, and a deny dropped as disabled or folded into the
// groups.
func TestKerberosPACSIDsGroupsAndDenies(t *testing.T) {
	p := pacSIDsServer(t)
	extra := p.group(t, "Extra", pacDomain+"-2003")
	resource := p.group(t, "Resource", pacDomain+"-2004")
	off := p.group(t, "Off", pacDomain+"-2005")
	payroll := p.group(t, "Payroll", pacDomain+"-2002")
	id := p.signedIn(t, identitytest.LogonInfo{
		Groups:         []identitytest.GroupRID{{RID: 2005, Attributes: disabled}, {RID: 2002, Attributes: denyOnly}},
		ExtraSIDs:      []identitytest.SIDAttrs{{SID: pacDomain + "-2003", Attributes: enabled}},
		ResourceDomain: pacDomain, ResourceGroups: []identitytest.GroupRID{{RID: 2004, Attributes: enabled}},
	})
	if got, want := sidStrings(id.Evidence.SIDs), []string{pacDomain + "-1105", pacDomain + "-2003", pacDomain + "-2004"}; !slices.Equal(got, want) {
		t.Errorf("SIDs = %v, want %v", got, want)
	}
	if got, want := sidStrings(id.Evidence.DenyOnly), []string{pacDomain + "-2002"}; !slices.Equal(got, want) {
		t.Errorf("DenyOnly = %v, want %v", got, want)
	}
	for _, c := range []struct {
		name  string
		group entity.Principal
		want  entity.ShareAccess
	}{
		{"an extra SID's group", extra, entity.AccessReadWrite},
		{"a resource group", resource, entity.AccessReadWrite},
		{"a disabled group", off, entity.AccessNone},
		{"a deny-only group", payroll, entity.AccessNone},
	} {
		if a := p.access(t, id, grantTo(c.group, entity.AccessReadWrite)); a != c.want {
			t.Errorf("%s: access %v to a share granting it, want %v", c.name, a, c.want)
		}
	}
}

// The PAC's user SID is not filtered, so a PAC whose logon domain is the
// installation's own, naming a local group with a share grant as alice's
// SID, or is under S-1-5-32 or a well-known authority, is refused
// ErrLogonFailure. Catches the user SID carrying a local group's grant.
func TestKerberosPACSIDsRefuseALocalLogonDomain(t *testing.T) {
	p := pacSIDsServer(t)
	p.group(t, "Local", p.own+"-1700")
	for _, l := range []identitytest.LogonInfo{
		{DomainSID: p.own, UserRID: 1700},
		{DomainSID: "S-1-5-32", UserRID: 544},
		{DomainSID: "S-1-5", UserRID: 18},
	} {
		res, oc, err := p.signIn(l)
		refused(t, l.DomainSID+" as the logon domain", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
	}
}

// Resource groups are read only when UserFlags says they are populated: with
// the bit clear, the same resource group is absent from the SIDs. Catches
// resource groups read whatever UserFlags says.
func TestKerberosPACSIDsResourceGroupsNeedTheirFlag(t *testing.T) {
	p := pacSIDsServer(t)
	for _, c := range []struct {
		clear bool
		want  []string
	}{
		{false, []string{pacDomain + "-1105", pacDomain + "-2004"}},
		{true, []string{pacDomain + "-1105"}},
	} {
		id := p.signedIn(t, identitytest.LogonInfo{ResourceDomain: pacDomain, ResourceFlagClear: c.clear,
			ResourceGroups: []identitytest.GroupRID{{RID: 2004, Attributes: enabled}}})
		if got := sidStrings(id.Evidence.SIDs); !slices.Equal(got, c.want) {
			t.Errorf("resource flag clear %v: SIDs = %v, want %v", c.clear, got, c.want)
		}
	}
}

// The sid index failing while a PAC's SIDs are filtered fails the sign-in
// ErrUnavailable, never keeping or dropping the SID unread.
func TestKerberosPACSIDsStoreFault(t *testing.T) {
	p := pacSIDsServer(t)
	p.fail(t, kvtest.OpGet, "PX‖03*") // the sid index's rows
	res, oc, err := p.signIn(identitytest.LogonInfo{Groups: []identitytest.GroupRID{{RID: 2001, Attributes: enabled}}})
	refused(t, "the sid index failing", res, oc, err, vfserr.ErrUnavailable, "unavailable")
}

// A PAC whose SIDs are accepted still binds no principal while nothing checks
// its user SID against the record: alice's sign-in is refused ErrUnavailable,
// uncounted on the throttle. Catches the PAC's SIDs taken as binding alice.
func TestKerberosPACSIDsBindNoRecordYet(t *testing.T) {
	p := pacSIDsServer(t)
	res, oc, err := p.signIn(identitytest.LogonInfo{Groups: []identitytest.GroupRID{{RID: 2001, Attributes: enabled}}})
	refused(t, "alice with a PAC", res, oc, err, vfserr.ErrUnavailable, "unavailable")
}
