package node_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/google/uuid"
)

var (
	// anAdmin is the identity the tests create files as.
	anAdmin = entity.Identity{Principal: entity.NewUser(), Authenticated: true, Admin: true, Privileged: true}
	// aClient is the address the tests' clients call from.
	aClient = netip.MustParseAddr("10.0.0.5")
)

// deleteShare marks sh removed and records its pending deletion in one
// write, as the management API's delete does.
func deleteShare(t *testing.T, h *nodetest.Harness, sh entity.Share) entity.Operation {
	t.Helper()
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpShareDelete),
		Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: sh.ID[:]}, Account: entity.NewUser(), State: entity.OpPending}
	if err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.DeleteShare(sh.ID, entity.SnapshotsDelete, sh.Generation); err != nil {
			return err
		}
		_, err := tx.PutOperation(op, 0)
		return err
	}); err != nil {
		t.Fatalf("mark share %s removed: %v", sh.Name, err)
	}
	return op
}

// createFiles creates n files in sh's root, named f0 onwards.
func createFiles(t *testing.T, h *nodetest.Harness, sh entity.Share, n int) []entity.File {
	t.Helper()
	if _, err := h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: anAdmin.Principal, Name: "files-admin"}, 0, entity.Alloc{}); err != nil {
		t.Fatalf("create the files' owner: %v", err)
	}
	if _, err := h.Store().ControlPlane().SetGrant(t.Context(), entity.ShareGrant{Share: sh.ID, Principal: anAdmin.Principal, Access: entity.AccessAdmin}, 0); err != nil {
		t.Fatalf("grant share %s: %v", sh.Name, err)
	}
	files := make([]entity.File, n)
	for i := range files {
		var err error
		if _, files[i], err = h.Store().Namespace().Create(t.Context(), anAdmin, entity.NewHandle(sh.ID, sh.Root), fmt.Appendf(nil, "f%d", i), kvstore.Regular(0o644)); err != nil {
			t.Fatalf("create a file in share %s: %v", sh.Name, err)
		}
	}
	return files
}

// gatedRunner has the node's operation runner wait after each turn until
// the test takes it with next, so no turn runs that the test did not let
// run; release lets it run freely, and a test defers it so that it runs
// before the node's Stop. Call it before Start.
type gatedRunner struct {
	h        *nodetest.Harness
	turns    chan struct{}
	released chan struct{}
	once     atomic.Bool
}

func gateRunner(h *nodetest.Harness) *gatedRunner {
	g := &gatedRunner{h: h, turns: make(chan struct{}), released: make(chan struct{})}
	h.Node().OnRunnerTurn(func() {
		select {
		case g.turns <- struct{}{}:
		case <-g.released:
		}
	})
	return g
}

// took waits for the turn the runner has ended.
func (g *gatedRunner) took(t *testing.T) {
	t.Helper()
	select {
	case <-g.turns:
	case <-time.After(10 * time.Second):
		t.Fatal("no operation runner turn within 10 s")
	}
}

// next advances the node's clock a second and waits for the turn it runs.
func (g *gatedRunner) next(t *testing.T) {
	t.Helper()
	g.h.Clock().Advance(time.Second)
	g.took(t)
}

func (g *gatedRunner) release() {
	if g.once.CompareAndSwap(false, true) {
		close(g.released)
	}
}

// tagState is the removal state the store records for share id's tag.
func tagState(t *testing.T, h *nodetest.Harness, id entity.ShareID) entity.TagState {
	t.Helper()
	tags, err := h.Store().Lifecycle().Tags(t.Context())
	if err != nil {
		t.Fatalf("read the tags: %v", err)
	}
	return tags[id].State
}

// offloaded runs runner turns until share id's shard is recorded offloaded
// or past it, forgotten by step 4.
func (g *gatedRunner) offloaded(t *testing.T, id entity.ShareID) {
	t.Helper()
	for range 100 {
		if tagState(t, g.h, id) >= entity.TagOffloaded {
			return
		}
		g.next(t)
	}
	t.Fatal("the shard not offloaded after 100 runner turns")
}

