package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

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
	cp    kvstore.ControlPlane
	fault *kvtest.FaultKV
	reads *kv.Counting // what reached the store
	reg   *metrics.Registry
	spans *tracetest.SpanRecorder
	tp    *sdktrace.TracerProvider
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
	r := &resolving{reads: &kv.Counting{KV: db}, reg: metrics.New("test", "test"), spans: tracetest.NewSpanRecorder()}
	r.fault = kvtest.NewFaultKV(r.reads)
	st, err := kvstore.Open(ctx, r.fault)
	if err != nil {
		t.Fatal(err)
	}
	r.cp = st.ControlPlane()
	r.tp = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(r.spans))
	t.Cleanup(func() { _ = r.tp.Shutdown(context.Background()) })
	if r.srv, err = identity.New(identity.Deps{Store: st, Metrics: r.reg, Tracer: r.tp.Tracer("test")}); err != nil {
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
	mfs, err := r.reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != metrics.Namespace+"_identity_resolution_refusals_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func uid(n string) entity.ProtocolIDs {
	return entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: n}}
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
	bob := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: uid("1001")}
	bob.Disabled = true
	r.user(t, bob, 0)
	carol := entity.User{Principal: entity.NewUser(), Name: "carol", IDs: uid("1002")}
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
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: uid("1000")}
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
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: uid("1000")}
	gen := r.user(t, alice, 0)
	renamed := alice
	renamed.Name = "alice-old"
	r.user(t, renamed, gen)
	r.user(t, entity.User{Principal: entity.NewUser(), Name: "alice", IDs: uid("1003")}, 0)

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
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: uid("1000")}
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
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: uid("1000")}
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
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: uid("1000")}
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
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: uid("1000")}
	r.user(t, alice, 0)
	bob := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: uid("1001"), Disabled: true}
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
				v := a.Value.Emit()
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
