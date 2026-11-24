//go:build foundationdb

package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv/fdbkvtest"
)

// foundationDBConfig is a bootstrap selecting FoundationDB on clusterFile,
// with a node and no Badger path.
func foundationDBConfig(clusterFile string) *bootstrap.Bootstrap {
	var cfg bootstrap.Bootstrap
	cfg.Metadata.Backend = "foundationdb"
	cfg.Metadata.FoundationDB.ClusterFile = clusterFile
	cfg.Node.ID = [16]byte{1}
	return &cfg
}

// A FoundationDB bootstrap names no Badger path, so checkKV accepts it
// without one.
func TestCheckKVFoundationDBNeedsNoBadgerPath(t *testing.T) {
	if err := checkKV(foundationDBConfig("fdb.cluster")); err != nil {
		t.Fatalf("checkKV(foundationdb, no badger path): %v, want nil", err)
	}
}

// openKV gives the client the bootstrap's TLS files: a missing key file is
// refused naming it, which fdbkv does before it touches the client, so no
// cluster is needed. Dropped on the way, the open would reach the client and
// fail on the cluster file instead.
func TestOpenKVPassesTheTLSFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := foundationDBConfig(dir + "/fdb.cluster")
	tls := &cfg.Metadata.FoundationDB.TLS
	tls.CertFile, tls.KeyFile, tls.CAFile = dir+"/cert.pem", dir+"/key.pem", dir+"/ca.pem"
	for _, f := range []string{tls.CertFile, tls.CAFile} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openKV(t.Context(), cfg, newSystemClock())
	if err == nil {
		_ = db.Close()
		t.Fatalf("openKV(foundationdb, no %s): nil error, want one naming it", tls.KeyFile)
	}
	if !strings.Contains(err.Error(), tls.KeyFile) {
		t.Fatalf("openKV(foundationdb, no %s): %v, want an error naming it", tls.KeyFile, err)
	}
}

// openKV gives up on a cluster it cannot reach when its context ends,
// naming the cluster file, rather than waiting for the cluster forever. No
// cluster listens on port 1.
func TestOpenKVGivesUpOnAnUnreachableCluster(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cf, []byte("test:test@127.0.0.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	type result struct {
		db  kv.KV
		err error
	}
	done := make(chan result, 1)
	go func() {
		db, err := openKV(ctx, foundationDBConfig(cf), newSystemClock())
		done <- result{db, err}
	}()
	var r result
	select {
	case r = <-done:
	// A bounded wait is the assertion: well past ctx's deadline, and short of
	// openKV's own, so an open that drops ctx fails here too.
	case <-time.After(15 * time.Second):
		t.Fatalf("openKV(foundationdb on an unreachable %s) did not return 13s after its context ended", cf)
	}
	db, err := r.db, r.err
	if err == nil {
		_ = db.Close()
		t.Fatalf("openKV(foundationdb on an unreachable %s): nil error, want one", cf)
	}
	if !strings.Contains(err.Error(), cf) {
		t.Fatalf("openKV(foundationdb on an unreachable %s): %v, want an error naming the cluster file", cf, err)
	}
}

