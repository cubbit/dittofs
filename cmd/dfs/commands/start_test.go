package commands

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/google/uuid"
)

// childEnv, when set, makes TestStartChild run dfs with the arguments it
// holds, one per line, as the dfs binary's main would.
const childEnv = "DFS_START_CHILD_ARGS"

// signalAtEnv, when set to one of runStart's stages, makes the child send
// itself SIGTERM at that stage and wait for the signal to land before it goes
// on: the signal lands at a known stage, at no particular timing.
const signalAtEnv = "DFS_START_CHILD_SIGTERM_AT"

// holdEnv, when set to an operation, a key as keys.Describe renders it, a
// count and an arming prefix in hex, one per line, makes the child hold the
// count-th store call of the operation on the key made once the store holds
// a row under the prefix, any call when the prefix is empty: it prints
// heldLine and never answers the call, so the test kills the child at that
// call, at no particular timing.
const holdEnv = "DFS_START_CHILD_HOLD"

// heldLine is the line the child prints once it holds the call holdEnv names.
const heldLine = "dfs start child: held"

// holdAt is the sim.Fault holdEnv sets: it passes every call before the
// n-th of op on key made once store holds a row under arm, and holds that
// one and every later one forever, once it printed heldLine; so a caller
// that gives up on a held call and retries is held again.
//
// key is the key's keys.Describe text, which is what FaultKV hands At, and
// Describe renders an ID by its first 4 bytes: a row names the whole key it
// means, and another key that renders alike, at 2^-32 per pair, is held too.
//
// decision: the hold names a store scan, not a point in the node, and stops
// the caller the row means only while nothing else scans that prefix once
// armed: the shares' rows are scanned by the reconcile's share list read,
// and by Removing and Tags only at start, before any share exists; a
// share's file rows only by step 4's batch, shardDropBatch. A new scanner of
// either prefix overturns it: the hold then moves to a named hook in the
// node, as stageKey is in runStart.
type holdAt struct {
	op, key string
	n       int64
	arm     []byte
	store   kv.KV
	seen    atomic.Int64
	once    sync.Once
}

func (h *holdAt) At(op string, key []byte) (time.Duration, int, error) {
	if op == h.op && string(key) == h.key && h.armed() && h.seen.Add(1) >= h.n {
		h.once.Do(func() { fmt.Println(heldLine) })
		<-make(chan struct{})
	}
	return 0, 0, nil
}

// armed reports whether the store holds a committed row under h.arm, so
// the call being consulted counts only once it does. Its View goes to the
// inner store, never through the fault, so it consults nothing and cannot
// hold itself or count as a call of its own.
func (h *holdAt) armed() bool {
	if len(h.arm) == 0 {
		return true
	}
	found := false
	_ = h.store.View(context.Background(), func(r kv.Reader) error {
		for _, err := range r.Scan(h.arm, nil) {
			found = err == nil
			break
		}
		return nil
	})
	return found
}

// fdbClusterEnv names the FoundationDB cluster the foundationdb tests run on,
// and fdbOwnClusterEnv says no other test shares it, so a test may clear it.
const (
	fdbClusterEnv    = "DITTOFS_FDB_CLUSTER_FILE"
	fdbOwnClusterEnv = "DITTOFS_FDB_OWN_CLUSTER"
)

// ownFDBCluster is whether the shell set fdbOwnClusterEnv, read before
// TestMain drops it.
var ownFDBCluster bool

// TestMain drops the DITTOFS_ variables the developer's shell exports before
// any test runs: the start refuses a name it does not know, so a stray one
// would fail every start test for a reason the test does not name. A test
// that needs one sets it itself. The child process keeps its environment,
// which holds what the parent test chose for it.
//
// decision: the two names that point the foundationdb tests at a cluster
// survive, or those tests skip wherever they run. fdbClusterEnv stays in the
// environment: the start's scan passes it as an allowed name and nothing the
// start runs reads it. fdbOwnClusterEnv is a name only tests read, which the
// scan refuses, so only its value is kept and the variable still goes; a
// test reads ownFDBCluster instead. Neither reaches a child, whose
// environment drops every DITTOFS_ name the host has.
func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "" {
		ownFDBCluster = os.Getenv(fdbOwnClusterEnv) != ""
		for _, kv := range os.Environ() {
			name, _, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(name, "DITTOFS_") && name != fdbClusterEnv {
				_ = os.Unsetenv(name)
			}
		}
	}

	os.Exit(m.Run())
}

