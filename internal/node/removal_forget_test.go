package node_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/google/uuid"
)

// journalHolds reports whether the node's journal names tag among the tags
// it holds records of.
func journalHolds(t *testing.T, h *nodetest.Harness, tag uint32) bool {
	t.Helper()
	for dir, js := range h.Node().Engine().Stats().Journals {
		if js.Stats == nil {
			t.Fatalf("the journal on %s reports no stats", dir)
		}
		if _, ok := js.Stats.Shares[journal.ShareTag(tag)]; ok {
			return true
		}
	}
	return false
}

// written writes a committed extent into file of sh, so the journal holds
// records of sh's tag that only a forget drops.
func written(t *testing.T, h *nodetest.Harness, sh entity.Share, file entity.File) {
	t.Helper()
	if err := writeSpared(t, h, sh.ID, file.ID, 0, pattern("forget", 64<<10)); err != nil {
		t.Fatalf("write into share %s: %v", sh.Name, err)
	}
	if err := jcommit(t, h, sh.ID, file.ID); err != nil {
		t.Fatalf("commit the write into share %s: %v", sh.Name, err)
	}
	if !journalHolds(t, h, sh.Tag) {
		t.Fatalf("the journal after a committed write into share %s: does not name tag %d, want it held", sh.Name, sh.Tag)
	}
}

// until runs runner turns until cond holds, failing after 100.
func (g *gatedRunner) until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 100 {
		if cond() {
			return
		}
		g.next(t)
	}
	t.Fatalf("%s: not after 100 runner turns", what)
}

// A share deleted through the API runs its removal to the end: the journal
// no longer holds its tag once the store records the shard forgotten, and
// the operation ends succeeded with the share's records gone.
func TestAShareDeletedThroughTheAPIEndsSucceeded(t *testing.T) {
	signing := func(context.Context) ([]byte, error) { return []byte("installation"), nil }
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewSigning(admin, signing)))
	configure(t, h, storeA)
	sh := createShare(t, h, "a", storeA)
	file := createFiles(t, h, sh, 3)[0]
	awaited := make(chan struct{}, 16)
	h.Node().OnAwaited(func() { awaited <- struct{}{} })
	g := gateRunner(h)
	defer g.release()
	started(t, h)
	g.took(t)
	caughtUp(t, h)
	written(t, h, sh, file)

	addr, _ := h.Node().Addrs()
	ch := send(t.Context(), addr, http.MethodDelete, "/api/v1/shares/"+sh.ID.String(), "",
		"Idempotency-Key", "delete-a", "If-Match", `"`+strconv.FormatUint(sh.Generation, 10)+`"`)
	// A write that waits for the node to apply it is let go by a second on
	// the node's clock, passed only once its wait is armed.
	var r sent
	select {
	case <-awaited:
		g.next(t)
		r = answeredIn(t, "DELETE /shares/{id} once a second passed", ch)
	case r = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("DELETE /shares/{id}: neither answered nor waiting for the node within 10 s")
	}
	if r.err != nil || r.status != http.StatusAccepted {
		t.Fatalf("DELETE /shares/{id}: answered %d %s (%v), want 202", r.status, r.body, r.err)
	}
	var op apiv1.Operation
	if err := json.Unmarshal(r.body, &op); err != nil {
		t.Fatalf("decode the deletion's operation %s: %v", r.body, err)
	}
	u, err := uuid.Parse(op.ID)
	opID := entity.OperationID(u)
	if err != nil {
		t.Fatalf("the deletion's operation ID %q: %v", op.ID, err)
	}

	g.until(t, "the shard recorded forgotten", func() bool { return tagState(t, h, sh.ID) == entity.TagForgotten })
	if journalHolds(t, h, sh.Tag) {
		t.Errorf("the journal once the shard is recorded forgotten: names tag %d, want it forgotten", sh.Tag)
	}
	g.until(t, "the deletion succeeded", func() bool { return opState(t, h, opID) == entity.OpSucceeded })
	if journalHolds(t, h, sh.Tag) {
		t.Errorf("the journal once the deletion succeeded: names tag %d, want it forgotten", sh.Tag)
	}
	if _, _, err := h.Store().ShareIndex().Share(t.Context(), sh.ID); !merrs.IsNotFoundError(err) {
		t.Errorf("the share's record once the deletion succeeded: read answered %v, want not found", err)
	}
}

