package node_test

import (
	"slices"
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

// startRotation records a pending rotation of the installation's signing
// key, as the rotate route does, and returns it with the key's reference.
func startRotation(t *testing.T, h *nodetest.Harness) (entity.Operation, entity.SecretRef) {
	t.Helper()
	in, err := h.Store().Installation(t.Context())
	if err != nil {
		t.Fatalf("read the installation: %v", err)
	}
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpRotateSigningKey),
		Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: in.ID[:]}, State: entity.OpPending}
	if _, err := h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("create the rotation: %v", err)
	}
	return op, in.SigningKey
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

// held is the signing key's current version, and whether version v of it
// is still stored.
func held(t *testing.T, h *nodetest.Harness, ref entity.SecretRef, v uint64) (current uint64, stored bool) {
	t.Helper()
	cur, err := h.Store().ControlPlane().Secret(t.Context(), ref, 0)
	if err != nil {
		t.Fatalf("read the signing key's current version: %v", err)
	}
	_, err = h.Store().ControlPlane().Secret(t.Context(), ref, v)
	if err != nil && !merrs.IsNotFoundError(err) {
		t.Fatalf("read the signing key's version %d: %v", v, err)
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

// A rotation switches signing to a new version at its first turn and
// refreshes the verifier there, so a token signed before it still verifies
// and the next is signed with the new version. Once an access token's 15 min
// have passed, a turn ends the operation and prunes the old version, and the
// token signed before the switch is refused.
func TestRotationKeepsTheOldVersionUntilItsTokensExpire(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	n, ctx := h.Node(), t.Context()
	tk := n.Tokens()
	user, session := entity.NewUser(), [16]byte(uuid.New())
	before, err := tk.Issue(ctx, user, session, h.Clock().Now())
	if err != nil {
		t.Fatalf("issue a token before the rotation: %v", err)
	}
	op, ref := startRotation(t, h)

	n.RunnerTurn(ctx)
	if got := operation(t, h, op.ID); got.State != entity.OpRunning {
		t.Fatalf("rotation after its first turn: state %v, want running", got.State)
	}
	if cur, stored := held(t, h, ref, 1); cur != 2 || !stored {
		t.Fatalf("after the switch: current version %d, version 1 stored %v; want 2, and 1 still stored", cur, stored)
	}
	var switched []string
	for a, err := range h.Store().ControlPlane().AuditRecords(ctx, entity.AuditFilter{Target: &op.Target}, nil) {
		if err != nil {
			t.Fatalf("list the installation's audit records: %v", err)
		}
		switched = append(switched, string(a.Change))
	}
	if want := `{"signing_key":{"new":2,"old":1}}`; !slices.Contains(switched, want) {
		t.Errorf("the installation's audit records after the switch: changes %q, want one %s", switched, want)
	}
	if _, err := tk.Verify(ctx, before); err != nil {
		t.Fatalf("token signed before the switch, verified after it: %v, want verified", err)
	}
	after, err := tk.Issue(ctx, user, session, h.Clock().Now())
	if err != nil {
		t.Fatalf("issue a token after the switch: %v", err)
	}
	if got := kid(t, after); got != "2" {
		t.Errorf("token issued at the switch, the clock unmoved: kid %q, want \"2\", the verifier refreshed at the switch", got)
	}

	h.Clock().Advance(15 * time.Minute)
	n.RunnerTurn(ctx)
	if got := operation(t, h, op.ID); got.State != entity.OpSucceeded {
		t.Fatalf("rotation 15 min after the switch: state %v, want succeeded", got.State)
	}
	if cur, stored := held(t, h, ref, 1); cur != 2 || stored {
		t.Errorf("after the prune: current version %d, version 1 stored %v; want 2, and 1 gone", cur, stored)
	}
	if _, err := tk.Verify(ctx, before); err == nil {
		t.Errorf("token signed before the switch, verified after the prune: verified, want refused")
	}
}

// The operation's progress, read through the API, names the switch at the
// first turn and the prune at the turn 15 min later.
func TestRotationProgressReadsThroughTheAPI(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	op, _ := startRotation(t, h)
	turned := make(chan struct{}, 64)
	h.Node().OnRunnerTurn(func() {
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

	h.Clock().Advance(15 * time.Minute)
	wait("15 min later")
	getJSON(t, addr, path, &got)
	if got.State != apiv1.OpState(entity.OpSucceeded.String()) || got.Progress.Phase != "pruned" || got.Progress.Done != 2 {
		t.Errorf("GET %s 15 min after the switch: state %s, progress %+v; want succeeded, phase pruned, 2 done", path, got.State, got.Progress)
	}
}

// Every turn between the switch and the 15 min commits nothing; the turn at
// the 15 min commits the operation's end, once.
func TestRotationWaitCommitsNothing(t *testing.T) {
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

	for _, at := range []time.Duration{time.Second, time.Minute, 15*time.Minute - time.Second} {
		h.Clock().Advance(at - h.Clock().Elapsed())
		n.RunnerTurn(ctx)
	}
	if u, c := counting.TakeUpdates(), counting.Take(); u.Updates != 0 || c.Sets+c.Deletes != 0 {
		t.Errorf("turns at 1 s, 1 min and 14 min 59 s after the switch: %d updates, %d keys set and %d deleted; want none", u.Updates, c.Sets, c.Deletes)
	}
	if got := operation(t, h, op.ID); got.State != entity.OpRunning {
		t.Errorf("rotation 14 min 59 s after the switch: state %v, want running", got.State)
	}

	h.Clock().Advance(15*time.Minute - h.Clock().Elapsed())
	n.RunnerTurn(ctx)
	if u := counting.TakeUpdates(); u.Updates != 1 {
		t.Errorf("turn at 15 min after the switch: %d updates, want 1, the operation's end", u.Updates)
	}
}

// A node restarted between the switch and the prune resumes at the prune:
// it seals no further version, and prunes the old one 15 min after the
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
	h.Clock().Advance(15 * time.Minute)
	h.Node().RunnerTurn(t.Context())
	if got := operation(t, h, op.ID); got.State != entity.OpSucceeded {
		t.Fatalf("rotation 15 min after the switch, across a restart: state %v, want succeeded", got.State)
	}
	if cur, stored := held(t, h, ref, 1); cur != 2 || stored {
		t.Errorf("after the prune, across a restart: current version %d, version 1 stored %v; want 2, and 1 gone", cur, stored)
	}
}