// TestStartChild is the child process the start tests run: dfs itself.
func TestStartChild(t *testing.T) {
	args := os.Getenv(childEnv)
	if args == "" {
		t.Skip("runs only as the start tests' child process")
	}
	root := NewRoot()
	root.SetArgs(strings.Split(args, "\n"))
	ctx := context.Background()
	if at := os.Getenv(signalAtEnv); at != "" {
		ctx = context.WithValue(ctx, stageKey{}, func(name string, signal context.Context) {
			if name != at {
				return
			}
			self, err := os.FindProcess(os.Getpid())
			if err == nil {
				err = self.Signal(syscall.SIGTERM)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: SIGTERM to itself: %v\n", err)
				os.Exit(2)
			}
			<-signal.Done()
		})
	}
	if hold := os.Getenv(holdEnv); hold != "" {
		parts := strings.Split(hold, "\n")
		var (
			n   int64
			arm []byte
			err = fmt.Errorf("%d lines", len(parts))
		)
		if len(parts) == 4 {
			if n, err = strconv.ParseInt(parts[2], 10, 64); err == nil {
				arm, err = hex.DecodeString(parts[3])
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s=%q: %v; want an operation, a key, a count and a prefix, one per line\n", holdEnv, hold, err)
			os.Exit(2)
		}
		ctx = context.WithValue(ctx, kvKey{}, func(inner kv.KV) kv.KV {
			f := kvtest.NewFaultKV(inner)
			f.SetFault(&holdAt{op: parts[0], key: parts[1], n: n, arm: arm, store: inner})
			return f
		})
	}
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// childBound is how long a child runs before the test kills it: the one
// bound on every wait for it, so a child that never logs or answers what the
// test waits for ends its output, and so the test's read.
const childBound = 60 * time.Second

// startChild runs dfs start over the bootstrap at config, its output, both
// streams, piped back; with config empty it passes no --config, and the env
// names the bootstrap. The child inherits no DITTOFS_ variable from the
// host, whose logging or endpoints would change what the tests read: it gets
// the storage key's passphrase and the env the test hands it.
func startChild(t *testing.T, config string, env ...string) (*exec.Cmd, io.Reader) {
	t.Helper()
	return startChildIn(t, config, "", env...)
}

// runtimeDir is a runtime directory of the child's own, named short because a
// socket's path is capped at about 104 bytes on some hosts and t.TempDir
// names its directory after the test. It keeps the child off the host's own
// runtime directory.
func runtimeDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "dfs")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})

	return dir
}

// startChildIn is startChild run in the working directory dir (the test's
// own when empty), with env added after the process's, so it wins.
func startChildIn(t *testing.T, config, dir string, env ...string) (*exec.Cmd, io.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartChild$")
	cmd.Dir = dir
	host := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "DITTOFS_") })
	args := "start"
	if config != "" {
		args += "\n--config\n" + config
	}
	cmd.Env = append(host, childEnv+"="+args, "XDG_STATE_HOME="+t.TempDir(),
		"XDG_RUNTIME_DIR="+runtimeDir(t), "DITTOFS_ENCRYPTION_PASSPHRASE="+storagePassphrase)
	cmd.Env = append(cmd.Env, env...)
	r, w := io.Pipe()
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}
	go func() { _ = w.CloseWithError(cmd.Wait()) }()
	backstop := time.AfterFunc(childBound, func() { _ = cmd.Process.Kill() })
	t.Cleanup(func() {
		backstop.Stop()
		_ = cmd.Process.Kill()
	})
	return cmd, r
}

// runChild runs dfs start over config to its end, returning its exit code
// and its output.
func runChild(t *testing.T, config string, env ...string) (int, string) {
	t.Helper()
	cmd, out := startChild(t, config, env...)
	b, _ := io.ReadAll(out)
	return cmd.ProcessState.ExitCode(), string(b)
}

// The node's identity as the test's bootstraps write it, and its bytes.
const nodeUUID = "00112233-4455-6677-8899-aabbccddeeff"

var testNode = entity.NodeID(uuid.MustParse(nodeUUID))

