package identity_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// resolving is a server over the in-memory store, read through a fault
// wrapper over a pass-through counter, with its metrics and spans recorded.
type resolving struct {
	srv   *identity.Server
	st    *kvstore.Store
	cp    kvstore.ControlPlane
	fault *kvtest.FaultKV
	reads *kv.Counting // what reached the store
	reg   *metrics.Registry
	spans *tracetest.SpanRecorder
	tp    *sdktrace.TracerProvider
	logs  *bytes.Buffer // what the server logged, one JSON record a line
}

func newResolving(t *testing.T) *resolving {
	t.Helper()
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	r := &resolving{reads: &kv.Counting{KV: db}, reg: metrics.New("test", "test"), spans: tracetest.NewSpanRecorder(), logs: &bytes.Buffer{}}
	r.fault = kvtest.NewFaultKV(r.reads)
	st, err := kvstore.Open(ctx, r.fault)
	if err != nil {
		t.Fatal(err)
	}
	r.st, r.cp = st, st.ControlPlane()
	// AlwaysSample, not the default parent-based sampler, so a span opened
	// under an unsampled parent is recorded and the test can see it.
	r.tp = sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(r.spans))
	t.Cleanup(func() { _ = r.tp.Shutdown(context.Background()) })
	if r.srv, err = identity.New(identity.Deps{Store: st, Clock: sim.NewTestClock(time.Unix(0, 0)), Metrics: r.reg, Tracer: r.tp.Tracer("test"), Logger: slog.New(slog.NewJSONHandler(r.logs, nil))}); err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// fail makes every op matching op on a key matching one of keys fail.
func (r *resolving) fail(t *testing.T, op string, keys ...string) {
	t.Helper()
	var rules []sim.Rule
	for _, k := range keys {
		rules = append(rules, sim.Rule{Op: op, Key: k, Action: sim.FaultAction{Err: errors.New("injected store fault")}})
	}
	s, err := sim.NewSchedule(1, sim.NewTestClock(time.Unix(0, 0)), rules...)
	if err != nil {
		t.Fatal(err)
	}
	r.fault.SetFault(s)
}

// user stores u and returns its generation.
func (r *resolving) user(t *testing.T, u entity.User, expect uint64) uint64 {
	t.Helper()
	gen, err := r.cp.PutUser(context.Background(), u, expect, entity.Alloc{})
	if err != nil {
		t.Fatalf("PutUser %s: %v", u.Name, err)
	}
	return gen
}

// refusals is identity_resolution_refusals_total's value for reason.
func (r *resolving) refusals(t *testing.T, reason string) float64 {
	t.Helper()
	return r.counter(t, "identity_resolution_refusals_total", "reason", reason)
}

// counter is the named counter's value where its label is value.
func (r *resolving) counter(t *testing.T, name, label, value string) float64 {
	t.Helper()
	mfs, err := r.reg.Registry().Gather()
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

// fileIDs is a file user's spellings: uid n, and a SID ending in n, since
// the store refuses a uid without a SID.
func fileIDs(n string) entity.ProtocolIDs {
	return entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: n}, sidID("S-1-5-21-9-9-9-" + n)}
}

// sidID is the sid spelling of s, in the binary form its index row holds.
func sidID(s string) entity.ProtocolID {
	var b bytes.Buffer
	sid.EncodeSID(&b, sid.ParseSIDMust(s))
	return entity.ProtocolID{Scheme: entity.SchemeSID, Value: b.String()}
}

func ticket(p entity.Principal, name string) pid.Identity {
	return pid.Identity{Proof: pid.ProofTicket, Evidence: pid.Evidence{Name: name, Principal: p}}
}

