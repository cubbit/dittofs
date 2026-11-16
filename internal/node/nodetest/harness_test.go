package nodetest

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The harness's installation is a real one: the journal the store records
// for the node's device is the journal in that device, and opens.
func TestTheRecordedJournalOpens(t *testing.T) {
	h := New(t, Watch())
	dev := h.cfg.Bootstrap.Devices[0]
	id, err := h.Store().Lifecycle().JournalID(t.Context(), h.cfg.Bootstrap.Node, entity.JournalDir(dev))
	if err != nil || id == ([16]byte{}) {
		t.Fatalf("JournalID of the node's device: got %x, %v; want the recorded journal", id, err)
	}
	j, err := journal.Open(t.Context(), dev, journal.Version{}, id, nil, journal.WithClock(h.Clock()))
	if err != nil {
		t.Fatalf("journal.Open of the device with the recorded identity: got %v, want it open", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close the journal: %v", err)
	}
}

// Restart builds a new node over the same installation: the store read
// after it holds the installation read before it.
func TestRestartKeepsTheInstallation(t *testing.T) {
	h := New(t, Watch())
	before, err := h.Store().Installation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first := h.Node
	h.Restart(t)
	after, err := h.Store().Installation(t.Context())
	if err != nil {
		t.Fatalf("Installation after the restart: %v", err)
	}
	if after.ID != before.ID || after.Instance != before.Instance {
		t.Errorf("Installation after the restart: got ID %x instance %x, want %x, %x", after.ID, after.Instance, before.ID, before.Instance)
	}
	if h.Node == first {
		t.Error("Restart kept the node it stopped, want a new one")
	}
}