// bootstrapFile is a bootstrap the tests write: every listener on loopback
// port 0 but the API's at api when set, logging to stderr. An empty field is
// left out of the file.
type bootstrapFile struct {
	backend, store, journal, node, key, api string
}

// write writes b under dir as config.yaml and returns its path.
func (b bootstrapFile) write(t *testing.T, dir string) string {
	t.Helper()
	var body strings.Builder
	body.WriteString("logging:\n  output: stderr\n")
	if b.node != "" {
		fmt.Fprintf(&body, "node:\n  id: %s\n", b.node)
	}
	body.WriteString("metadata:\n")
	if b.backend != "" {
		fmt.Fprintf(&body, "  backend: %s\n", b.backend)
	}
	if b.backend == "foundationdb" { // the loader requires its cluster file
		fmt.Fprintf(&body, "  foundationdb:\n    cluster_file: '%s'\n", filepath.Join(dir, "fdb.cluster"))
	}
	if b.store != "" {
		fmt.Fprintf(&body, "  badger:\n    path: '%s'\n", b.store)
	}
	if b.journal != "" {
		fmt.Fprintf(&body, "journals:\n  - path: '%s'\n", b.journal)
	}
	if b.key != "" {
		fmt.Fprintf(&body, "keys:\n  storage:\n    kind: local\n    local:\n      file: '%s'\n", b.key)
	}
	fmt.Fprintf(&body, `api:
  listen: ['%s']
metrics:
  listen: '127.0.0.1:0'
adapters:
  listen: ['127.0.0.1']
`, cmp.Or(b.api, "127.0.0.1:0"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// installed writes the bootstrap of the installation under dir and returns
// its path.
func installed(t *testing.T, dir string) string {
	t.Helper()
	return installation(t, dir).write(t, dir)
}

// skipUnsuitableMount skips the test when the journal's platform check
// refuses the mount holding dir. The child opens its journal under the
// platform's own check, which a test cannot hand a probe, so the check runs
// here first, over the same mount.
func skipUnsuitableMount(t *testing.T, dir string) {
	t.Helper()
	scratch := filepath.Join(dir, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Init(t.Context(), scratch, [16]byte{1}); errors.Is(err, journal.ErrUnsuitableStorage) {
		t.Skipf("the journal's platform check refuses the mount holding %s (%v); set TMPDIR to a directory on a suitable mount", dir, err)
	} else if err != nil {
		t.Fatalf("journal init on the scratch directory: %v", err)
	}
}

// installation is the bootstrap of an installation under dir, as dfs init
// leaves one: the metadata store at dir/store, one journal at dir/device,
// recorded for the test's node, and the storage key at dir/storage.key. A
// host whose mount the journal's platform check refuses skips the test.
func installation(t *testing.T, dir string) bootstrapFile {
	t.Helper()
	ctx := t.Context()
	skipUnsuitableMount(t, dir)

	store, dev := filepath.Join(dir, "store"), filepath.Join(dir, "device")
	install := [16]byte{'s', 't', 'a', 'r', 't'}
	if err := os.Mkdir(dev, 0o700); err != nil {
		t.Fatal(err)
	}
	jid, err := journal.Init(ctx, dev, install, journaltest.SuitableMount())
	if err != nil {
		t.Fatalf("journal init: %v", err)
	}
	db, err := badgerkv.Open(badgerkv.Options{Dir: store, NodeID: testNode})
	if err != nil {
		t.Fatalf("open the KV: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close the KV: %v", err)
		}
	}()
	if err := kvstore.Init(ctx, db, install); err != nil {
		t.Fatalf("store init: %v", err)
	}
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	if err := st.RecordJournal(ctx, jid, testNode, entity.JournalDir(dev)); err != nil {
		t.Fatalf("record the journal: %v", err)
	}
	return bootstrapFile{store: store, journal: dev, node: nodeUUID, key: storageKeyFile(t, dir)}
}

// storagePassphrase wraps the storage key the tests write; startChild hands
// it to every child.
const storagePassphrase = "dfs commands test passphrase"

// storageKeyFile writes a storage key file under dir, its passphrase set in
// this process's environment for the commands run in it, and returns its path.
func storageKeyFile(t *testing.T, dir string) string {
	t.Helper()
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", storagePassphrase)
	b, err := keyprovider.GenerateKeyFile(storagePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "storage.key")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Over a directory holding no installation, dfs start exits non-zero at its
// start check, naming the missing initialisation, and creates no store
// directory.
func TestStartNoInstallation(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	code, out := runChild(t, bootstrapFile{store: store, journal: filepath.Join(dir, "device"), node: nodeUUID}.write(t, dir))
	if code == 0 || !strings.Contains(out, "Error: the start check: the node is not initialised") ||
		!strings.Contains(out, "dfs init") {
		t.Fatalf("dfs start over no installation: exit %d, output:\n%s\nwant non-zero naming the start check's not-initialised verdict", code, out)
	}
	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("store directory %s after the refusal: %v, want none created", store, err)
	}
}

// Beside v1's controlplane.db, over the store the layout leaves — empty, or
// holding whatever it left — dfs start refuses naming the database: the v1
// refusal runs first, before the configuration's own checks and the start
// check, so a v1 deployment's partial bootstrap is refused as v1 rather than
// for a missing key, and an operator is never told to run dfs init into a v1
// deployment's directory: the v1 sentence names dfs init only on empty
// storage.
func TestStartRefusesV1Database(t *testing.T) {
	for _, tc := range []struct {
		name  string
		b     func(store, dir string) bootstrapFile
		byEnv bool // the bootstrap named by DITTOFS_CONFIG, not --config
	}{
		{"a full bootstrap", func(store, dir string) bootstrapFile {
			return bootstrapFile{store: store, journal: filepath.Join(dir, "device"), node: nodeUUID}
		}, false},
		{"a bootstrap naming no store or node", func(_, dir string) bootstrapFile {
			return bootstrapFile{journal: filepath.Join(dir, "device")}
		}, false},
		{"a bootstrap named by DITTOFS_CONFIG", func(store, dir string) bootstrapFile {
			return bootstrapFile{store: store, journal: filepath.Join(dir, "device"), node: nodeUUID}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := filepath.Join(dir, "store")
			if err := os.Mkdir(store, 0o700); err != nil {
				t.Fatal(err)
			}
			v1 := filepath.Join(dir, "controlplane.db")
			if err := os.WriteFile(v1, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			config := tc.b(store, dir).write(t, dir)
			var code int
			var out string
			if tc.byEnv {
				// XDG_CONFIG_HOME points away, so only the env's file can
				// lead the check to the v1 database beside it.
				code, out = runChild(t, "", "DITTOFS_CONFIG="+config, "XDG_CONFIG_HOME="+t.TempDir())
			} else {
				code, out = runChild(t, config)
			}
			sentence := v1Sentence("a control-plane database", v1)
			if code == 0 || !strings.Contains(out, "Error: "+sentence) {
				t.Fatalf("dfs start beside %s over %s: exit %d, output:\n%s\nwant non-zero naming it", v1, tc.name, code, out)
			}
			if entries, err := os.ReadDir(store); err != nil || len(entries) != 0 {
				t.Errorf("store directory %s after the refusal: %d entries (%v), want none: nothing opened", store, len(entries), err)
			}
			if strings.Contains(strings.ReplaceAll(out, sentence, ""), "dfs init") {
				t.Errorf("dfs start beside v1's database: the output advises running dfs init outside the v1 sentence, which sends it to empty storage (%s); over a v1 deployment's directory that is the advice that destroys data", out)
			}
		})
	}
}

// writeV1Layout makes a pre-journal (v1) layout under root: a share's journal
// directory holding a file.
func writeV1Layout(t *testing.T, root string) {
	t.Helper()

	dir := filepath.Join(root, "shares", "s", "journal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dfs start looks for a pre-journal (v1) layout on each journal device and in
// `blocks` beside the store directory, v1's default root: a layout there
// refuses naming it. The working directory and a state directory unrelated to
// the store are not read, so the check answers these rows with its own
// verdict and not a v1 one, and a row without a layout on a probed root
// asserts on the check itself, which runs before anything opens: a whole
// start over no installation goes on to open the journal, whose platform
// check a host with an unsuitable mount fails, and the row would skip on
// every run there.
//
// The store directory's own parent is not read either, because the start
// check takes one extra root; there is no row for that gap, so a check that
// probes the parent does not break a test.
func TestStartProbesV1Layouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		// at says where the v1 layout is: a key of the roots below.
		at      string
		refused bool

		// defaultStore leaves metadata.badger.path unset, so the store is the
		// default one in the state directory the child's environment names.
		defaultStore bool
	}{
		{"on a device", "device", true, false},
		{"in blocks beside the store", "blocks", true, false},
		{"in the working directory", "cwd", false, false},
		{"in an unrelated state directory", "state", false, false},
		{"in blocks beside the default store", "state", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			roots := map[string]string{
				"device": filepath.Join(dir, "device"),
				"blocks": filepath.Join(dir, "blocks"),
				"cwd":    filepath.Join(dir, "cwd"),
				"state":  filepath.Join(dir, "state", "dittofs", "blocks"),
			}

			for _, d := range []string{roots["device"], roots["cwd"], filepath.Join(dir, "state")} {
				if err := os.Mkdir(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			writeV1Layout(t, roots[tc.at])

			if !tc.refused {
				// The start passes the check the same roots the rows give it,
				// and the mount it is handed suits the host, as the node's
				// own journal options channel does.
				store := filepath.Join(dir, "store")
				devices := []string{roots["device"]}

				err := nodetest.Preflight(t.Context(), store, v1ProbeRoot(store, devices),
					devices, newSystemClock())
				if err == nil ||
					!strings.Contains(err.Error(), "the node is not initialised") {
					t.Errorf(
						"the start check with a v1 layout %s: %v, want the not-initialised verdict",
						tc.name, err,
					)
				}

				if err != nil &&
					strings.Contains(err.Error(), "upgrading from v1 is not supported") {
					t.Errorf(
						"the start check with a v1 layout %s: %v, want no v1 verdict",
						tc.name, err,
					)
				}

				return
			}

			file := bootstrapFile{
				store:   filepath.Join(dir, "store"),
				journal: roots["device"],
				node:    nodeUUID,
			}

			if tc.defaultStore {
				file.store = ""
			}

			config := file.write(t, dir)

			cmd, r := startChildIn(
				t, config, roots["cwd"], "XDG_STATE_HOME="+filepath.Join(dir, "state"),
			)
			b, _ := io.ReadAll(r)
			out, code := string(b), cmd.ProcessState.ExitCode()

			if code == 0 {
				t.Fatalf("dfs start: exit 0, output:\n%s\nwant a refusal", out)
			}

			if !strings.Contains(out, "upgrading from v1 is not supported") {
				t.Errorf(
					"dfs start with a v1 layout %s: want the v1 refusal; output:\n%s",
					tc.name, out,
				)
			}

			named := filepath.Join(roots[tc.at], "shares", "s", "journal")
			if !strings.Contains(out, named) {
				t.Errorf("dfs start: the refusal does not name %s:\n%s", named, out)
			}
		})
	}
}

// The refusals dfs start answers before anything opens, each naming the
// piece of the configuration it is: the configuration's own checks, then the
// start check's verdicts over the storage. Nothing is opened by a refusal.
func TestStartRefusesTheStore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, tc := range []struct {
		name string
		b    func(dir string) bootstrapFile
		want string
		// noFDB marks a refusal only a build without FoundationDB answers.
		noFDB bool
	}{
		{"foundationdb in a build without it", func(dir string) bootstrapFile {
			return bootstrapFile{
				backend: "foundationdb",
				journal: filepath.Join(dir, "device"),
				node:    nodeUUID,
			}
		}, `metadata.backend "foundationdb" is opened only by a build with the foundationdb tag`, true},
		{"default store path, nothing installed", func(dir string) bootstrapFile {
			return bootstrapFile{journal: filepath.Join(dir, "device"), node: nodeUUID}
		}, "the start check: the node is not initialised", false},
		{"no node", func(dir string) bootstrapFile {
			return bootstrapFile{store: filepath.Join(dir, "store"), journal: filepath.Join(dir, "device")}
		}, "node.id is not set", false},
		{"no store directory", func(dir string) bootstrapFile {
			return bootstrapFile{
				store:   filepath.Join(dir, "missing"),
				journal: filepath.Join(dir, "device"),
				node:    nodeUUID,
			}
		}, "the start check: the node is not initialised", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if foundationDB && tc.noFDB {
				t.Skip("this build opens foundationdb")
			}
			dir := t.TempDir()
			root := NewRoot()
			root.SetOut(io.Discard)
			root.SetArgs([]string{"start", "--config", tc.b(dir).write(t, dir)})
			err := root.Execute()
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("dfs start: %v, want an error starting %q", err, tc.want)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Errorf("%s after the refusal holds %d entries, want only the bootstrap: nothing opened", dir, len(entries))
			}
		})
	}
}

// v1Sentence is the sentence every v1 find refuses with, written out here
// rather than read from the code, so that a change to the code's wording
// fails the tests that pin it.
func v1Sentence(found, path string) string {
	return "refusing to start: " + found + " at " + path + " is from v1; " +
		"upgrading from v1 is not supported: start a new installation with dfs init on empty storage"
}

// refuseV1 refuses a database beside the bootstrap or at a fixed path,
// naming it, and the path beside the bootstrap when it cannot check it; it
// skips a fixed path it may not search, and passes when none is there.
func TestRefuseV1(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	present := filepath.Join(dir, "controlplane.db")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		beside string
		fixed  []string
		want   string // the refusal's prefix; empty is no refusal
	}{
		{"none there", missing, []string{filepath.Join(dir, "also-missing.db")}, ""},
		{"beside the bootstrap", present, []string{missing}, v1Sentence("a control-plane database", present)},
		{"at a fixed path", missing, []string{missing, present}, v1Sentence("a control-plane database", present)},
		// A file where a directory is expected holds no database beneath it.
		{"under a file, beside and fixed", filepath.Join(present, "controlplane.db"), []string{filepath.Join(present, "controlplane.db")}, ""},
	} {
		err := refuseV1(tc.beside, tc.fixed)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.want)) {
			t.Errorf("%s: refuseV1(%q, %q) = %v, want %q", tc.name, tc.beside, tc.fixed, err, tc.want)
		}
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Log("an unreadable directory is not checked: the test's user reads every directory here")
		return
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	p := filepath.Join(locked, "controlplane.db")
	if err := refuseV1(p, nil); err == nil || !strings.HasPrefix(err.Error(), "refusing to start: checking for v1's control-plane database at "+p) {
		t.Errorf("refuseV1 beside the bootstrap under an unsearchable directory = %v, want the check refused, not passed", err)
	}
	if err := refuseV1(missing, []string{p}); err != nil {
		t.Errorf("refuseV1 at a fixed path under an unsearchable directory = %v, want it skipped", err)
	}
	if err := refuseV1(missing, []string{p, present}); err == nil || err.Error() != v1Sentence("a control-plane database", present) {
		t.Errorf("refuseV1 at a fixed path after an unsearchable one = %v, want %s refused: the skip goes on to the next path", err, present)
	}

	// A path whose parent is a symlink loop cannot be checked for any reason
	// but permission, so the start is refused there, a fixed path included.
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	looped := filepath.Join(loop, "controlplane.db")
	if err := refuseV1(missing, []string{looped}); err == nil || !strings.HasPrefix(err.Error(), "refusing to start: checking for v1's control-plane database at "+looped) {
		t.Errorf("refuseV1 at a fixed path under a symlink loop = %v, want the check refused, not skipped", err)
	}
}

