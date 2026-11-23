//go:build unix

package backend

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// TestDittofsArgv_InitAndStartTheBuiltDfs guards the two command lines the
// setup hands the server, which the files test above does not reach: a
// renamed admin-password flag of dfs init, or dfs start losing its
// passphrase, fails here instead of at every managed cell. It builds dfs,
// runs the setup's exact init argv over the files writeDittofsInit wrote,
// then the exact start argv, each through env(1) as the setup runs them, and
// waits for the started server to accept on the two listen addresses the
// test adds to the configuration, ports it picks so nothing binds a fixed
// one. A host without a go toolchain, or one whose mount the journal's
// platform check refuses, skips.
func TestDittofsArgv_InitAndStartTheBuiltDfs(t *testing.T) {
	goBin, err := osexec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH; building dfs for the command lines needs one")
	}
	dfsBin := filepath.Join(t.TempDir(), "dfs")
	if out, err := osexec.Command(goBin, "build", "-o", dfsBin,
		"github.com/cubbit/dittofs/cmd/dfs").CombinedOutput(); err != nil {
		t.Fatalf("go build -o %s github.com/cubbit/dittofs/cmd/dfs: %v\n%s", dfsBin, err, out)
	}

	home, data := t.TempDir(), t.TempDir()
	state := filepath.Join(home, ".local", "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeDir, err := os.MkdirTemp("", "dittofs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	for _, dir := range []string{"meta", "blocks"} {
		if err := os.MkdirAll(filepath.Join(data, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The child initialises its journal under the platform's own check,
	// which a test cannot hand a probe, so the check runs here first, over
	// the same mount.
	scratch := filepath.Join(data, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Init(t.Context(), scratch, [16]byte{1}); errors.Is(err, journal.ErrUnsuitableStorage) {
		t.Skipf("the journal's platform check refuses the mount holding %s (%v); set TMPDIR to a directory on a suitable mount", data, err)
	} else if err != nil {
		t.Fatalf("journal init on the scratch directory: %v", err)
	}

	configPath, passwordFile, err := writeDittofsInit(filepath.Join(home, ".config", "dittofs"), data)
	if err != nil {
		t.Fatalf("writeDittofsInit: %v", err)
	}

	// The child inherits no DITTOFS_ variable from the host and no directory
	// of the host's own: the environment the setup passes rides in the argv
	// itself, through env(1), the way the setup runs it.
	childEnv := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "STATE_DIRECTORY", "RUNTIME_DIRECTORY":
			return true
		}
		return strings.HasPrefix(k, "DITTOFS_")
	})
	childEnv = append(childEnv,
		"HOME="+home,
		"XDG_STATE_HOME="+state,
		"XDG_RUNTIME_DIR="+runtimeDir,
	)

	// dfs init runs the setup's whole command line over the files just
	// written.
	initCmd := osexec.Command("env", dittofsInitArgs(dfsBin, configPath, passwordFile)...)
	initCmd.Env = childEnv
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("the setup's dfs init argv: %v\n%s", err, out)
	}

	// The listen keys the configuration template does not name are the
	// test's own: ports picked, not fixed, the same spellings the dfs start
	// tests use. No argv the setup runs changes with them.
	api, metrics := freeLocalAddr(t), freeLocalAddr(t)
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath,
		fmt.Appendf(body, "api:\n  listen: ['%s']\nmetrics:\n  listen: '%s'\n", api, metrics),
		0o600); err != nil {
		t.Fatal(err)
	}

	var serverOut bytes.Buffer
	startCmd := osexec.Command("env", dittofsStartArgs(dfsBin, configPath)...)
	startCmd.Env = childEnv
	startCmd.Stdout, startCmd.Stderr = &serverOut, &serverOut
	if err := startCmd.Start(); err != nil {
		t.Fatalf("start the built dfs: %v", err)
	}
	// One goroutine waits on the process, so a server that exits at startup
	// fails the test at once with its output instead of after the full bound.
	exited := make(chan error, 1)
	go func() { exited <- startCmd.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = startCmd.Process.Kill()
		_ = <-exited
	}
	t.Cleanup(stop)
	for _, addr := range []string{api, metrics} {
		if err := waitAccept(addr, 60*time.Second, exited); err != nil {
			stop()
			t.Fatalf("the setup's dfs start argv: the server did not accept on %s: %v\nserver output:\n%s",
				addr, err, serverOut.String())
		}
	}
	stop()
}

// freeLocalAddr returns a loopback address the kernel picked, taken from a
// listener closed at once so the server may bind it. A port stolen between
// the close and the bind shows up as the server's own bind failure, which
// the test reports with its output.
func freeLocalAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// waitAccept polls the server's own listener until the address accepts a
// connection, the process exits (its error, or nil for a clean exit, comes
// back on exited) or the bound passes: the server coming up is the event,
// and the short pause between attempts only keeps the loop quiet, the same
// wait as the setup's port probe. A receive here consumes the exit, so
// stop's own receive would block: the exit is put back on the channel.
func waitAccept(addr string, bound time.Duration, exited chan error) error {
	deadline := time.Now().Add(bound)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			return conn.Close()
		}
		select {
		case werr := <-exited:
			exited <- werr
			return fmt.Errorf("dfs exited before accepting: %v", werr)
		default:
		}
		if !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}