// A share with files and an open file, deleted while serving, has its live
// state ended by the deletion's step 3: its binding refuses its calls, its
// open state is gone, it leaves the running set, and its shard is recorded
// offloaded, while the operation goes on running.
func TestAShareDeletedWhileServingEndsItsLiveState(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	ctx := t.Context()
	configure(t, h, storeA)
	sh := createShare(t, h, "a", storeA)
	file := createFiles(t, h, sh, 3)[2]
	g := gateRunner(h)
	defer g.release()
	started(t, h)
	g.took(t)
	if _, err := h.Node().Engine().Read(ctx, sh.ID, file.ID, 0, make([]byte, 1)); err != nil {
		t.Fatalf("a read of the share before its deletion: %v", err)
	}
	tab := h.Node().OpenState()
	c := openstate.Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolNFSv4, Name: []byte("c"), Boot: [8]byte{1}}}
	p, err := tab.Clients().Propose(ctx, c, aClient)
	if err != nil {
		t.Fatalf("propose a client: %v", err)
	}
	client, _, err := tab.Clients().Connect(ctx, c, &p)
	if err != nil {
		t.Fatalf("connect a client: %v", err)
	}
	who := openstate.Caller{Client: client, Principal: anAdmin.Principal, Protocol: entity.ProtocolNFSv4}
	res, err := tab.Opens().Open(ctx, who, openstate.FileKey{Share: sh.ID, Shard: openstate.ShardID(file.Shard), ID: file.ID},
		openstate.OpenRequest{Via: sh.Root, Name: []byte("f2"), Access: entity.ReadData})
	if err != nil {
		t.Fatalf("open a file: %v", err)
	}
	ref := []openstate.StateRef{res.Open.Ref}

	op := deleteShare(t, h, sh)
	g.offloaded(t, sh.ID)
	if _, err := h.Node().Engine().Read(ctx, sh.ID, file.ID, 0, make([]byte, 1)); !errors.Is(err, vfs.ErrStale) {
		t.Errorf("a read of the deleted share once offloaded: got %v, want %v, its binding unbound", err, vfs.ErrStale)
	}
	if errs := tab.Recovery().TestState(ctx, who, ref); !errors.Is(errs[0], openstate.ErrBadState) {
		t.Errorf("the deleted share's open once offloaded: got %v, want %v", errs[0], openstate.ErrBadState)
	}
	if _, ok := h.Node().Bound(sh.ID); ok {
		t.Error("the deleted share once offloaded: in the running set, want out")
	}
	if s := opState(t, h, op.ID); s != entity.OpRunning {
		t.Errorf("the deletion once offloaded: got %v, want running", s)
	}
	if p := h.Node().Progress(ctx, op); p.Phase != "live state" || p.Done != 2 {
		t.Errorf("the deletion's progress once offloaded: got %+v, want phase live state, 2 steps done, read from the removal's record", p)
	}
}

// A node starting over a share whose removal is marked and not past its
// step 1, as a node stopped between the mark and step 3 finds it, joins the
// share in its filesystem step: the shard's removal is recorded offloaded
// before the runner starts and the node is ready, and the share joins no
// running set.
func TestARemovalLeftAtItsMarkIsJoinedAtStart(t *testing.T) {
	// Start calls the stepping hook on the test's goroutine, so it reads
	// what the test wrote before it.
	var (
		h            *nodetest.Harness
		sh           entity.Share
		atFilesystem *entity.TagState
	)
	h = nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(func(_ *node.Node, step string) {
		if step == "filesystem" {
			s := tagState(t, h, sh.ID)
			atFilesystem = &s
		}
	})))
	configure(t, h, storeA)
	sh = createShare(t, h, "a", storeA)
	file := createFiles(t, h, sh, 1)[0]
	deleteShare(t, h, sh)
	if s := tagState(t, h, sh.ID); s != entity.TagServing {
		t.Fatalf("the shard before the start: %v, want serving", s)
	}
	g := gateRunner(h)
	defer g.release()
	started(t, h)
	if atFilesystem == nil {
		t.Fatal("the shard's removal once the filesystem started: not read, the step not reached")
	}
	if *atFilesystem != entity.TagOffloaded {
		t.Fatalf("the shard's removal once the filesystem started: got %v, want %v", *atFilesystem, entity.TagOffloaded)
	}
	if _, ok := h.Node().Bound(sh.ID); ok {
		t.Error("the share being removed after the start: in the running set, want out")
	}
	if _, err := h.Node().Engine().Read(t.Context(), sh.ID, file.ID, 0, make([]byte, 1)); !errors.Is(err, vfs.ErrStale) {
		t.Errorf("a read of the share being removed after the start: got %v, want %v, its binding unbound", err, vfs.ErrStale)
	}
}