// The command's error once the node stopped: a start that failed for a
// reason but a signal names its step, a signal exits zero once every part
// stopped, and a stop that failed or missed its bound names the stop step.
//
// The table runs the rule in process; TestStartFailingUnderSIGTERMExitsNonZero
// runs a start that failed for its own reason under a signal end to end,
// through a hook that signals the child once Start returned, because no
// timing can land a signal between the steps that check for cancellation and
// one that fails on its own.
func TestAnswer(t *testing.T) {
	startErr := fmt.Errorf("node: start step engine: %w", context.Canceled)
	bindErr := errors.New("node: start step listeners: listen tcp 127.0.0.1:1: bind: address already in use")
	shareErr := errors.New("node: start step engine: share 7: the journal is full")
	stopErr := errors.New("node: stop engine: not stopped within 30s")
	ownAndCanceled := errors.Join(shareErr, context.Canceled)
	bothCanceled := errors.Join(startErr, fmt.Errorf("node: halt: %w", context.Canceled))
	for _, tc := range []struct {
		name      string
		start     error
		signalled bool
		stop      error
		want      string // empty is nil, exit zero
	}{
		{"stopped on a signal", nil, true, nil, ""},
		{"a start the signal ended", startErr, true, nil, ""},
		{"a start that failed", startErr, false, nil, "starting the node: " + startErr.Error()},
		{"a start that failed for its own reason under a signal", bindErr, true, nil, "starting the node: " + bindErr.Error()},
		{"a stop that missed its bound", nil, true, stopErr, "stopping the node: " + stopErr.Error()},
		{"both", startErr, false, stopErr, "starting the node: " + startErr.Error() + "\nstopping the node: " + stopErr.Error()},
		{"an own failure joined with a cancellation under a signal", ownAndCanceled, true, nil, "starting the node: " + ownAndCanceled.Error()},
		{"only cancellations joined under a signal", bothCanceled, true, nil, ""},
	} {
		err := answer(tc.start, endedBySignal(tc.start, tc.signalled), tc.stop)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s: answer = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// dfs start looks for v1's database at the Kubernetes operator's default and
// at the Docker image's state volume, wherever its bootstrap file is.
func TestV1Databases(t *testing.T) {
	want := []string{"/data/controlplane/controlplane.db", "/data/state/controlplane.db"}
	if got := v1Databases(); !slices.Equal(got, want) {
		t.Errorf("v1Databases() = %q, want %q", got, want)
	}
}

// Over an empty store directory, such as a volume mounted late, with a
// device that holds no journal either, dfs start answers the start check's
// not-initialised verdict and leaves the directory empty: a start creates
// no store.
func TestStartRefusesAnEmptyStore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	root := NewRoot()
	root.SetOut(io.Discard)
	root.SetArgs([]string{"start", "--config", bootstrapFile{
		store:   store,
		journal: filepath.Join(dir, "device"),
		node:    nodeUUID,
	}.write(t, dir)})
	err := root.Execute()
	if want := "the start check: the node is not initialised"; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("dfs start over an empty store directory: %v, want an error starting %q", err, want)
	}
	if entries, err := os.ReadDir(store); err != nil || len(entries) != 0 {
		t.Errorf("store directory %s after the refusal: %d entries (%v), want none: no store created", store, len(entries), err)
	}
}

