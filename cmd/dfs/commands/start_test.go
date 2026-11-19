package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
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
	// The backstop: a child that never logs what the test waits for is
	// killed, which ends its output and so the test's read.
	backstop := time.AfterFunc(60*time.Second, func() { _ = cmd.Process.Kill() })
	t.Cleanup(func() {
		backstop.Stop()
		_ = cmd.Process.Kill()
	})
	return cmd, r
}

// The node's identity as the test's bootstraps write it, and its bytes.
const nodeUUID = "00112233-4455-6677-8899-aabbccddeeff"

var testNode = entity.NodeID{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}

// writeBootstrap writes a bootstrap under dir naming store and the journal
// device dev, every listener on loopback port 0, and returns its path.
func writeBootstrap(t *testing.T, dir, store, dev string) string {
	t.Helper()
	body := fmt.Sprintf(`logging:
  output: stderr
node:
  id: %s
metadata:
  backend: badger
  badger:
    path: '%s'
journals:
  - path: '%s'
api:
  listen: ['127.0.0.1:0']
metrics:
  listen: '127.0.0.1:0'
adapters:
  listen: ['127.0.0.1']
blockstore:
  journal:
    path: '%s'
`, nodeUUID, store, dev, filepath.Join(dir, "blocks"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// initInstallation initialises an installation as dfs init leaves one: the
// metadata store at store, one journal at dev, recorded for the test's node.
func initInstallation(t *testing.T, store, dev string) {
	t.Helper()
	ctx := t.Context()
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
}

// apiAddr matches the record naming the API's bound address.
var apiAddr = regexp.MustCompile(`node: listening api=(\S+)`)

// Over an initialised installation, dfs start answers ready, logging the
// address it bound, and exits zero on SIGTERM.
func TestStartServesUntilSIGTERM(t *testing.T) {
	dir := t.TempDir()
	store, dev := filepath.Join(dir, "store"), filepath.Join(dir, "device")
	initInstallation(t, store, dev)
	cmd, out := startChild(t, writeBootstrap(t, dir, store, dev))

	lines := bufio.NewScanner(out)
	var addr string
	var log strings.Builder
	for addr == "" && lines.Scan() {
		log.WriteString(lines.Text() + "\n")
		if m := apiAddr.FindStringSubmatch(lines.Text()); m != nil {
			addr = m[1]
		}
	}
	if addr == "" {
		t.Fatalf("dfs start logged no API address before it ended (%v):\n%s", lines.Err(), log.String())
	}
	rest := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(out)
		rest <- string(b)
	}()

	// Readiness is polled until it answers ready, the bound below the
	// backstop; Start turns it ready after its first reconcile.
	deadline := time.Now().Add(30 * time.Second)
	for got := ""; got != "ready"; {
		if time.Now().After(deadline) {
			t.Fatalf("/health/ready on %s: %q, want ready within 30s", addr, got)
		}
		got = readiness(t, addr)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	tail := <-rest
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("dfs start exited %d on SIGTERM, want 0:\n%s%s", code, log.String(), tail)
	}
}

func readiness(t *testing.T, addr string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health/ready", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /health/ready on %s: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}

// runChild runs dfs start over config to its end, returning its exit code
// and its output.
func runChild(t *testing.T, config string) (int, string) {
	t.Helper()
	cmd, out := startChild(t, config)
	b, _ := io.ReadAll(out)
	return cmd.ProcessState.ExitCode(), string(b)
}

// Over a directory holding no installation, dfs start exits non-zero naming
// the step that refused.
func TestStartNoInstallation(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "device")
	if err := os.Mkdir(dev, 0o700); err != nil {
		t.Fatal(err)
	}
	code, out := runChild(t, writeBootstrap(t, dir, filepath.Join(dir, "store"), dev))
	if code == 0 || !strings.Contains(out, "Error: building the node: ") {
		t.Fatalf("dfs start over no installation: exit %d, output:\n%s\nwant non-zero naming the node's build", code, out)
	}
}

// Beside v1's controlplane.db, dfs start refuses naming it, before the
// metadata store's directory is created.
func TestStartRefusesV1Database(t *testing.T) {
	dir := t.TempDir()
	store, dev := filepath.Join(dir, "store"), filepath.Join(dir, "device")
	v1 := filepath.Join(dir, "controlplane.db")
	if err := os.WriteFile(v1, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runChild(t, writeBootstrap(t, dir, store, dev))
	if code == 0 || !strings.Contains(out, "refusing to start: "+v1+" is v1's control-plane database") {
		t.Fatalf("dfs start beside %s: exit %d, output:\n%s\nwant non-zero naming it", v1, code, out)
	}
	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("store directory %s after the refusal: %v, want none created", store, err)
	}
}

// A stop that failed or missed its bound is the command's error, naming the
// step, so dfs exits non-zero; one that stopped every part exits zero.
func TestStoppedNamesTheStep(t *testing.T) {
	if err := stopped(nil); err != nil {
		t.Errorf("stopped(nil) = %v, want nil, so dfs exits zero", err)
	}
	stepErr := errors.New("node: stop engine: not stopped within 30s")
	err := stopped(stepErr)
	if err == nil || !errors.Is(err, stepErr) || err.Error() != "stopping the node: node: stop engine: not stopped within 30s" {
		t.Errorf("stopped(%v) = %v, want the stop step named under stopping the node", stepErr, err)
	}
}

// startImports are the packages of this module start.go may import: it
// reads the bootstrap, opens the KV and the registry, and hands them to the
// node, building nothing else.
var startImports = map[string]bool{
	"github.com/cubbit/dittofs/internal/node":            true,
	"github.com/cubbit/dittofs/pkg/bootstrap":            true,
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv": true,
	"github.com/cubbit/dittofs/pkg/metrics":              true,
}

// moduleImports returns the imports of path, of this module, outside
// startImports.
func moduleImports(path string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var bad []string
	for _, s := range f.Imports {
		p, err := strconv.Unquote(s.Path.Value)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(p, "github.com/cubbit/dittofs/") && !startImports[p] {
			bad = append(bad, p)
		}
	}
	return bad, nil
}

// start.go imports, of this module, only the node, the bootstrap, Badger and
// the metrics registry; a planted import of another is reported.
func TestStartImports(t *testing.T) {
	bad, err := moduleImports("start.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Errorf("start.go imports %q, want only %v of this module", bad, startImports)
	}

	planted := filepath.Join(t.TempDir(), "start.go")
	src := "package commands\n\nimport (\n\t\"github.com/cubbit/dittofs/internal/node\"\n\t\"github.com/cubbit/dittofs/pkg/metadata/kvstore\"\n)\n"
	if err := os.WriteFile(planted, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	bad, err = moduleImports(planted)
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/cubbit/dittofs/pkg/metadata/kvstore"; len(bad) != 1 || bad[0] != want {
		t.Errorf("planted start.go: reported %q, want [%s]", bad, want)
	}
}