// A forwarded Identity inconsistent with itself is refused ErrInvalid before
// any store read: with every read failing, a check placed after one would
// answer ErrUnavailable instead.
func TestResolveRefusesAnInconsistentIdentity(t *testing.T) {
	r := newResolving(t)
	r.fail(t, "*", "*")
	alice := entity.NewUser()
	share := entity.SharePolicy{Share: entity.NewShareID(), Squash: entity.SquashNone, Anonymous: entity.AnonymousPrincipal}
	for _, c := range []struct {
		name string
		id   pid.Identity
	}{
		{"a principal at Claimed", pid.Identity{Proof: pid.ProofClaimed, Evidence: pid.Evidence{Principal: alice}}},
		{"a principal at None", pid.Identity{Proof: pid.ProofNone, Evidence: pid.Evidence{Principal: alice}}},
		{"a name at None", pid.Identity{Proof: pid.ProofNone, Evidence: pid.Evidence{Name: "alice"}}},
		{"no principal at Secret", pid.Identity{Proof: pid.ProofSecret, Evidence: pid.Evidence{Name: "alice"}}},
		{"no principal at Ticket", pid.Identity{Proof: pid.ProofTicket}},
		{"an unknown proof kind", pid.Identity{Proof: pid.ProofTicket + 1, Evidence: pid.Evidence{Principal: alice}}},
	} {
		got, err := r.srv.Resolve(context.Background(), c.id, share, pid.ReadOnlyNotNarrowed)
		if !errors.Is(err, vfserr.ErrInvalid) || got != nil {
			t.Errorf("Resolve of %s = %+v, %v; want nil, ErrInvalid", c.name, got, err)
		}
	}
}

// A disabled user and a deleted one are refused ErrAccess whatever their
// evidence, each counted as disabled.
func TestResolveRefusesADisabledOrDeletedUser(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	share := entity.SharePolicy{Share: entity.NewShareID(), Squash: entity.SquashNone, Anonymous: entity.AnonymousPrincipal}
	bob := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: fileIDs("1001")}
	bob.Disabled = true
	r.user(t, bob, 0)
	carol := entity.User{Principal: entity.NewUser(), Name: "carol", IDs: fileIDs("1002")}
	gen := r.user(t, carol, 0)
	if err := r.cp.DeleteUser(ctx, carol.Principal, gen); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	for i, u := range []entity.User{bob, carol} {
		for _, p := range []entity.SharePolicy{share, {}} {
			c, err := r.srv.Resolve(ctx, ticket(u.Principal, u.Name), p, pid.ReadOnlyNotNarrowed)
			if !errors.Is(err, vfserr.ErrAccess) || c != nil {
				t.Errorf("Resolve of %s on share %v = %+v, %v; want nil, ErrAccess", u.Name, p.Share, c, err)
			}
		}
		if got, want := r.refusals(t, "disabled"), float64(2*(i+1)); got != want {
			t.Errorf("refusals{reason=disabled} after %s = %v, want %v", u.Name, got, want)
		}
	}
}

// A store error on the user read is ErrUnavailable, never a caller, and
// never anonymous.
func TestResolveAnswersAStoreErrorUnavailable(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	r.fail(t, kvtest.OpGet, "U‖*")
	share := entity.SharePolicy{Share: entity.NewShareID(), Squash: entity.SquashAll, Anonymous: entity.AnonymousPrincipal}
	c, err := r.srv.Resolve(context.Background(), ticket(alice.Principal, alice.Name), share, pid.ReadOnlyNotNarrowed)
	if !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Fatalf("Resolve with the user read failing = %+v, %v; want nil, ErrUnavailable", c, err)
	}
	if got := r.refusals(t, "store_unavailable"); got != 1 {
		t.Errorf("refusals{reason=store_unavailable} = %v, want 1", got)
	}
	if got := r.refusals(t, "disabled"); got != 0 {
		t.Errorf("refusals{reason=disabled} = %v, want 0", got)
	}
}