// A shard's removal recorded before the removal reaches step 3 is refused
// with ErrRemovalOrder and changes nothing: the deletion then runs to step 3,
// which records it.
func TestAShardRecordedBeforeStep3IsRefused(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	configure(t, h, storeA)
	sh := createShare(t, h, "a", storeA)
	op := deleteShare(t, h, sh)
	err := h.Store().Nodes().RecordShardRemoval(t.Context(), sh.ID, sh.Shard, entity.TagOffloaded)
	if !errors.Is(err, kvstore.ErrRemovalOrder) {
		t.Fatalf("the shard recorded offloaded at the mark: got %v, want %v", err, kvstore.ErrRemovalOrder)
	}
	if s := opState(t, h, op.ID); s != entity.OpPending {
		t.Fatalf("the deletion after the refused record: got %v, want pending", s)
	}
	if s := tagState(t, h, sh.ID); s != entity.TagServing {
		t.Fatalf("the shard after the refused record: got %v, want serving", s)
	}
	g := gateRunner(h)
	defer g.release()
	started(t, h)
	g.took(t)
	g.offloaded(t, sh.ID)
}

// A share's unbind takes it out of the running set and out of the refusals
// at once, with no reconcile pass between: a bound share deleted leaves the
// running set, and a refused one leaves the refusals and the node status.
func TestAnUnbindClearsTheRunningSetAndTheRefusals(t *testing.T) {
	var hold atomic.Bool
	release := make(chan struct{})
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewPausing(func(kvstore.Counters) {
		if hold.Load() {
			<-release
		}
	})))
	defer close(release)
	configure(t, h, storeA)
	bound := createShare(t, h, "bound", storeA)
	refused := createShare(t, h, "refused", storeB)
	statuses := watchStatus(h)
	g := gateRunner(h)
	defer g.release()
	started(t, h)
	g.took(t)
	if _, ok := h.Node().Bound(bound.ID); !ok {
		t.Fatal("the share on a configured store: not in the running set")
	}
	if !refusedShare(h.Node().Refused(), refused.ID) {
		t.Fatal("the share on a store with no settings: not refused")
	}
	turnAt(t, h, statuses, "listing the refused share", func(tr statusTurn) bool { return refusedShare(tr.s.Refused, refused.ID) })
	hold.Store(true)
	deleteShare(t, h, bound)
	deleteShare(t, h, refused)
	for _, sh := range []entity.Share{bound, refused} {
		for tagState(t, h, sh.ID) != entity.TagOffloaded {
			g.next(t)
		}
	}
	if _, ok := h.Node().Bound(bound.ID); ok {
		t.Error("the bound share once unbound: in the running set, want out")
	}
	if refusedShare(h.Node().Refused(), refused.ID) {
		t.Error("the refused share once unbound: among the refusals, want out")
	}
	drain(statuses)
	tr := turnAt(t, h, statuses, "written", func(tr statusTurn) bool { return tr.wrote })
	if refusedShare(tr.s.Refused, refused.ID) || refusedShare(readStatus(t, h).Refused, refused.ID) {
		t.Error("the node status once the refused share is unbound: lists it refused, want not")
	}
}