// dfs start over a cluster no one answers: nothing listens on port 1, so the
// open would wait out its 30 s bound. A v1 layout on the device is refused
// before the store opens, and a SIGTERM as the open begins stops the start,
// exiting zero; both answer well inside that bound, with no cluster. The
// signal lands before the backend's open is called, which then runs its
// first read under a context already ended: nothing marks the moment the
// open starts waiting on the cluster, so the row cannot land it later.
func TestStartBeforeTheClusterAnswers(t *testing.T) {
	for _, r := range []struct {
		name string
		// v1 plants a pre-journal layout on the device.
		v1 bool
		// signalAt is the stage at which the start delivers SIGTERM to
		// itself, "" for none.
		signalAt string
		// want is the prefix of the command's error, "" for a nil answer.
		want string
	}{
		{name: "a v1 layout is refused before the open", v1: true, want: "the start check: refusing to start: "},
		{name: "a SIGTERM as the open begins", signalAt: stageOpen},
	} {
		t.Run(r.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "fdb.cluster"), []byte("test:test@127.0.0.1:1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			device := filepath.Join(dir, "device")
			if r.v1 {
				blobs := filepath.Join(device, "shares", "s1", "blobs")
				if err := os.MkdirAll(blobs, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(blobs, "f1"), []byte("v1 data"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			config := bootstrapFile{backend: "foundationdb", journal: device, node: nodeUUID}.write(t, dir)
			var opened atomic.Int32
			ctx := context.WithValue(t.Context(), kvKey{}, func(inner kv.KV) kv.KV {
				opened.Add(1)
				return inner
			})
			if r.signalAt != "" {
				ctx = context.WithValue(ctx, stageKey{}, sigtermAt(t, r.signalAt))
			}
			root := NewRoot()
			root.SetOut(io.Discard)
			root.SetArgs([]string{"start", "--config", config})
			done := make(chan error, 1)
			go func() { done <- root.ExecuteContext(ctx) }()
			var err error
			select {
			case err = <-done:
			// A bounded wait is the assertion: short of the open's own bound,
			// so a start that waits for the cluster fails here.
			case <-time.After(20 * time.Second):
				t.Fatalf("dfs start over an unreachable cluster did not return within 20s, want an answer before the open's 30s bound")
			}
			switch {
			case r.want == "" && err != nil:
				t.Fatalf("dfs start over an unreachable cluster: %v, want nil", err)
			case r.want != "" && (err == nil || !strings.HasPrefix(err.Error(), r.want)):
				t.Fatalf("dfs start over an unreachable cluster: %v, want an error starting %q", err, r.want)
			}
			if n := opened.Load(); n != 0 {
				t.Fatalf("dfs start over an unreachable cluster opened the store %d times, want never", n)
			}
		})
	}
}

// sigtermAt is a stageKey hook that sends the test process SIGTERM at stage
// name and returns once runStart's signal context ended. runStart's handler
// is installed by then, so the SIGTERM ends its context rather than the test
// process.
func sigtermAt(t *testing.T, name string) func(string, context.Context) {
	return func(at string, signal context.Context) {
		if at != name {
			return
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Errorf("SIGTERM to itself: %v", err)
			return
		}
		<-signal.Done()
	}
}

// sigtermInView sends the test process SIGTERM inside its first View, once
// the backend's transaction is open, and runs fn only once a read through
// that transaction fails: the read the start check makes then runs canceled.
type sigtermInView struct {
	kv.KV
	t    *testing.T
	once atomic.Bool
}

func (s *sigtermInView) View(ctx context.Context, fn func(kv.Reader) error) error {
	if !s.once.CompareAndSwap(false, true) {
		return s.KV.View(ctx, fn)
	}
	return s.KV.View(ctx, func(r kv.Reader) error {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			s.t.Errorf("SIGTERM to itself: %v", err)
			return err
		}
		<-ctx.Done()
		// The backend cancels its transaction from a goroutine of its own:
		// poll the condition itself, a read that fails, with a generous
		// deadline as the backstop.
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := r.Get([]byte("probe")); err != nil && !errors.Is(err, kv.ErrNotFound) {
				break
			}
			if time.Now().After(deadline) {
				s.t.Errorf("a read in a View whose context ended still answered 20s later, want it to fail once the backend canceled its transaction")
				return errors.New("the backend never canceled the read")
			}
			time.Sleep(time.Millisecond)
		}
		return fn(r)
	})
}

