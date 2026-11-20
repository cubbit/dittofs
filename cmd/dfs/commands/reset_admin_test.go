package commands

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// badgerLocked is the text of Badger's own refusal when another process
// holds its directory lock; Badger exports no error value for it.
const badgerLocked = "Another process is using this Badger database"

// dfs reset-admin over a store a running node holds is refused by Badger's
// directory lock and changes nothing. Once the node stops it resets the
// administrator: the password is printed, the account's not-before is after
// the time before the reset, and the audit record's actor is this host.
func TestResetAdminWhileANodeHoldsTheStore(t *testing.T) {
	h := nodetest.New(t)
	ctx := t.Context()
	cp := h.Store().ControlPlane()
	before, ok, err := cp.UserNamed(ctx, install.AdminName)
	if err != nil || !ok {
		t.Fatalf("UserNamed(%q): %v, %v; want the administrator", install.AdminName, ok, err)
	}
	if err := h.Node().Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	running := true
	t.Cleanup(func() {
		if running {
			_ = h.Node().Stop(context.Background())
		}
	})
	config := bootstrapFile{store: h.StoreDir(), node: uuid.UUID(h.NodeID()).String(), key: h.StorageKeyFile()}.write(t, t.TempDir())

	out, err := runDfs("reset-admin", "--config", config)
	if err == nil || !strings.Contains(err.Error(), badgerLocked) || out != "" {
		t.Fatalf("dfs reset-admin while the node runs: got %q, %v; want Badger's directory lock refusing it, nothing printed", out, err)
	}
	if got, _, err := cp.UserNamed(ctx, install.AdminName); err != nil || got.Generation != before.Generation {
		t.Errorf("administrator after the refused reset: generation %d, %v; want %d unchanged", got.Generation, err, before.Generation)
	}

	if err := h.Node().Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	running = false
	// The command runs on the system clock, so the store time it writes is
	// compared with the system clock's.
	t0 := time.Now()
	out, err = runDfs("reset-admin", "--config", config)
	if err != nil || !strings.Contains(out, "generated password") {
		t.Fatalf("dfs reset-admin with the node stopped: got %q, %v; want the generated password printed", out, err)
	}

	db, err := badgerkv.Open(badgerkv.Options{Dir: h.StoreDir(), NodeID: h.NodeID(), Clock: time.Now})
	if err != nil {
		t.Fatalf("reopen the store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	cp = st.ControlPlane()
	after, _, err := cp.UserNamed(ctx, install.AdminName)
	if err != nil || after.Generation == before.Generation || !after.MustChange {
		t.Fatalf("administrator after the reset: generation %d, must-change %v, %v; want it rewritten with must-change", after.Generation, after.MustChange, err)
	}
	if !after.NotBefore.After(t0) {
		t.Errorf("not-before %v: want it after %v, the system clock before the reset", after.NotBefore, t0)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	var recs []entity.AuditRecord
	for r, err := range cp.AuditRecords(ctx, entity.AuditFilter{}, nil) {
		if err != nil {
			t.Fatalf("AuditRecords: %v", err)
		}
		if bytes.Contains(r.Request, []byte("dfs reset-admin")) {
			recs = append(recs, r)
		}
	}
	if len(recs) != 1 {
		t.Fatalf("the reset's audit records: got %d, want one, the refused run writing none", len(recs))
	}
	if r := recs[0]; r.Actor != (entity.Principal{}) || r.ActorName != host || r.Credential != entity.HostCredential+host || r.Node != h.NodeID() {
		t.Errorf("audit actor: got %v %q %q node %v; want no account, the host %q and the node %v", r.Actor, r.ActorName, r.Credential, r.Node, host, h.NodeID())
	}
}

// An administrator with a uid needs keys.protocol for its NT hash while
// NTLM is on: a bootstrap without it refuses, naming the key, and prints
// nothing.
func TestResetAdminNamesAMissingProtocolKey(t *testing.T) {
	h := nodetest.New(t)
	ctx := t.Context()
	u := entity.User{Principal: entity.NewUser(), Name: "ops", Role: entity.RoleAdmin,
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "4000"}}}
	if _, err := h.Store().ControlPlane().PutUser(ctx, u, 0, entity.Alloc{RID: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.Node().Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	config := bootstrapFile{store: h.StoreDir(), node: uuid.UUID(h.NodeID()).String(), key: h.StorageKeyFile()}.write(t, t.TempDir())
	out, err := runDfs("reset-admin", "--config", config, "--name", "ops")
	if !errors.Is(err, install.ErrNoProtocolKey) || !strings.Contains(err.Error(), "keys.protocol is not set") || out != "" {
		t.Errorf("dfs reset-admin --name ops without keys.protocol: got %q, %v; want keys.protocol named, nothing printed", out, err)
	}
}