// A share a pass binds just before its removal unbinds it, the unbind
// landing between the engine's bind and the pass's write of the running set,
// is not put back in the running set by that pass.
func TestAPassDoesNotRunAShareUnboundAfterItsBind(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	configure(t, h, storeA)
	atBound := make(chan entity.ShareID)
	resume := make(chan struct{})
	h.Node().OnBound(func(s entity.Share) {
		atBound <- s.ID
		<-resume
	})
	waiting := make(chan struct{}, 1)
	h.Node().OnReconcileWait(func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	})
	g := gateRunner(h)
	defer g.release()
	started(t, h)
	g.took(t)
	sh := createShare(t, h, "a", storeA)
	// Only the clock wakes reconcile here, so an advance before the loop's
	// first wait after Start is set would be lost.
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile's loop set no wait within 10 s of Start")
	}
	moved := h.Node().Moved()
	h.Clock().Advance(time.Second)
	select {
	case id := <-atBound:
		if id != sh.ID {
			t.Fatalf("the pass bound share %v, want %v", id, sh.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no pass bound the share within 10 s")
	}
	deleteShare(t, h, sh)
	for tagState(t, h, sh.ID) != entity.TagOffloaded {
		g.next(t)
	}
	close(resume)
	select {
	case <-moved:
	case <-time.After(10 * time.Second):
		t.Fatal("the held pass did not end once resumed")
	}
	if _, ok := h.Node().Bound(sh.ID); ok {
		t.Error("the share unbound while the pass that bound it ran: in the running set, want out")
	}
}

// A refused share unbound while a pass that listed it before its mark is
// held is not refused again by that pass once it ends.
func TestAPassDoesNotRefuseAShareUnboundWhileItRan(t *testing.T) {
	h, f := faulted(t)
	refused := createShare(t, h, "refused", storeB)
	turns := make(chan struct{}, 1)
	h.Node().OnRunnerTurn(func() {
		select {
		case turns <- struct{}{}:
		default:
		}
	})
	started(t, h)
	if !refusedShare(h.Node().Refused(), refused.ID) {
		t.Fatal("the share on a store with no settings: not refused")
	}
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held, sim.Rule{Op: kvtest.OpScan, Key: nsScan, Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	defer held.Advance(time.Hour)
	createShare(t, h, "other", storeB)
	heldUntil(t, h, held, "the namespaces' scan of a pass that listed the refused share")
	deleteShare(t, h, refused)
	// Each second waits for the runner's turn, so the clock stays inside
	// the held scan's bound.
	for range 10 {
		if tagState(t, h, refused.ID) == entity.TagOffloaded {
			break
		}
		select {
		case <-turns:
		default:
		}
		h.Clock().Advance(time.Second)
		select {
		case <-turns:
		case <-time.After(10 * time.Second):
			t.Fatal("no runner turn within 10 s")
		}
	}
	if s := tagState(t, h, refused.ID); s != entity.TagOffloaded {
		t.Fatalf("the refused share after ten runner turns: got %v, want %v", s, entity.TagOffloaded)
	}
	if refusedShare(h.Node().Refused(), refused.ID) {
		t.Fatal("the refused share once unbound: among the refusals, want out")
	}
	moved := h.Node().Moved()
	held.Advance(time.Hour)
	select {
	case <-moved:
	case <-time.After(10 * time.Second):
		t.Fatal("the held pass did not end once its scan was released")
	}
	if refusedShare(h.Node().Refused(), refused.ID) {
		t.Error("the refused share after the pass that listed it before its mark: among the refusals, want out")
	}
}

// grantedCaller creates a user with a UID, so a proven call acts as it,
// grants it admin on sh, and returns a call made as it.
func grantedCaller(t *testing.T, h *nodetest.Harness, sh entity.Share) vfs.Call {
	t.Helper()
	cp := h.Store().ControlPlane()
	u := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "60001"}}}
	if _, err := cp.PutUser(t.Context(), u, 0, entity.Alloc{RID: true}); err != nil {
		t.Fatalf("create a user: %v", err)
	}
	if _, err := cp.SetGrant(t.Context(), entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessAdmin}, 0); err != nil {
		t.Fatalf("grant share %s: %v", sh.Name, err)
	}
	return vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofTicket, Protection: vfs.ProtectionPrivacy, Evidence: vfs.Evidence{Principal: u.Principal}, Client: aClient}}
}

