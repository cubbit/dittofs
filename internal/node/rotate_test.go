package node_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// pruneAfter is how long after the switch the rotation prunes: an access
// token's 15 min and the two seconds' margin for a node re-reading late.
const pruneAfter = 15*time.Minute + 2*time.Second

// startRotation records a pending rotation of the installation's signing
// key, started by an account, as the rotate route does, and returns it with
// the key's reference.
func startRotation(t *testing.T, h *nodetest.Harness) (entity.Operation, entity.SecretRef) {
	t.Helper()
	in, err := h.Store().Installation(t.Context())
	if err != nil {
		t.Fatalf("read the installation: %v", err)
	}
	return rotation(t, h, entity.ResourceRef{Kind: entity.ResourceInstallation, ID: in.ID[:]}), in.SigningKey
}

// rotation records a pending rotation of target, started by an account.
func rotation(t *testing.T, h *nodetest.Harness, target entity.ResourceRef) entity.Operation {
	t.Helper()
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpRotateSigningKey),
		Target: target, Account: entity.NewUser(), State: entity.OpPending}
	if _, err := h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("create the rotation: %v", err)
	}
	return op
}

// operation reads op back from the store.
func operation(t *testing.T, h *nodetest.Harness, id entity.OperationID) entity.Operation {
	t.Helper()
	op, found, err := h.Store().ControlPlane().Operation(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("read operation %v: found %v, %v", id, found, err)
	}
	return op
}

// held is a secret's current version, the signing key's or a password's,
// and whether version v of it is still stored.
func held(t *testing.T, h *nodetest.Harness, ref entity.SecretRef, v uint64) (current uint64, stored bool) {
	t.Helper()
	cur, err := h.Store().ControlPlane().Secret(t.Context(), ref, 0)
	if err != nil {
		t.Fatalf("read secret %x's current version: %v", ref[:], err)
	}
	_, err = h.Store().ControlPlane().Secret(t.Context(), ref, v)
	if err != nil && !merrs.IsNotFoundError(err) {
		t.Fatalf("read secret %x's version %d: %v", ref[:], v, err)
	}
	return cur.Version, err == nil
}

// kid is the version an access token's header names.
func kid(t *testing.T, raw string) string {
	t.Helper()
	tok, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse the token %q: %v", raw, err)
	}
	k, _ := tok.Header["kid"].(string)
	return k
}

// A rotation switches signing to a new version at its first turn, audited
// as the operation's account on the node, and refreshes the verifier there,
// so a token signed before it still verifies and the next is signed with the
// new version. Once an access token's 15 min and the margin have passed, a
// turn ends the operation and prunes the old version: a token it signed is
// refused, one issued an hour ahead and so not expired among them, for its
// version is no longer held.
func TestRotationKeepsTheOldVersionUntilItsTokensExpire(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	n, ctx := h.Node(), t.Context()
	tk := n.Tokens()
	user, session := entity.NewUser(), [16]byte(uuid.New())
	before, err := tk.Issue(ctx, user, session, h.Clock().Now())
	if err != nil {
		t.Fatalf("issue a token before the rotation: %v", err)
	}
	ahead, err := tk.Issue(ctx, user, session, h.Clock().Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("issue a token an hour ahead before the rotation: %v", err)
	}
	op, ref := startRotation(t, h)

	n.RunnerTurn(ctx)
	if got := operation(t, h, op.ID); got.State != entity.OpRunning {
		t.Fatalf("rotation after its first turn: state %v, want running", got.State)
	}
	if cur, stored := held(t, h, ref, 1); cur != 2 || !stored {
		t.Fatalf("after the switch: current version %d, version 1 stored %v; want 2, and 1 still stored", cur, stored)
	}
	var switched []entity.AuditRecord
	for a, err := range h.Store().ControlPlane().AuditRecords(ctx, entity.AuditFilter{Target: &op.Target}, nil) {
		if err != nil {
			t.Fatalf("list the installation's audit records: %v", err)
		}
		if string(a.Change) == `{"signing_key":"set"}` {
			switched = append(switched, a)
		}
	}
	if len(switched) != 1 || switched[0].Actor != op.Account || switched[0].Node != h.NodeID() || string(switched[0].Outcome) != entity.ShareID(op.ID).String() {
		t.Errorf("the installation's audit records of the switch: %+v, want one, its actor the operation's account %v, on node %v, its outcome the operation %v", switched, op.Account, h.NodeID(), op.ID)
	}
	for name, raw := range map[string]string{"signed before the switch": before, "issued an hour ahead": ahead} {
		if _, err := tk.Verify(ctx, raw); err != nil {
			t.Fatalf("token %s, verified after the switch: %v, want verified", name, err)
		}
	}
	after, err := tk.Issue(ctx, user, session, h.Clock().Now())
	if err != nil {
		t.Fatalf("issue a token after the switch: %v", err)
	}
	if got := kid(t, after); got != "2" {
		t.Errorf("token issued at the switch, the clock unmoved: kid %q, want \"2\", the verifier refreshed at the switch", got)
	}

	h.Clock().Advance(pruneAfter)
	n.RunnerTurn(ctx)
	if got := operation(t, h, op.ID); got.State != entity.OpSucceeded {
		t.Fatalf("rotation 15 min 2 s after the switch: state %v, want succeeded", got.State)
	}
	if cur, stored := held(t, h, ref, 1); cur != 2 || stored {
		t.Errorf("after the prune: current version %d, version 1 stored %v; want 2, and 1 gone", cur, stored)
	}
	if _, err := tk.Verify(ctx, before); err == nil {
		t.Errorf("token signed before the switch, verified after the prune: verified, want refused")
	}
	const notHeld = "version 1 of the signing key is not held"
	if _, err := tk.Verify(ctx, ahead); err == nil || !strings.Contains(err.Error(), notHeld) {
		t.Errorf("unexpired token of version 1, verified after the prune: %v, want refused with %q", err, notHeld)
	}
	fresh, err := tk.Issue(ctx, user, session, h.Clock().Now())
	if err != nil {
		t.Fatalf("issue a token after the prune: %v", err)
	}
	if _, err := tk.Verify(ctx, fresh); err != nil {
		t.Errorf("token of version 2 issued after the prune, verified: %v, want verified", err)
	}
}

