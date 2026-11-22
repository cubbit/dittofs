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

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
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

// armed reports whether the store holds a committed row under h.arm, read
// past the fault, so the call being consulted counts only once it does.
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartChild$")
	host := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "DITTOFS_") })
	args := "start"
	if config != "" {
		args += "\n--config\n" + config
	}
	cmd.Env = append(host, childEnv+"="+args, "XDG_STATE_HOME="+t.TempDir(),
		"DITTOFS_ENCRYPTION_PASSPHRASE="+storagePassphrase)
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
blockstore:
  journal:
    path: '%s'
`, cmp.Or(b.api, "127.0.0.1:0"), filepath.Join(dir, "blocks"))
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
// deployment's directory.
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
			if code == 0 || !strings.Contains(out, "Error: refusing to start: "+v1+" is v1's control-plane database") {
				t.Fatalf("dfs start beside %s over %s: exit %d, output:\n%s\nwant non-zero naming it", v1, tc.name, code, out)
			}
			if entries, err := os.ReadDir(store); err != nil || len(entries) != 0 {
				t.Errorf("store directory %s after the refusal: %d entries (%v), want none: nothing opened", store, len(entries), err)
			}
			if strings.Contains(out, "dfs init") {
				t.Errorf("dfs start beside v1's database: the refusal advises running dfs init (%s), which over a v1 deployment's directory is the advice that destroys data", out)
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
	}{
		{"backend not badger", func(dir string) bootstrapFile {
			return bootstrapFile{
				backend: "foundationdb",
				journal: filepath.Join(dir, "device"),
				node:    nodeUUID,
			}
		}, `metadata.backend "foundationdb" is not one this build opens; it opens badger`},
		{"no store path", func(dir string) bootstrapFile {
			return bootstrapFile{journal: filepath.Join(dir, "device"), node: nodeUUID}
		}, "metadata.badger.path is not set"},
		{"no node", func(dir string) bootstrapFile {
			return bootstrapFile{store: filepath.Join(dir, "store"), journal: filepath.Join(dir, "device")}
		}, "node.id is not set"},
		{"no store directory", func(dir string) bootstrapFile {
			return bootstrapFile{store: filepath.Join(dir, "missing"),
				journal: filepath.Join(dir, "device"), node: nodeUUID}
		}, "the start check: the node is not initialised"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
		{"beside the bootstrap", present, []string{missing}, "refusing to start: " + present + " is v1's"},
		{"at a fixed path", missing, []string{missing, present}, "refusing to start: " + present + " is v1's"},
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
	if err := refuseV1(missing, []string{p, present}); err == nil || !strings.HasPrefix(err.Error(), "refusing to start: "+present+" is v1's") {
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
	root.SetArgs([]string{"start", "--config", bootstrapFile{store: store,
		journal: filepath.Join(dir, "device"), node: nodeUUID}.write(t, dir)})
	err := root.Execute()
	if want := "the start check: the node is not initialised"; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("dfs start over an empty store directory: %v, want an error starting %q", err, want)
	}
	if entries, err := os.ReadDir(store); err != nil || len(entries) != 0 {
		t.Errorf("store directory %s after the refusal: %d entries (%v), want none: no store created", store, len(entries), err)
	}
}