// A call admitted into a share before its mark holds step 3 until it
// answers: the shard is not recorded offloaded while it runs, the step gives
// up at its bound on the node's clock and the runner goes on turning, and
// once the call answers a retry ends the share's live state.
func TestStep3WaitsForTheSharesCallsWithinItsBound(t *testing.T) {
	h, f := faulted(t)
	configure(t, h, storeA)
	sh := createShare(t, h, "a", storeA)
	file := createFiles(t, h, sh, 1)[0]
	call := grantedCaller(t, h, sh)
	ending := make(chan struct{}, 1)
	h.Node().OnEndLiveState(func(entity.ShareID) {
		select {
		case ending <- struct{}{}:
		default:
		}
	})
	g := gateRunner(h)
	defer g.release()
	started(t, h)
	g.took(t)

	held := sim.NewTestClock(time.Unix(0, 0))
	files := keys.Describe(keys.ID([]byte{keys.KindF}, sh.ID[:])) + "*"
	setFault(t, f, held, sim.Rule{Op: kvtest.OpGet, Key: files, Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	defer held.Advance(time.Hour)
	answered := make(chan error, 1)
	go func() {
		_, err := h.Node().Filesystem().GetAttr(context.Background(), call, entity.NewHandle(sh.ID, file.ID))
		answered <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); held.Pending() == 0; runtime.Gosched() {
		if time.Now().After(deadline) {
			t.Fatal("the call into the share did not reach its file's read within 10 s")
		}
	}

	deleteShare(t, h, sh)
	h.Clock().Advance(time.Second)
	select {
	case <-ending:
	case <-time.After(10 * time.Second):
		t.Fatal("step 3 did not begin within 10 s")
	}
	h.Clock().Advance(node.LiveStateBound)
	g.took(t)
	if s := tagState(t, h, sh.ID); s != entity.TagServing {
		t.Fatalf("the shard while a call into the share runs: got %v, want %v", s, entity.TagServing)
	}
	held.Advance(time.Hour)
	select {
	case err := <-answered:
		if !errors.Is(err, vfs.ErrStale) {
			t.Fatalf("the call admitted before the mark, once released after the unbind: got %v, want %v", err, vfs.ErrStale)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held call did not answer once released")
	}
	g.offloaded(t, sh.ID)
}

func refusedShare(refused []entity.RefusedRecord, id entity.ShareID) bool {
	return slices.ContainsFunc(refused, func(r entity.RefusedRecord) bool {
		return r.Record.Kind == entity.ResourceShare && string(r.Record.ID) == string(id[:])
	})
}

// A share reconcile refused is not served: admission answers its first call
// with a Delay, while a share it bound answers the same call.
func TestAdmissionServesOnlyTheRunningSet(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory())
	ctx := t.Context()
	configure(t, h, storeA)
	bound := createShare(t, h, "bound", storeA)
	refused := createShare(t, h, "refused", storeB)
	// A proven caller acts as itself only with a UID, else as the anonymous one.
	u := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "60001"}}}
	cp := h.Store().ControlPlane()
	if _, err := cp.PutUser(ctx, u, 0, entity.Alloc{RID: true}); err != nil {
		t.Fatalf("create a user: %v", err)
	}
	for _, sh := range []entity.Share{bound, refused} {
		if _, err := cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessAdmin}, 0); err != nil {
			t.Fatalf("grant share %s: %v", sh.Name, err)
		}
	}
	started(t, h)
	call := vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofTicket, Protection: vfs.ProtectionPrivacy, Evidence: vfs.Evidence{Principal: u.Principal}, Client: aClient}}
	if _, err := h.Node().Filesystem().StatFS(ctx, call, entity.NewHandle(bound.ID, bound.Root)); err != nil {
		t.Fatalf("a call into the bound share: %v", err)
	}
	if _, err := h.Node().Filesystem().StatFS(ctx, call, entity.NewHandle(refused.ID, refused.Root)); !errors.Is(err, vfs.ErrDelay) {
		t.Errorf("the first call into the refused share: got %v, want %v", err, vfs.ErrDelay)
	}
}

// A removal joined at start whose store stops answering fails Start at the
// filesystem step, its step named, within the store's bound on the node's
// clock, rather than hang it: whether the removal's own record write hangs
// or the shard's removal record write does.
func TestARemovalsHungStoreFailsStartAtItsStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  func(entity.Share) string
	}{
		{"the removal's state", func(sh entity.Share) string {
			return keys.Describe(append(keys.ID([]byte{keys.KindS}, sh.ID[:]), keys.SSt))
		}},
		{"the shard's removal record", func(sh entity.Share) string {
			return keys.Describe(append(keys.Uint64([]byte{keys.KindSH}, uint64(sh.Shard)), keys.SHRm))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, f := faulted(t)
			configure(t, h, storeA)
			sh := createShare(t, h, "a", storeA)
			deleteShare(t, h, sh)
			held := sim.NewTestClock(time.Unix(0, 0))
			setFault(t, f, held, sim.Rule{Op: kvtest.OpSet, Key: tc.key(sh), Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
			defer held.Advance(time.Hour)
			failed := make(chan error, 1)
			go func() { failed <- h.Node().Start(context.Background()) }()
			for deadline := time.Now().Add(10 * time.Second); held.Pending() == 0; runtime.Gosched() {
				if time.Now().After(deadline) {
					t.Fatalf("the write of %s at start did not reach the store within 10 s", tc.name)
				}
			}
			h.Clock().Advance(time.Minute)
			select {
			case err := <-failed:
				if err == nil || !strings.Contains(err.Error(), "filesystem") {
					t.Fatalf("Start with the write of %s hung: %v, want it failed at the filesystem step", tc.name, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("Start with the write of %s hung: not returned within 10 s of the clock passing the store's bound", tc.name)
			}
			// A failed start leaves the listeners open for health; the late
			// write is released before the node stops.
			held.Advance(time.Hour)
			if err := h.Node().Stop(context.Background()); err != nil {
				t.Errorf("Stop after the failed start: %v", err)
			}
		})
	}
}
