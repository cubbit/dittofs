package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/cubbit/dittofs/cmd/dfsctl/internal/clisuite"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Every row of the CLI suite passes from a dfsctl binary built from the
// module and run as a child process, against a node this test starts: the
// child shares no memory with the node and reaches it only through its API.
// Its environment holds only an empty home, where the default bootstrap file
// is unreadable, so a command that loads the bootstrap fails; one that opened
// a store would find none of the node's records, which is in this process's
// memory. The rows that write through the store or advance the node's clock
// run too, the test doing that part. Each noun runs over a node of its own,
// as its package runs it: a node shared by every noun would run ahead of
// this machine's clock, which the child reads, by every wait the rows before
// advanced it through. Each node starts ten minutes behind that clock, and
// child moves it up before every command, so the row "token create signs a
// context in through login --token-file until token revoke" fails if that
// catch-up breaks: its token's expiry, which the child reckons on this
// machine's clock, must land within a minute of an hour after its creation,
// which the node stamps at its store's time.
func TestSuiteFromAChildProcess(t *testing.T) {
	prof.Watch(t, prof.Options{})
	bin := filepath.Join(t.TempDir(), "dfsctl")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build dfsctl: %v\n%s", err, out)
	}
	for _, noun := range clisuite.Nouns() {
		t.Run(noun, func(t *testing.T) {
			s := clisuite.Start(t, nodetest.StartAt(time.Now().Add(-10*time.Minute)))
			clisuite.Run(t, func(t *testing.T) clisuite.Runner { return child(t, bin, s.Harness) }, s, noun)
		})
	}
}

// child runs bin with each invocation's arguments, its home an empty one of
// t's, shared by its invocations so they share one credentials file, after
// catchUp moves h's node up to this machine's clock. Ending ctx kills the
// child.
func child(t *testing.T, bin string, h *nodetest.Harness) clisuite.Runner {
	home := t.TempDir()
	// The bootstrap's default path, unreadable, so a read of it refuses
	// rather than loading the defaults a missing file loads.
	poison := filepath.Join(home, ".config", "dittofs", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(poison), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poison, []byte("not: [a bootstrap\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home}
	return func(ctx context.Context, args ...string) (string, string, int) {
		if err := catchUp(ctx, h); err != nil {
			return "", "clisuite child: " + err.Error(), -1
		}
		var out, errOut bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env, cmd.Dir = env, home
		cmd.Stdout, cmd.Stderr = &out, &errOut
		// Not t.Errorf: a row's goroutine can outlive its test.
		if err := cmd.Run(); err != nil && !errors.As(err, new(*exec.ExitError)) {
			return out.String(), errOut.String() + "clisuite child: " + err.Error(), -1
		}
		return out.String(), errOut.String(), cmd.ProcessState.ExitCode()
	}
}

// catchUp moves h's clock up to this machine's if it fell behind, then waits
// until the store's time has followed it, as a write's time reads it. Store
// time trails a moved clock until the store next raises its ceiling, about
// once a second of real time, so a record stamped right after the move would
// still carry the old time.
func catchUp(ctx context.Context, h *nodetest.Harness) error {
	clock := h.Clock()
	if behind := time.Since(clock.Now()); behind > 0 {
		clock.Advance(behind)
	}
	want := clock.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		now, err := storeNow(ctx, h)
		if err != nil {
			return err
		}
		if !now.Before(want) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the store's time %v has not reached the clock's %v: %w", now, want, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// storeNow is h's store time, as a write reads it.
func storeNow(ctx context.Context, h *nodetest.Harness) (time.Time, error) {
	var now time.Time
	err := h.Store().ControlPlane().Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		now = tx.Now()
		return nil
	})
	if err != nil {
		return now, fmt.Errorf("read the store's time: %w", err)
	}
	return now, nil
}

// A node ten minutes behind, once caught up, stamps a write at this
// machine's time, not at the store time it had before the move.
func TestCatchUpBringsTheStoreTimeAlong(t *testing.T) {
	h := nodetest.New(t, nodetest.InMemory(), nodetest.StartAt(time.Now().Add(-10*time.Minute)))
	before := time.Now()
	if err := catchUp(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	now, err := storeNow(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if now.Before(before) {
		t.Errorf("store time after catchUp: got %v, want at least this machine's %v", now, before)
	}
}