// A proven identity maps to the principal sign-in bound, never to its name
// again: a user renamed after sign-in, with another user now holding the
// old name, still resolves to the bound principal.
func TestResolveMapsTheBoundPrincipalNotTheName(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	gen := r.user(t, alice, 0)
	renamed := alice
	renamed.Name = "alice-old"
	r.user(t, renamed, gen)
	r.user(t, entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1003")}, 0)

	c, err := r.srv.Resolve(context.Background(), ticket(alice.Principal, "alice"), entity.SharePolicy{}, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Mapped != alice.Principal {
		t.Errorf("Mapped = %v, want the bound principal %v", c.Mapped, alice.Principal)
	}
}

// A call naming no share runs the map step alone: it answers Mapped, reads
// no grant and no group, which fail here, and squashes nothing.
func TestResolveOnNoShareMapsAlone(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	r.fail(t, "*", "G*", "M*") // every grant, membership and group read
	expires := time.Unix(1_900_000_000, 0)
	id := ticket(alice.Principal, alice.Name)
	id.Evidence.Expires = expires
	c, err := r.srv.Resolve(context.Background(), id, entity.SharePolicy{}, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve on no share: %v", err)
	}
	want := pid.Caller{Mapped: alice.Principal, Fresh: expires}
	if c.Mapped != want.Mapped || !c.Principal.IsZero() || c.Groups != nil || c.Access != entity.AccessNone || c.Authenticated || !c.Fresh.Equal(want.Fresh) {
		t.Errorf("Resolve on no share = %+v, want %+v", *c, want)
	}
}

// The fault pattern above must reach the reads it names, or the test above
// would pass on a grant read the wrapper never failed.
func TestResolveOnAShareReadsWhatTheFaultFails(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	r.fail(t, "*", "G*", "M*")
	share := entity.SharePolicy{Share: entity.NewShareID(), Squash: entity.SquashNone, Anonymous: entity.AnonymousPrincipal}
	if _, err := r.srv.Resolve(context.Background(), ticket(alice.Principal, alice.Name), share, pid.ReadOnlyNotNarrowed); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("Resolve on a share with grant reads failing = %v, want ErrUnavailable", err)
	}
}

// Identity keeps no table of callers: each resolution of one identity reads
// the user's record from the store.
func TestResolveReadsTheStoreOnEveryCall(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	r.reads.Take()
	for i := range 2 {
		if _, err := r.srv.Resolve(context.Background(), ticket(alice.Principal, alice.Name), entity.SharePolicy{}, pid.ReadOnlyNotNarrowed); err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
		if got := r.reads.Take().Gets; got == 0 {
			t.Errorf("Resolve %d read no key from the store; want the user's record read", i)
		}
	}
}

// A resolution opens identity.resolve as a child of the caller's span, with
// the proof kind, the outcome and the principal's ID, and no name or SID.
func TestResolveOpensItsSpan(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	bob := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: fileIDs("1001"), Disabled: true}
	r.user(t, bob, 0)
	userSID := sid.ParseSIDMust("S-1-5-21-1-2-3-1104")

	for _, c := range []struct {
		who     entity.User
		outcome string
	}{{alice, "ok"}, {bob, "disabled"}} {
		ctx, parent := r.tp.Tracer("test").Start(context.Background(), "call")
		id := ticket(c.who.Principal, c.who.Name)
		id.Evidence.SIDs = []pid.SID{*userSID}
		_, _ = r.srv.Resolve(ctx, id, entity.SharePolicy{}, pid.ReadOnlyNotNarrowed)
		parent.End()

		var found bool
		for _, s := range r.spans.Ended() {
			if s.Name() != "identity.resolve" || s.Parent().SpanID() != parent.SpanContext().SpanID() {
				continue
			}
			found = true
			attrs := map[string]string{}
			for _, a := range s.Attributes() {
				v := a.Value.String()
				attrs[string(a.Key)] = v
				if strings.Contains(v, c.who.Name) || strings.Contains(v, sid.FormatSID(userSID)) {
					t.Errorf("%s: attribute %s = %q carries a name or a SID", c.who.Name, a.Key, v)
				}
			}
			want := map[string]string{"identity.proof": "3", "identity.outcome": c.outcome, "identity.principal": c.who.Principal.ID.String()}
			for k, v := range want {
				if attrs[k] != v {
					t.Errorf("%s: span attribute %s = %q, want %q", c.who.Name, k, attrs[k], v)
				}
			}
		}
		if !found {
			t.Errorf("%s: no identity.resolve span under the call's span", c.who.Name)
		}
	}
}

// An unsampled request's resolution opens no span: the parent carries a
// trace that the ratio rule did not sample.
func TestResolveOpensNoSpanUnderAnUnsampledParent(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	unsampled := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}})
	ctx := trace.ContextWithSpanContext(context.Background(), unsampled)
	if _, err := r.srv.Resolve(ctx, ticket(alice.Principal, alice.Name), entity.SharePolicy{}, pid.ReadOnlyNotNarrowed); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, s := range r.spans.Ended() {
		if s.Name() == "identity.resolve" {
			t.Errorf("Resolve under an unsampled parent opened span %q, want none", s.Name())
		}
	}
}