// v1Records are keys of v1's Badger metadata store, written raw, as v1 left
// them, with no format record.
var v1Records = map[string]string{"f:1": "v1 file", "p:x": "v1 parent", "c:y": "v1 child", "lock:z": "v1 lock"}

// writeV1Store writes v1Records into a Badger store at dir.
func writeV1Store(t *testing.T, dir string) {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{Dir: dir, NodeID: testNode})
	if err != nil {
		t.Fatalf("open the v1 store: %v", err)
	}
	err = db.Update(t.Context(), func(tx kv.Txn) error {
		for k, v := range v1Records {
			if err := tx.Set([]byte(k), []byte(v)); err != nil {
				return err
			}
		}
		return nil
	})
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatalf("write the v1 store: %v", err)
	}
}

// checkV1Store fails the test unless the Badger store at dir still holds
// every one of v1Records unchanged.
func checkV1Store(t *testing.T, dir string) {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{Dir: dir, NodeID: testNode})
	if err != nil {
		t.Fatalf("reopen the v1 store: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	err = db.View(t.Context(), func(r kv.Reader) error {
		for k, want := range v1Records {
			got, err := r.Get([]byte(k))
			if err != nil {
				return fmt.Errorf("v1 key %q: %w", k, err)
			}
			if string(got) != want {
				t.Errorf("v1 key %q after the refusal: %q, want %q unchanged", k, got, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Errorf("v1 store after the refusal: %v, want every v1 key still there", err)
	}
}

// Over a v1 Badger store at metadata.badger.path, which the start check
// passes as a present store, dfs start refuses with the v1 sentence naming
// the directory, rather than sending the operator to dfs init, and leaves
// v1's keys as they were.
func TestStartRefusesAV1MetadataStore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	writeV1Store(t, store)
	_, err := runDfs("start", "--config", bootstrapFile{store: store, journal: filepath.Join(dir, "device"),
		node: nodeUUID, key: storageKeyFile(t, dir)}.write(t, dir))
	if want := v1Sentence("a metadata store", store); err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("dfs start over a v1 store: %v, want it refused with %q", err, want)
	}
	checkV1Store(t, store)
}
