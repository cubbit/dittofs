package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/google/uuid"
)

// childEnv, when set, makes TestStartChild run dfs with the arguments it
// holds, one per line, as the dfs binary's main would.
const childEnv = "DFS_START_CHILD_ARGS"

// TestStartChild is the child process the start tests run: dfs itself.
func TestStartChild(t *testing.T) {
	args := os.Getenv(childEnv)
	if args == "" {
		t.Skip("runs only as the start tests' child process")
	}
	root := NewRoot()
	root.SetArgs(strings.Split(args, "\n"))
	if err := root.Execute(); err != nil {
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
// streams, piped back.
func startChild(t *testing.T, config string) (*exec.Cmd, io.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartChild$")
	cmd.Env = append(os.Environ(), childEnv+"=start\n--config\n"+config, "XDG_STATE_HOME="+t.TempDir())
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
func runChild(t *testing.T, config string) (int, string) {
	t.Helper()
	cmd, out := startChild(t, config)
	b, _ := io.ReadAll(out)
	return cmd.ProcessState.ExitCode(), string(b)
}

// The node's identity as the test's bootstraps write it, and its bytes.
const nodeUUID = "00112233-4455-6677-8899-aabbccddeeff"

var testNode = entity.NodeID(uuid.MustParse(nodeUUID))

// bootstrapFile is a bootstrap the tests write: every listener on loopback
// port 0, logging to stderr. An empty field is left out of the file.
type bootstrapFile struct {
	backend, store, journal, node string
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
	fmt.Fprintf(&body, `api:
  listen: ['127.0.0.1:0']
metrics:
  listen: '127.0.0.1:0'
adapters:
  listen: ['127.0.0.1']
blockstore:
  journal:
    path: '%s'
`, filepath.Join(dir, "blocks"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// installed is the bootstrap of an installation under dir, as dfs init
// leaves one: the metadata store at dir/store, one journal at dir/device,
// recorded for the test's node. A host whose mount the journal's platform
// check refuses skips the test.
func installed(t *testing.T, dir string) string {
	t.Helper()
	ctx := t.Context()
	scratch := filepath.Join(dir, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	// The child opens its journal under the platform's own check, which a
	// test cannot hand a probe, so the check runs here first, over the same
	// mount.
	if _, err := journal.Init(ctx, scratch, [16]byte{1}); errors.Is(err, journal.ErrUnsuitableStorage) {
		t.Skipf("the journal's platform check refuses the mount holding %s (%v); set TMPDIR to a directory on a suitable mount", dir, err)
	} else if err != nil {
		t.Fatalf("journal init on the scratch directory: %v", err)
	}

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
	return bootstrapFile{store: store, journal: dev, node: nodeUUID}.write(t, dir)
}

// Over a directory holding no installation, dfs start exits non-zero naming
// the step that refused, and creates no store directory.
func TestStartNoInstallation(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	code, out := runChild(t, bootstrapFile{store: store, journal: filepath.Join(dir, "device"), node: nodeUUID}.write(t, dir))
	if code == 0 || !strings.Contains(out, "Error: opening the metadata store: metadata.badger.path: ") {
		t.Fatalf("dfs start over no installation: exit %d, output:\n%s\nwant non-zero naming the metadata store's step", code, out)
	}
	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("store directory %s after the refusal: %v, want none created", store, err)
	}
}

// Beside v1's controlplane.db, dfs start refuses naming it, and opens no
// store.
func TestStartRefusesV1Database(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	v1 := filepath.Join(dir, "controlplane.db")
	if err := os.WriteFile(v1, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runChild(t, bootstrapFile{store: store, journal: filepath.Join(dir, "device"), node: nodeUUID}.write(t, dir))
	if code == 0 || !strings.Contains(out, "refusing to start: "+v1+" is v1's control-plane database") {
		t.Fatalf("dfs start beside %s: exit %d, output:\n%s\nwant non-zero naming it", v1, code, out)
	}
	if entries, err := os.ReadDir(store); err != nil || len(entries) != 0 {
		t.Errorf("store directory %s after the refusal: %d entries (%v), want none: nothing opened", store, len(entries), err)
	}
}

// Each bootstrap the store's opening refuses fails dfs start in process,
// naming the step and the key, before anything is opened.
func TestStartRefusesTheStore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, tc := range []struct {
		name string
		b    func(dir string) bootstrapFile
		want string
	}{
		{"backend not badger", func(dir string) bootstrapFile {
			return bootstrapFile{backend: "foundationdb", store: dir, node: nodeUUID}
		}, `metadata.backend "foundationdb" is not one this build opens`},
		{"no store path", func(string) bootstrapFile {
			return bootstrapFile{node: nodeUUID}
		}, "metadata.badger.path is not set"},
		{"no node", func(dir string) bootstrapFile {
			return bootstrapFile{store: dir}
		}, "node.id is not set"},
		{"no store directory", func(dir string) bootstrapFile {
			return bootstrapFile{store: filepath.Join(dir, "missing"), node: nodeUUID}
		}, "metadata.badger.path: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			root := NewRoot()
			root.SetOut(io.Discard)
			root.SetArgs([]string{"start", "--config", tc.b(dir).write(t, dir)})
			err := root.Execute()
			if want := "opening the metadata store: " + tc.want; err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("dfs start: %v, want an error starting %q", err, want)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Errorf("%s after the refusal holds %d entries, want only the bootstrap: nothing opened", dir, len(entries))
			}
		})
	}
}

// refuseV1 refuses a database at any of its paths, naming it, and a path it
// cannot check, and passes when none is there.
func TestRefuseV1(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	present := filepath.Join(dir, "controlplane.db")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		paths []string
		want  string // the refusal's prefix; empty is no refusal
	}{
		{"none there", []string{missing, filepath.Join(dir, "also-missing.db")}, ""},
		{"the first there", []string{present, missing}, "refusing to start: " + present + " is v1's"},
		{"the second there", []string{missing, present}, "refusing to start: " + present + " is v1's"},
	} {
		err := refuseV1(tc.paths...)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.want)) {
			t.Errorf("%s: refuseV1(%q) = %v, want %q", tc.name, tc.paths, err, tc.want)
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
	if err := refuseV1(p); err == nil || !strings.HasPrefix(err.Error(), "refusing to start: checking for v1's control-plane database at "+p) {
		t.Errorf("refuseV1 under an unreadable directory = %v, want the check refused, not passed", err)
	}
}

// The command's error once the node stopped: a start that failed for a
// reason but a signal names its step, a signal exits zero once every part
// stopped, and a stop that failed or missed its bound names the stop step.
func TestAnswer(t *testing.T) {
	startErr := errors.New("node: start step engine: context canceled")
	stopErr := errors.New("node: stop engine: not stopped within 30s")
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
		{"a stop that missed its bound", nil, true, stopErr, "stopping the node: " + stopErr.Error()},
		{"both", startErr, false, stopErr, "starting the node: " + startErr.Error() + "\nstopping the node: " + stopErr.Error()},
	} {
		err := answer(tc.start, tc.signalled, tc.stop)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s: answer = %q, want %q", tc.name, got, tc.want)
		}
	}
}
