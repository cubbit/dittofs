package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// dfs reset-admin over a store a running node holds is refused by Badger's
// directory lock and changes nothing. Once the node stops it resets the
// administrator: the password is printed, a session signed in before the
// reset is older than the account's not-before, and the audit record's actor
// is this host.
func TestResetAdminWhileANodeHoldsTheStore(t *testing.T) {
	h := nodetest.New(t)
	ctx := t.Context()
	cp := h.Store().ControlPlane()
	before, ok, err := cp.UserNamed(ctx, install.AdminName)
	if err != nil || !ok {
		t.Fatalf("UserNamed(%q): %v, %v; want the administrator", install.AdminName, ok, err)
	}
	// No sign-in path is built, so the session is written as the store's
	// own tests write one.
	sess := entity.Session{ID: [16]byte{1}, Account: before.Principal, Refresh: [32]byte{1},
		Created: h.Clock().Now(), Expires: h.Clock().Now().Add(24 * time.Hour)}
	if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.PutSession(sess) }); err != nil {
		t.Fatalf("PutSession: %v", err)
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

	dir := t.TempDir()
	config := bootstrapFile{store: h.StoreDir(), node: uuid.UUID(h.NodeID()).String()}.write(t, dir)
	f, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(f, "keys:\n  storage:\n    kind: local\n    local:\n      file: '%s'\n", h.StorageKeyFile())
	if err := errors.Join(err, f.Close()); err != nil {
		t.Fatal(err)
	}

	out, err := runDfs("reset-admin", "--config", config)
	if err == nil || !strings.Contains(err.Error(), "is a node running") || out != "" {
		t.Fatalf("dfs reset-admin while the node runs: got %q, %v; want the store's lock refusing it, nothing printed", out, err)
	}
	if got, _, err := cp.UserNamed(ctx, install.AdminName); err != nil || got.Generation != before.Generation {
		t.Errorf("administrator after the refused reset: generation %d, %v; want %d unchanged", got.Generation, err, before.Generation)
	}

	if err := h.Node().Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	running = false
	out, err = runDfs("reset-admin", "--config", config)
	if err != nil || !strings.Contains(out, "generated password") {
		t.Fatalf("dfs reset-admin with the node stopped: got %q, %v; want the generated password printed", out, err)
	}

	db, err := badgerkv.Open(badgerkv.Options{Dir: h.StoreDir(), NodeID: h.NodeID(), Clock: h.Clock().Now})
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
	if !sess.Created.Before(after.NotBefore) {
		t.Errorf("not-before %v: want it after the session signed in before the reset, created %v", after.NotBefore, sess.Created)
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
	if r := recs[0]; r.Actor != (entity.Principal{}) || r.ActorName != host || r.Credential != "host:"+host || r.Node != h.NodeID() {
		t.Errorf("audit actor: got %v %q %q node %v; want no account, the host %q and the node %v", r.Actor, r.ActorName, r.Credential, r.Node, host, h.NodeID())
	}
}
