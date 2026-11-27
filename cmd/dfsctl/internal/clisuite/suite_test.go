package clisuite_test

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/cmd/dfsctl/commands"
	"github.com/cubbit/dittofs/cmd/dfsctl/internal/clisuite"
)

func inProcess(t *testing.T) clisuite.Runner {
	return clisuite.InProcess(cmdutil.Env{ConfigPath: filepath.Join(t.TempDir(), "dfsctl", "config.json")})
}

// An in-process invocation answers its own stdout, stderr and exit status,
// and invocations overlapping on one runner each answer only their own.
func TestInProcessAnswersEachInvocationsOwn(t *testing.T) {
	run := inProcess(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			out, errOut, status := run(t.Context(), "version", "--short")
			if status != 0 || out != commands.Version+"\n" || errOut != "" {
				t.Errorf("dfsctl version --short: status %d, stdout %q, stderr %q, want 0, %q and nothing", status, out, errOut, commands.Version+"\n")
			}
		}()
		go func() {
			defer wg.Done()
			out, errOut, status := run(t.Context(), "bogus")
			if status != 1 || out != "" || !strings.HasPrefix(errOut, `Error: unknown command "bogus"`) {
				t.Errorf("dfsctl bogus: status %d, stdout %q, stderr %q, want 1, nothing and the unknown command", status, out, errOut)
			}
		}()
	}
	wg.Wait()
}

// Login signs in only the runner it is given: another runner, over a
// credentials file of its own, is still signed out.
func TestLoginSignsInOnlyItsRunner(t *testing.T) {
	s := clisuite.Start(t)
	in, out := inProcess(t), inProcess(t)
	s.Login(t, in)
	if _, errOut, status := in(t.Context(), "operation", "list"); status != 0 {
		t.Errorf("operation list signed in: status %d, stderr %q, want 0", status, errOut)
	}
	if _, errOut, status := out(t.Context(), "operation", "list"); status == 0 || !strings.Contains(errOut, "not logged in") {
		t.Errorf("operation list through another runner: status %d, stderr %q, want it refused as not logged in", status, errOut)
	}
}

// Await advances a harness's clock until what it waits for happens, here a
// wait on that clock that nothing else advances.
func TestAwaitAdvancesTheHarnessClock(t *testing.T) {
	s := clisuite.Start(t)
	done := make(chan struct{})
	go func() {
		<-s.Harness.Clock().After(5 * time.Second)
		close(done)
	}()
	clisuite.Await(t, s, "a wait of five seconds on the node's clock", done)
}