// openKV opens the cluster a FoundationDB bootstrap names, not Badger.
func TestOpenKVOpensFoundationDB(t *testing.T) {
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		t.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	if !ownFDBCluster {
		t.Skip("clears the whole cluster: set DITTOFS_FDB_OWN_CLUSTER=1 on a cluster of its own, with -p 1")
	}
	// The open writes the node's clock ceiling: the test owns the cluster,
	// as an installation does, and clears it.
	t.Cleanup(func() { fdbkvtest.ClearAll(t, cf) })
	db, err := openKV(t.Context(), foundationDBConfig(cf), newSystemClock())
	if err != nil {
		t.Fatalf("openKV(foundationdb on %s): %v", cf, err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("closing the store: %v", err)
	}
	if _, ok := db.(*fdbkv.Store); !ok {
		t.Fatalf("openKV(foundationdb) opened a %T, want *fdbkv.Store", db)
	}
}

// closeRecorder passes every call through to the store it wraps and records
// each Close that reaches it.
type closeRecorder struct {
	kv.KV
	closes atomic.Int32
}

func (c *closeRecorder) Close() error {
	c.closes.Add(1)
	return c.KV.Close()
}

// dfs start opens a FoundationDB store before the start check, so every
// return before node.New takes the store must close it, the command's
// answer either way. Each row runs dfs start over an empty cluster, with the
// store wrapped where it opens, and asserts the answer and that the store saw
// exactly one Close by the time the command returned.
func TestStartClosesTheStoreOnFoundationDB(t *testing.T) {
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		t.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	if !ownFDBCluster {
		t.Skip("clears the whole cluster: set DITTOFS_FDB_OWN_CLUSTER=1 on a cluster of its own, with -p 1")
	}
	cluster, err := os.ReadFile(cf)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		name string
		// signalAt is the stage at which the start delivers SIGTERM to
		// itself, "" for none.
		signalAt string
		// signalInRead delivers SIGTERM inside the start check's store
		// read instead, so the read the backend answers runs canceled.
		signalInRead bool
		// want is the prefix of the command's error, "" for a nil answer.
		want string
	}{
		{name: "the check refuses", want: "the start check: the node is not initialised"},
		// The signal lands once the check opened the store and before it
		// reads it, so the start ends on it and exits zero, with the store
		// it opened closed.
		{name: "a SIGTERM during the check", signalAt: stageOpened},
		// The read fails with FoundationDB's own error, which the check
		// answers as the signal's: the start exits zero.
		{name: "a SIGTERM during the check's store read", signalInRead: true},
	} {
		t.Run(r.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Cleanup(func() { fdbkvtest.ClearAll(t, cf) })
			dir := t.TempDir()
			// The bootstrap names the cluster file beside it.
			if err := os.WriteFile(filepath.Join(dir, "fdb.cluster"), cluster, 0o600); err != nil {
				t.Fatal(err)
			}
			config := bootstrapFile{backend: "foundationdb", journal: filepath.Join(dir, "device"), node: nodeUUID}.write(t, dir)
			var opened []*closeRecorder
			ctx := context.WithValue(t.Context(), kvKey{}, func(inner kv.KV) kv.KV {
				c := &closeRecorder{KV: inner}
				opened = append(opened, c)
				if r.signalInRead {
					// The recorder stays outermost so it still sees Close.
					c.KV = &sigtermInView{KV: inner, t: t}
				}
				return c
			})
			if r.signalAt != "" {
				ctx = context.WithValue(ctx, stageKey{}, sigtermAt(t, r.signalAt))
			}
			root := NewRoot()
			root.SetOut(io.Discard)
			root.SetArgs([]string{"start", "--config", config})
			err := root.ExecuteContext(ctx)
			switch {
			case r.want == "" && err != nil:
				t.Fatalf("dfs start over an empty cluster: %v, want nil", err)
			case r.want != "" && (err == nil || !strings.HasPrefix(err.Error(), r.want)):
				t.Fatalf("dfs start over an empty cluster: %v, want an error starting %q", err, r.want)
			}
			if len(opened) != 1 {
				t.Fatalf("dfs start over an empty cluster opened the store %d times, want once", len(opened))
			}
			if n := opened[0].closes.Load(); n != 1 {
				t.Fatalf("dfs start over an empty cluster: the store saw %d Close calls by the time the command returned, want 1", n)
			}
		})
	}
}