// group stores a group named name and returns its principal. A group holds a
// gid and a SID, so the store allocates the gid, and the SID too unless sd
// names it.
func (r *resolving) group(t *testing.T, name, sd string) entity.Principal {
	t.Helper()
	g := entity.Group{Principal: entity.NewGroup(), Name: name}
	alloc := entity.Alloc{GID: true, RID: true, Range: entity.IDRange{Lo: 1_000_000, Hi: 1_999_999_999}}
	if sd != "" {
		g.IDs = entity.ProtocolIDs{sidID(sd)}
		alloc.RID = false
	}
	if _, err := r.cp.PutGroup(context.Background(), g, 0, alloc); err != nil {
		t.Fatalf("PutGroup %s: %v", name, err)
	}
	return g.Principal
}

// member puts m in g.
func (r *resolving) member(t *testing.T, g, m entity.Principal) {
	t.Helper()
	if err := r.cp.AddMember(context.Background(), entity.Membership{Group: g, Member: m}); err != nil {
		t.Fatalf("AddMember %v in %v: %v", m, g, err)
	}
}

// sorted is ps in the order a Caller lists its groups.
func sorted(ps ...entity.Principal) []entity.Principal {
	return slices.SortedFunc(slices.Values(ps), func(a, b entity.Principal) int {
		if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	})
}

func onShare(squash entity.Squash, anonymous entity.Principal) entity.SharePolicy {
	return entity.SharePolicy{Share: entity.NewShareID(), Squash: squash, Anonymous: anonymous}
}

// grantedShare stores a share in a namespace of its own, grants p access
// there, and returns its policy.
func (r *resolving) grantedShare(t *testing.T, squash entity.Squash, anonymous, p entity.Principal, access entity.ShareAccess) entity.SharePolicy {
	t.Helper()
	ctx := context.Background()
	pol := onShare(squash, anonymous)
	ns := entity.Namespace{ID: entity.NewNamespaceID(), Store: [16]byte{15: 1}}
	if _, err := r.cp.PutNamespace(ctx, ns, 0); err != nil {
		t.Fatalf("PutNamespace: %v", err)
	}
	sh := entity.Share{ID: pol.Share, Name: "s", Path: "/s", Root: entity.NewFileID(), Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: ns.ID}}}
	if _, err := r.cp.PutShare(ctx, sh, 0); err != nil {
		t.Fatalf("PutShare: %v", err)
	}
	if _, err := r.cp.SetGrant(ctx, entity.ShareGrant{Share: pol.Share, Principal: p, Access: access}, 0); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	return pol
}

// A proven caller's groups are the closure of its store memberships unioned
// with each group its evidence's SIDs map to and that group's closure, each
// once, sorted. Its own SID, first in the evidence, adds nothing.
func TestResolveCollectsTheClosureAndTheEvidenceGroups(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	accounts, finance := r.group(t, "Accounts", ""), r.group(t, "Finance", "")
	payroll, ops := r.group(t, "Payroll", "S-1-5-21-1-2-3-2001"), r.group(t, "Ops", "")
	r.group(t, "Unrelated", "")
	r.member(t, accounts, alice.Principal)
	r.member(t, finance, accounts)
	r.member(t, ops, payroll)
	r.member(t, finance, payroll) // reached twice, listed once

	id := ticket(alice.Principal, alice.Name)
	id.Evidence.SIDs = []pid.SID{*sid.ParseSIDMust("S-1-5-21-9-9-9-1000"), *sid.ParseSIDMust("S-1-5-21-1-2-3-2001")}
	c, err := r.srv.Resolve(context.Background(), id, onShare(entity.SquashNone, entity.AnonymousPrincipal), pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := sorted(accounts, finance, payroll, ops); !slices.Equal(c.Groups, want) {
		t.Errorf("Groups = %v, want %v", c.Groups, want)
	}
}

// A membership cycle is no error: each group of it is listed once.
func TestResolveWalksACycleOnce(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	accounts, finance := r.group(t, "Accounts", ""), r.group(t, "Finance", "")
	r.member(t, accounts, alice.Principal)
	r.member(t, finance, accounts)
	r.member(t, accounts, finance)
	c, err := r.srv.Resolve(context.Background(), ticket(alice.Principal, alice.Name), onShare(entity.SquashNone, entity.AnonymousPrincipal), pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve over a cycle: %v", err)
	}
	if want := sorted(accounts, finance); !slices.Equal(c.Groups, want) {
		t.Errorf("Groups over a cycle = %v, want %v", c.Groups, want)
	}
}

// The closure is read whole or the call fails: a nested membership read
// failing, that of the group an evidence SID maps to, is ErrUnavailable,
// never a caller with fewer groups. The failed read is Accounts' alone, so
// no read but the closure's meets it.
func TestResolveRefusesAClosureWithAFailedRead(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	accounts, finance := r.group(t, "Accounts", "S-1-5-21-1-2-3-2001"), r.group(t, "Finance", "")
	r.member(t, finance, accounts)
	r.fail(t, kvtest.OpScan, "MR‖"+hex.EncodeToString(accounts.ID[:])+"*")
	id := ticket(alice.Principal, alice.Name)
	id.Evidence.SIDs = []pid.SID{*sid.ParseSIDMust("S-1-5-21-1-2-3-2001")}
	c, err := r.srv.Resolve(context.Background(), id, onShare(entity.SquashNone, entity.AnonymousPrincipal), pid.ReadOnlyNotNarrowed)
	if !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Errorf("Resolve with Accounts' membership read failing = %+v, %v; want nil, ErrUnavailable", c, err)
	}
}

