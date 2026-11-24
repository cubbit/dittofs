package nodetest

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The harness's installation is a real one, in memory too: the journal the
// store records for the node's device is the journal in that device, and
// opens.
func TestTheRecordedJournalOpens(t *testing.T) {
	h := New(t, Watch(), InMemory())
	dev := h.cfg.Bootstrap.Journals[0].Path

	id, err := h.Store().Lifecycle().JournalID(
		t.Context(),
		h.cfg.Bootstrap.Node.ID,
		entity.JournalDir(dev),
	)
	if err != nil || id == ([16]byte{}) {
		t.Fatalf("JournalID of the node's device: got %x, %v; want the recorded journal", id, err)
	}
	j, err := journal.Open(t.Context(), dev, journal.Version{}, id, nil, journal.WithClock(h.Clock()), journaltest.SuitableMount())
	if err != nil {
		t.Fatalf("journal.Open of the device with the recorded identity: got %v, want it open", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close the journal: %v", err)
	}
}

// Restart builds a new node over the same installation: the store read
// after it holds the journal recorded before it, which only the old
// directory holds. A Restart over a fresh directory stops in the harness
// before this, or, if it initialises one, fails here.
func TestRestartKeepsTheInstallation(t *testing.T) {
	h := New(t, Watch())
	dev := entity.JournalDir(h.cfg.Bootstrap.Journals[0].Path)
	jBefore, err := h.Store().Lifecycle().JournalID(t.Context(), nodeID, dev)
	if err != nil {
		t.Fatal(err)
	}
	h.Restart(t)
	jAfter, err := h.Store().Lifecycle().JournalID(t.Context(), nodeID, dev)
	if err != nil || jAfter != jBefore {
		t.Errorf("JournalID of the device after the restart: got %x, %v; want %x", jAfter, err, jBefore)
	}
}

// New and Restart both build through the constructor WithNew hands them,
// and Node is the one built last.
func TestRestartBuildsThroughWithNew(t *testing.T) {
	var built []*node.Node
	h := New(t, Watch(), WithNew(func(cfg node.Config) (*node.Node, error) {
		n, err := node.New(cfg)
		if err == nil {
			built = append(built, n)
		}
		return n, err
	}))
	h.Restart(t)
	if len(built) != 2 {
		t.Fatalf("nodes built through WithNew's constructor: got %d, want 2", len(built))
	}
	if h.Node() != built[1] {
		t.Errorf("Node after Restart: got %p, want the second node %p, not the first %p", h.Node(), built[1], built[0])
	}
}

// The harness's installation holds the one account the initialisation step
// creates. A node started over it once that account is deleted creates none:
// only the initialisation step creates the administrator.
func TestStartCreatesNoAccount(t *testing.T) {
	h := New(t, Watch())
	ctx := t.Context()
	cp := h.Store().ControlPlane()
	admin, ok, err := cp.UserNamed(ctx, install.AdminName)
	if err != nil || !ok || admin.Role != entity.RoleAdmin {
		t.Fatalf("UserNamed(%q) after New: got %v, %t, %v; want the administrator", install.AdminName, admin, ok, err)
	}
	if err := cp.DeleteUser(ctx, admin.Principal, admin.Generation); err != nil {
		t.Fatalf("DeleteUser of the administrator: %v", err)
	}
	h.Restart(t)
	if err := h.Node().Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Node().Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	for u, err := range h.Store().ControlPlane().Users(ctx, "") {
		t.Errorf("user after the start: %v (%v), want none", u, err)
	}
}
