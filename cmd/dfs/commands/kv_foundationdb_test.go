//go:build foundationdb

package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv/fdbkvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
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

// openKV opens the cluster a FoundationDB bootstrap names, not Badger.
func TestOpenKVOpensFoundationDB(t *testing.T) {
	// The open writes the node's clock ceiling: the test owns the cluster,
	// as an installation does, and clears it.
	cf := ownCluster(t)
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
		// want is the prefix of the command's error, "" for a nil answer.
		want string
	}{
		{name: "the check refuses", want: "the start check: the node is not initialised"},
		// The signal lands before the check reads the store, so the start
		// ends on it and exits zero, with the store it opened closed.
		{name: "a SIGTERM during the check", signalAt: stagePreflight},
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
				return c
			})
			if r.signalAt != "" {
				// runStart's signal handler is installed by then, so the
				// SIGTERM ends its context rather than the test process.
				ctx = context.WithValue(ctx, stageKey{}, func(name string, signal context.Context) {
					if name != r.signalAt {
						return
					}
					if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
						t.Errorf("SIGTERM to itself: %v", err)
						return
					}
					<-signal.Done()
				})
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

// ownCluster is the cluster file of the FoundationDB cluster the test owns,
// as an installation does, cleared of every key when the test ends. Without
// one, or without DITTOFS_FDB_OWN_CLUSTER to say no other test shares it,
// the test is skipped.
func ownCluster(t *testing.T) string {
	t.Helper()
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		t.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	if os.Getenv("DITTOFS_FDB_OWN_CLUSTER") == "" {
		t.Skip("clears the whole cluster: set DITTOFS_FDB_OWN_CLUSTER=1 on a cluster of its own, with -p 1")
	}
	t.Cleanup(func() { fdbkvtest.ClearAll(t, cf) })
	return cf
}

// foundationDBInstallation writes under dir the bootstrap of a FoundationDB
// installation on the cluster cf names, as dfs init expects one: one journal
// directory and a storage key. A host whose mount the journal's platform
// check refuses skips the test.
func foundationDBInstallation(t *testing.T, dir, cf string) string {
	t.Helper()
	skipUnsuitableMount(t, dir)
	cluster, err := os.ReadFile(cf)
	if err != nil {
		t.Fatal(err)
	}
	// The bootstrap names the cluster file beside it.
	if err := os.WriteFile(filepath.Join(dir, "fdb.cluster"), cluster, 0o600); err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(dir, "device")
	if err := os.Mkdir(dev, 0o700); err != nil {
		t.Fatal(err)
	}
	return bootstrapFile{backend: "foundationdb", journal: dev, node: nodeUUID, key: storageKeyFile(t, dir)}.write(t, dir)
}

// On an empty cluster, dfs init initialises the installation; dfs start then
// serves it until ready and, on SIGTERM, stops and exits zero; and a second
// dfs init refuses it as finished.
func TestInitStartStopOnFoundationDB(t *testing.T) {
	cf := ownCluster(t)
	config := foundationDBInstallation(t, t.TempDir(), cf)
	if _, err := runDfs("init", "--config", config); err != nil {
		t.Fatalf("dfs init on an empty cluster: %v", err)
	}
	serveUntilSIGTERM(t, config)
	if out, err := runDfs("init", "--config", config); !errors.Is(err, install.ErrInstalled) || out != "" {
		t.Fatalf("second dfs init on the cluster: got %q, %v; want nothing printed and ErrInstalled", out, err)
	}
}

// A key the installation did not write, here another node's through fdbkv,
// makes both dfs start and dfs init refuse the cluster with the foreign
// store's own wording, naming the key, never the sentence for a v1 store: v1
// never ran on FoundationDB. Each open writes this node's clock ceiling
// before the store is read, so the refusals add that ceiling to the cluster
// and nothing else but fdbkv's commit marks, which it sweeps itself.
func TestStartAndInitRefuseAForeignClusterOnFoundationDB(t *testing.T) {
	cf := ownCluster(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	config := foundationDBInstallation(t, t.TempDir(), cf)
	foreign := []byte("another program's key")
	other, err := fdbkv.Open(t.Context(), fdbkv.Options{ClusterFile: cf, NodeID: [16]byte{9}})
	if err != nil {
		t.Fatalf("open the cluster as another node: %v", err)
	}
	if err := other.Update(t.Context(), func(tx kv.Txn) error { return tx.Set(foreign, []byte("its value")) }); err != nil {
		t.Fatalf("write %q as another node: %v", foreign, err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	// The cluster's keys but fdbkv's commit marks.
	records := func() [][]byte {
		return slices.DeleteFunc(fdbkvtest.Keys(t, cf), func(k []byte) bool { return bytes.HasPrefix(k, keys.Commit) })
	}
	before := records()

	// start runs as a child, bounded by childBound: a check that stopped
	// refusing would serve, and fail here at the bound's kill, not at the
	// package's timeout.
	code, startOut := runChild(t, config)
	_, initErr := runDfs("init", "--config", config)
	if code == 0 {
		t.Errorf("dfs start on a cluster holding %q: exit 0; want a refusal:\n%s", foreign, startOut)
	}
	refusals := map[string]string{"start": startOut}
	if initErr != nil {
		refusals["init"] = initErr.Error()
	}
	for _, command := range []string{"start", "init"} {
		got := refusals[command]
		if !strings.Contains(got, kvstore.ErrForeignStore.Error()) || !strings.Contains(got, string(foreign)) {
			t.Errorf("dfs %s on a cluster holding %q: %q; want the foreign store's refusal naming the key", command, foreign, got)
		} else if strings.Contains(got, "v1") {
			t.Errorf("dfs %s on a cluster holding %q: %q; want no mention of v1", command, foreign, got)
		}
	}

	ceiling := append(keys.ID([]byte{keys.KindN}, testNode[:]), keys.NClk)
	want := append(slices.Clone(before), ceiling)
	slices.SortFunc(want, bytes.Compare)
	if got := records(); !slices.EqualFunc(got, want, bytes.Equal) {
		t.Fatalf("the cluster's keys after the refusals: %q; want those before them, %q, plus this node's clock ceiling %q, nothing else", got, before, ceiling)
	}
}