// A caller in more than 1 024 groups is refused ErrAccess, counted as
// group_cap and logged with the count, and never resolved with fewer; one in
// exactly 1 024 resolves, and a deny-only principal does not count.
func TestResolveRefusesACallerOverTheGroupCap(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	for i := range 1024 {
		r.member(t, r.group(t, fmt.Sprintf("g%d", i), ""), alice.Principal)
	}
	denied := r.group(t, "Denied", "S-1-5-21-1-2-3-3000")
	share := onShare(entity.SquashNone, entity.AnonymousPrincipal)
	id := ticket(alice.Principal, alice.Name)
	id.Evidence.DenyOnly = []pid.SID{*sid.ParseSIDMust("S-1-5-21-1-2-3-3000")}
	c, err := r.srv.Resolve(ctx, id, share, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve in 1 024 groups and one deny-only: %v", err)
	}
	if len(c.Groups) != 1024 || !slices.Equal(c.DenyOnly, []entity.Principal{denied}) {
		t.Errorf("Resolve in 1 024 groups: %d groups, deny-only %v; want 1024, [%v]", len(c.Groups), c.DenyOnly, denied)
	}

	r.member(t, r.group(t, "g1024", ""), alice.Principal)
	c, err = r.srv.Resolve(ctx, id, share, pid.ReadOnlyNotNarrowed)
	if !errors.Is(err, vfserr.ErrAccess) || c != nil {
		t.Fatalf("Resolve in 1 025 groups = %+v, %v; want nil, ErrAccess", c, err)
	}
	if got := r.refusals(t, "group_cap"); got != 1 {
		t.Errorf("refusals{reason=group_cap} = %v, want 1", got)
	}
	if !strings.Contains(r.logs.String(), `"groups":1025`) {
		t.Errorf("log after the cap's refusal = %q, want a record holding the count 1025", r.logs.String())
	}
}

// A squashed caller carries nothing of the principal it replaced: its groups
// are the Anonymous principal's closure, it holds no deny-only principal and
// no flag, though its evidence's SIDs map to groups.
func TestResolveSquashedCallerKeepsNothingOfItsOwn(t *testing.T) {
	r := newResolving(t)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: fileIDs("60000")}
	r.user(t, nobody, 0)
	guests := r.group(t, "Guests", "")
	r.member(t, guests, nobody.Principal)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	accounts := r.group(t, "Accounts", "")
	r.member(t, accounts, alice.Principal)
	r.group(t, "Finance", "S-1-5-21-1-2-3-2001")
	r.group(t, "Denied", "S-1-5-21-1-2-3-3000")

	id := ticket(alice.Principal, alice.Name)
	id.Evidence.SIDs = []pid.SID{*sid.ParseSIDMust("S-1-5-21-1-2-3-2001")}
	id.Evidence.DenyOnly = []pid.SID{*sid.ParseSIDMust("S-1-5-21-1-2-3-3000")}
	share := r.grantedShare(t, entity.SquashAll, nobody.Principal, alice.Principal, entity.AccessAdmin)
	c, err := r.srv.Resolve(context.Background(), id, share, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve squashed: %v", err)
	}
	if c.Principal != nobody.Principal || !slices.Equal(c.Groups, []entity.Principal{guests}) || len(c.DenyOnly) != 0 {
		t.Errorf("squashed caller: Principal %v, Groups %v, DenyOnly %v; want %v, [%v], none", c.Principal, c.Groups, c.DenyOnly, nobody.Principal, guests)
	}
	if c.Authenticated || c.Admin || c.Privileged {
		t.Errorf("squashed caller's flags: Authenticated %v, Admin %v, Privileged %v; want all false", c.Authenticated, c.Admin, c.Privileged)
	}
}

