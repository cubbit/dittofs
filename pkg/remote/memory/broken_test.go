package memory_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/memory"
	"github.com/cubbit/dittofs/pkg/remote/remotetest"
	"github.com/cubbit/dittofs/pkg/sim"
)

// servesEmpty is a fault around the in-memory store: a range of no bytes
// other than the zero Range is answered with no bytes instead of refused.
type servesEmpty struct{ *memory.Store }

func (s servesEmpty) Get(ctx context.Context, n remote.Name, r remote.Range) (io.ReadCloser, remote.StoredSum, error) {
	if r != (remote.Range{}) && r.Len <= 0 {
		return io.NopCloser(bytes.NewReader(nil)), remote.StoredSum{}, nil
	}

	return s.Store.Get(ctx, n, r)
}

// servesAfterClose is a fault around the in-memory store: Close does not
// close it, so it keeps serving.
type servesAfterClose struct{ *memory.Store }

func (servesAfterClose) Close() error { return nil }

// broken names each fault and the one row it must fail.
var broken = map[string]struct {
	wrap func(*memory.Store) remote.Store
	row  string
}{
	"serves-empty":       {func(s *memory.Store) remote.Store { return servesEmpty{s} }, "GetEmptyRange"},
	"serves-after-close": {func(s *memory.Store) remote.Store { return servesAfterClose{s} }, "AfterClose"},
}

// brokenEnv selects the fault TestBrokenStore runs the suite over, in the
// child process TestTheSuiteFailsABrokenStoreOnItsRow starts.
const brokenEnv = "MEMORY_SUITE_BROKEN"

// TestBrokenStore runs the suite over the fault brokenEnv names, and is
// skipped without it: the failures it reports are the parent test's evidence.
func TestBrokenStore(t *testing.T) {
	b, ok := broken[os.Getenv(brokenEnv)]
	if !ok {
		t.Skip("runs only as the child of TestTheSuiteFailsABrokenStoreOnItsRow")
	}

	remotetest.Run(t, func(t *testing.T) remotetest.Subject {
		return remotetest.Subject{Store: b.wrap(memory.New(sim.NewTestClock(epoch).Now, remote.ChecksumNone))}
	})
}

// failedRow matches a failed row of the suite in verbose test output.
var failedRow = regexp.MustCompile(`(?m)^\s*--- FAIL: TestBrokenStore/(\S+)`)

// The suite fails each broken store on exactly the row that holds the rule it
// breaks, and on no other row.
func TestTheSuiteFailsABrokenStoreOnItsRow(t *testing.T) {
	for fault, b := range broken {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBrokenStore$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), brokenEnv+"="+fault)
		out, _ := cmd.CombinedOutput()

		var rows []string
		for _, m := range failedRow.FindAllStringSubmatch(string(out), -1) {
			rows = append(rows, m[1])
		}

		if !slices.Equal(rows, []string{b.row}) {
			t.Errorf("the suite over a store that %s failed rows %q, want exactly %q; output:\n%s", fault, rows, b.row, out)
		}
	}
}