// The operation's progress, read through the API, names the switch at the
// first turn and the prune at the turn 15 min 2 s later.
func TestRotationProgressReadsThroughTheAPI(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	op, _ := startRotation(t, h)
	turned := make(chan struct{}, 64)
	h.Node().OnRunnerTurn(func(context.Context) {
		select {
		case turned <- struct{}{}:
		default:
		}
	})
	started(t, h)
	addr, _ := h.Node().Addrs()
	path := "/api/v1/operations/" + uuid.UUID(op.ID).String()
	wait := func(what string) {
		t.Helper()
		select {
		case <-turned:
		case <-time.After(10 * time.Second):
			t.Fatalf("no runner turn %s", what)
		}
	}

	wait("at the start")
	var got apiv1.Operation
	getJSON(t, addr, path, &got)
	if got.State != apiv1.OpState(entity.OpRunning.String()) || got.Progress.Phase != "switched" || got.Progress.Done != 1 {
		t.Errorf("GET %s after the first turn: state %s, progress %+v; want running, phase switched, 1 done", path, got.State, got.Progress)
	}

	h.Clock().Advance(pruneAfter)
	wait("15 min 2 s later")
	getJSON(t, addr, path, &got)
	if got.State != apiv1.OpState(entity.OpSucceeded.String()) || got.Progress.Phase != "pruned" || got.Progress.Done != 2 {
		t.Errorf("GET %s 15 min 2 s after the switch: state %s, progress %+v; want succeeded, phase pruned, 2 done", path, got.State, got.Progress)
	}
}

// Every turn between the switch and the 15 min and margin commits nothing;
// the turn at them commits the operation's end, once.
func TestRotationWaitCommitsNothing(t *testing.T) {
	t.Parallel()
	var counting *kv.Counting
	h := nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		counting = &kv.Counting{KV: cfg.KV}
		cfg.KV = counting
		return node.New(cfg)
	}))
	n, ctx := h.Node(), t.Context()
	op, _ := startRotation(t, h)
	n.RunnerTurn(ctx)
	counting.TakeUpdates()
	counting.Take()

	for _, at := range []time.Duration{time.Second, time.Minute, 15 * time.Minute, pruneAfter - time.Second} {
		h.Clock().Advance(at - h.Clock().Elapsed())
		n.RunnerTurn(ctx)
	}
	if u, c := counting.TakeUpdates(), counting.Take(); u.Updates != 0 || c.Sets+c.Deletes != 0 {
		t.Errorf("turns at 1 s, 1 min, 15 min and 15 min 1 s after the switch: %d updates, %d keys set and %d deleted; want none", u.Updates, c.Sets, c.Deletes)
	}
	if got := operation(t, h, op.ID); got.State != entity.OpRunning {
		t.Errorf("rotation 15 min 1 s after the switch: state %v, want running", got.State)
	}

	h.Clock().Advance(pruneAfter - h.Clock().Elapsed())
	n.RunnerTurn(ctx)
	if u := counting.TakeUpdates(); u.Updates != 1 {
		t.Errorf("turn at 15 min 2 s after the switch: %d updates, want 1, the operation's end", u.Updates)
	}
}

