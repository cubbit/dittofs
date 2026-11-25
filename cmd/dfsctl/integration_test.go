package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cubbit/dittofs/cmd/dfsctl/internal/clisuite"
	"github.com/cubbit/dittofs/internal/node/nodetest"
)

// Every row of the CLI suite passes from a dfsctl binary built from the
// module and run as a child process, against a node this test starts: the
// child shares no memory with the node, and its environment holds only an
// empty home and empty XDG directories, so a command that reaches the node
// any way but its API, through the bootstrap or the store, fails its row.
// The node is in this process, so the rows that write through the store or
// advance the node's clock run too, the test doing that part. Each noun runs
// over a node of its own, as its package runs it, its clock started at this
// machine's, which the child reads: a node shared by every noun would run
// ahead of it by every wait the rows before advanced it through.
func TestSuiteFromAChildProcess(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "dfsctl")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build dfsctl: %v\n%s", err, out)
	}
	for _, noun := range clisuite.Nouns() {
		t.Run(noun, func(t *testing.T) {
			s := clisuite.Start(t, nodetest.StartAt(time.Now()))
			clisuite.Run(t, func(t *testing.T) clisuite.Runner { return child(t, bin) }, s, noun)
		})
	}
}

// child runs bin with each invocation's arguments, its home and XDG
// directories empty ones of t's, shared by its invocations so they share one
// credentials file. Ending ctx interrupts the child, as a user's interrupt
// ends a dfsctl process's command.
func child(t *testing.T, bin string) clisuite.Runner {
	home := t.TempDir()
	env := []string{"HOME=" + home}
	for _, xdg := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		dir := filepath.Join(home, xdg)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		env = append(env, xdg+"="+dir)
	}
	return func(ctx context.Context, args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env, cmd.Dir = env, home
		cmd.Stdout, cmd.Stderr = &out, &errOut
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		// A child that outlives its interrupt this long is killed.
		cmd.WaitDelay = time.Minute
		if err := cmd.Run(); err != nil && !errors.As(err, new(*exec.ExitError)) {
			t.Errorf("dfsctl %q: %v", args, err)
			return out.String(), errOut.String(), -1
		}
		return out.String(), errOut.String(), cmd.ProcessState.ExitCode()
	}
}