// A node restarted after step 4 dropped the share's tag and before it
// recorded the shard forgotten does not attach the share again, and its
// runner drops the tag again, records the shard and ends the deletion
// succeeded.
func TestARestartBetweenTheDropAndTheForgetRecordEndsSucceeded(t *testing.T) {
	h, f := faulted(t)
	configure(t, h, storeA)
	sh := createShare(t, h, "a", storeA)
	file := createFiles(t, h, sh, 3)[0]
	g := gateRunner(h)
	defer g.release()
	started(t, h)
	g.took(t)
	written(t, h, sh, file)

	op := deleteShare(t, h, sh)
	g.offloaded(t, sh.ID)
	rm := keys.Describe(append(keys.Uint64([]byte{keys.KindSH}, uint64(sh.Shard)), keys.SHRm))
	setFault(t, f, sim.NewTestClock(time.Unix(0, 0)), sim.Rule{Op: kvtest.OpSet, Key: rm, Action: sim.FaultAction{Err: errors.New("the forget record lost")}})
	g.until(t, "the tag dropped from the journal", func() bool { return !journalHolds(t, h, sh.Tag) })
	if s := tagState(t, h, sh.ID); s != entity.TagOffloaded {
		t.Fatalf("the shard once its tag is dropped and its record refused: got %v, want %v", s, entity.TagOffloaded)
	}

	// Every write of the shard's record fails until the restart, which
	// opens a store without the fault, so the runner the release lets go
	// cannot record it before the node stops: the node starts again with
	// the tag dropped and the shard recorded offloaded.
	g.release()
	h.Restart(t)
	g = gateRunner(h)
	defer g.release()
	started(t, h)
	if inEngine(h, sh.ID) {
		t.Error("the share after the restart: attached in the engine, want left out")
	}
	if _, err := h.Node().Engine().Read(t.Context(), sh.ID, file.ID, 0, make([]byte, 1)); !errors.Is(err, vfs.ErrStale) {
		t.Errorf("a read of the share after the restart: got %v, want %v", err, vfs.ErrStale)
	}
	g.took(t)
	g.until(t, "the deletion succeeded after the restart", func() bool { return opState(t, h, op.ID) == entity.OpSucceeded })
	if journalHolds(t, h, sh.Tag) {
		t.Errorf("the journal once the deletion succeeded: names tag %d, want it forgotten", sh.Tag)
	}
}

// A share the node refused at its start, its remote store's setting deleted,
// is never attached in the engine, yet the journal still holds its tag's
// records from before: its deletion forgets the tag the store names, so
// once the deletion succeeded the journal holds nothing of it, then or
// after another restart.
func TestADeletionOfAShareRefusedAtStartForgetsItsTag(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	remote := entity.SettingScope{Kind: entity.ScopeRemote, ID: storeA}
	gen, err := h.Store().ControlPlane().PutSetting(t.Context(), entity.Setting{Scope: remote, Key: "endpoint", Value: []byte("https://store.invalid")}, 0)
	if err != nil {
		t.Fatalf("configure the remote store: %v", err)
	}
	sh := createShare(t, h, "a", storeA)
	file := createFiles(t, h, sh, 1)[0]
	g := gateRunner(h)
	defer g.release()
	started(t, h)
	g.took(t)
	written(t, h, sh, file)
	if err := h.Store().ControlPlane().DeleteSetting(t.Context(), remote, "endpoint", gen); err != nil {
		t.Fatalf("delete the remote store's setting: %v", err)
	}

	g.release()
	h.Restart(t)
	g = gateRunner(h)
	defer g.release()
	started(t, h)
	if inEngine(h, sh.ID) {
		t.Fatal("the share on a store with no settings after the restart: attached in the engine, want refused")
	}
	if !journalHolds(t, h, sh.Tag) {
		t.Fatalf("the journal after the restart: does not name tag %d, want its records still held", sh.Tag)
	}
	g.took(t)
	op := deleteShare(t, h, sh)
	g.until(t, "the deletion succeeded", func() bool { return opState(t, h, op.ID) == entity.OpSucceeded })
	if journalHolds(t, h, sh.Tag) {
		t.Errorf("the journal once the deletion of the refused share succeeded: names tag %d, want it forgotten", sh.Tag)
	}

	g.release()
	h.Restart(t)
	g = gateRunner(h)
	defer g.release()
	started(t, h)
	if journalHolds(t, h, sh.Tag) {
		t.Errorf("the journal after a restart past the deletion: names tag %d, want it forgotten", sh.Tag)
	}
}

// A node restarted between a share's mark and its removal's step 3 starts
// with the shard reading serving and joins the removal at the filesystem
// step, which records it offloaded, and
// its runner ends the deletion succeeded with the journal holding nothing of
// the tag.
func TestARestartBetweenTheMarkAndStep3EndsSucceeded(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	configure(t, h, storeA)
	sh := createShare(t, h, "a", storeA)
	file := createFiles(t, h, sh, 1)[0]
	g := gateRunner(h)
	defer g.release()
	started(t, h)
	g.took(t)
	caughtUp(t, h)
	written(t, h, sh, file)
	op := deleteShare(t, h, sh)

	// No runner turn ran since the mark, and the clock does not move before
	// the restart, so the removal is still at its mark when the node stops.
	g.release()
	h.Restart(t)
	read := map[string]entity.TagState{}
	h.Node().OnStep(func(step string) {
		if step == "engine" || step == "filesystem" {
			read[step] = tagState(t, h, sh.ID)
		}
	})
	g = gateRunner(h)
	defer g.release()
	started(t, h)
	atEngine, ok := read["engine"]
	if !ok {
		t.Fatal("the shard's removal before the filesystem step: not read, the engine step not reached")
	}
	if atEngine != entity.TagServing {
		t.Fatalf("the shard's removal before the filesystem step: got %v, want %v, the restart between the mark and step 3", atEngine, entity.TagServing)
	}
	if s := read["filesystem"]; s != entity.TagOffloaded {
		t.Fatalf("the shard's removal once the filesystem step ended: got %v, want %v, step 3 joined at start", s, entity.TagOffloaded)
	}
	g.took(t)
	g.until(t, "the deletion succeeded after the restart", func() bool { return opState(t, h, op.ID) == entity.OpSucceeded })
	if journalHolds(t, h, sh.Tag) {
		t.Errorf("the journal once the deletion succeeded: names tag %d, want it forgotten", sh.Tag)
	}
}