// A node restarted between the switch and the prune resumes at the prune:
// it seals no further version, and prunes the old one 15 min 2 s after the
// switch.
func TestRotationResumesAtThePruneAfterARestart(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	op, ref := startRotation(t, h)
	h.Node().RunnerTurn(t.Context())
	h.Restart(t)

	h.Node().RunnerTurn(t.Context())
	if cur, stored := held(t, h, ref, 1); cur != 2 || !stored {
		t.Fatalf("a turn after the restart, before the 15 min: current version %d, version 1 stored %v; want 2, and 1 still stored", cur, stored)
	}
	h.Clock().Advance(pruneAfter)
	h.Node().RunnerTurn(t.Context())
	if got := operation(t, h, op.ID); got.State != entity.OpSucceeded {
		t.Fatalf("rotation 15 min after the switch, across a restart: state %v, want succeeded", got.State)
	}
	if cur, stored := held(t, h, ref, 1); cur != 2 || stored {
		t.Errorf("after the prune, across a restart: current version %d, version 1 stored %v; want 2, and 1 gone", cur, stored)
	}
}

// A rotation whose target is not the installation, by kind or by ID, fails
// at its first turn naming the target, and seals nothing.
func TestRotationRefusesAnotherTarget(t *testing.T) {
	t.Parallel()
	h := nodetest.New(t)
	in, err := h.Store().Installation(t.Context())
	if err != nil {
		t.Fatalf("read the installation: %v", err)
	}
	other := uuid.New()
	for _, tc := range []struct {
		name   string
		target entity.ResourceRef
	}{
		{"another kind", entity.ResourceRef{Kind: entity.ResourceShare, ID: in.ID[:]}},
		{"another installation", entity.ResourceRef{Kind: entity.ResourceInstallation, ID: other[:]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := rotation(t, h, tc.target)
			h.Node().RunnerTurn(t.Context())
			got := operation(t, h, op.ID)
			if want := fmt.Sprintf("%x", tc.target.ID); got.State != entity.OpFailed || !strings.Contains(string(got.Error), want) {
				t.Errorf("rotation of %s: state %v, error %s; want failed, naming %s", tc.name, got.State, got.Error, want)
			}
			if cur, _ := held(t, h, in.SigningKey, 1); cur != 1 {
				t.Errorf("signing key after the rotation of %s: current version %d, want 1", tc.name, cur)
			}
		})
	}
}

// A rotation over an installation that names no signing key fails, naming
// it, rather than retry at every turn.
func TestRotationFailsWithNoSigningKey(t *testing.T) {
	t.Parallel()
	h := nodetest.New(t)
	err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.SetSigningKey(entity.SecretRef{})
	})
	if err != nil {
		t.Fatalf("clear the installation's signing key: %v", err)
	}
	h.Restart(t)
	op, _ := startRotation(t, h)
	h.Node().RunnerTurn(t.Context())
	if got := operation(t, h, op.ID); got.State != entity.OpFailed || !strings.Contains(string(got.Error), "names no signing key") {
		t.Errorf("rotation with no signing key: state %v, error %s; want failed, naming the missing key", got.State, got.Error)
	}
}

// A rotation fails, writing nothing of the key, once the installation names
// another signing key than the one the node was built with: at the switch,
// before it seals, or at the end, before it prunes.
func TestRotationRefusesARepointedKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		switchd bool // repointed after the switch, before the end
	}{
		{"before the switch", false},
		{"between the switch and the end", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := nodetest.New(t)
			op, ref := startRotation(t, h)
			if tc.switchd {
				h.Node().RunnerTurn(t.Context())
			}
			err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				return tx.SetSigningKey(entity.SecretRef(uuid.New()))
			})
			if err != nil {
				t.Fatalf("repoint the installation's signing key: %v", err)
			}
			h.Clock().Advance(pruneAfter)
			h.Node().RunnerTurn(t.Context())
			got := operation(t, h, op.ID)
			if got.State != entity.OpFailed || !strings.Contains(string(got.Error), "not "+fmt.Sprintf("%x", ref)) {
				t.Errorf("rotation with the key repointed %s: state %v, error %s; want failed, naming the key it was built with", tc.name, got.State, got.Error)
			}
			want := uint64(1)
			if tc.switchd {
				want = 2
			}
			if cur, stored := held(t, h, ref, 1); cur != want || !stored {
				t.Errorf("the key repointed %s: current version %d, version 1 stored %v; want %d, and 1 still stored", tc.name, cur, stored, want)
			}
		})
	}
}