// A deleted Anonymous fails closed: a call the policy would squash is
// refused ErrAccess and counted as anonymous_missing.
func TestResolveRefusesASquashToADeletedAnonymous(t *testing.T) {
	ctx := context.Background()
	r := newResolving(t)
	nobody := entity.User{Principal: entity.NewUser(), Name: "nobody", IDs: fileIDs("60000")}
	gen := r.user(t, nobody, 0)
	if err := r.cp.DeleteUser(ctx, nobody.Principal, gen); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	share := r.grantedShare(t, entity.SquashAll, nobody.Principal, alice.Principal, entity.AccessRead)
	for i, id := range []pid.Identity{{Proof: pid.ProofNone}, ticket(alice.Principal, alice.Name)} {
		c, err := r.srv.Resolve(ctx, id, share, pid.ReadOnlyNotNarrowed)
		if !errors.Is(err, vfserr.ErrAccess) || c != nil {
			t.Errorf("Resolve at proof %d squashed to a deleted Anonymous = %+v, %v; want nil, ErrAccess", id.Proof, c, err)
		}
		if got, want := r.refusals(t, "anonymous_missing"), float64(i+1); got != want {
			t.Errorf("refusals{reason=anonymous_missing} after proof %d = %v, want %v", id.Proof, got, want)
		}
	}
}

// Each deny-only SID maps to a principal in DenyOnly, never in Groups, and
// adds none of its groups; one no principal holds stays unmapped, with no
// error; a lookup error mapping one refuses the call.
func TestResolveMapsDenyOnlyApart(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	r.user(t, alice, 0)
	accounts := r.group(t, "Accounts", "")
	r.member(t, accounts, alice.Principal)
	finance, corp := r.group(t, "Finance", "S-1-5-21-1-2-3-2001"), r.group(t, "Corp", "")
	r.member(t, corp, finance)
	share := onShare(entity.SquashNone, entity.AnonymousPrincipal)

	for _, c := range []struct {
		name     string
		denyOnly string
		want     []entity.Principal
	}{
		{"unmapped", "S-1-5-21-1-2-3-4444", nil},
		{"Finance", "S-1-5-21-1-2-3-2001", []entity.Principal{finance}},
	} {
		id := ticket(alice.Principal, alice.Name)
		id.Evidence.DenyOnly = []pid.SID{*sid.ParseSIDMust(c.denyOnly)}
		got, err := r.srv.Resolve(context.Background(), id, share, pid.ReadOnlyNotNarrowed)
		if err != nil {
			t.Fatalf("Resolve with a deny-only %s SID: %v", c.name, err)
		}
		if !slices.Equal(got.DenyOnly, c.want) || !slices.Equal(got.Groups, []entity.Principal{accounts}) {
			t.Errorf("deny-only %s: DenyOnly %v, Groups %v; want %v, [%v]", c.name, got.DenyOnly, got.Groups, c.want, accounts)
		}
	}

	r.fail(t, kvtest.OpGet, "PX*")
	id := ticket(alice.Principal, alice.Name)
	id.Evidence.DenyOnly = []pid.SID{*sid.ParseSIDMust("S-1-5-21-1-2-3-2001")}
	if c, err := r.srv.Resolve(context.Background(), id, share, pid.ReadOnlyNotNarrowed); !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Errorf("Resolve with the sid index failing = %+v, %v; want nil, ErrUnavailable", c, err)
	}
}
