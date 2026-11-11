package adaptertest

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/resources"
)

// recorder is a testing.TB whose failures and cleanups are kept rather than
// acted on, so a test can leave something behind, run CheckLeaks' cleanups
// and assert that it failed without failing itself.
type recorder struct {
	testing.TB
	dir      string
	errors   []string
	cleanups []func()
}

// newRecorder returns a recorder whose cleanups also run when t ends, so a
// test that stops early leaves no check active for the next.
func newRecorder(t *testing.T) *recorder {
	r := &recorder{TB: t, dir: t.TempDir()}
	t.Cleanup(r.finish)

	return r
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) Logf(string, ...any) {}

func (r *recorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

func (r *recorder) ArtifactDir() string { return r.dir }

func (r *recorder) Failed() bool { return len(r.errors) > 0 }

func (r *recorder) failure() string { return strings.Join(r.errors, "\n") }

// finish runs the recorded cleanups last-in first-out, as the testing package
// would at the end of the test.
func (r *recorder) finish() {
	for _, f := range slices.Backward(r.cleanups) {
		f()
	}
	r.cleanups = nil
}

// leftBehind blocks until release closes: a goroutine a test forgot to join.
func leftBehind(release <-chan struct{}) { <-release }

// A goroutine still running at the test's end fails CheckLeaks, by name, with
// no budgets to read.
func TestCheckLeaksFailsOnAGoroutineLeftBehind(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rec := newRecorder(t)

	CheckLeaks(rec, nil)
	go leftBehind(release)
	rec.finish()

	if !rec.Failed() {
		t.Fatal("a goroutine still running at the end of the test did not fail CheckLeaks")
	}
	if !strings.Contains(rec.failure(), "leftBehind") {
		t.Errorf("the failure does not name the goroutine's function:\n%s", rec.failure())
	}
}

// A test that leaves nothing behind passes CheckLeaks.
func TestCheckLeaksPassesACleanTest(t *testing.T) {
	rec := newRecorder(t)

	CheckLeaks(rec, nil)
	done := make(chan struct{})
	go func() { close(done) }()
	<-done
	rec.finish()

	if rec.Failed() {
		t.Fatalf("a test that left nothing behind failed CheckLeaks:\n%s", rec.failure())
	}
}

// A Budgets with no budget set holds nothing.
func TestBudgetsHeldSkipsAnUnsetBudget(t *testing.T) {
	if err := budgetsHeld(&resources.Budgets{}); err != nil {
		t.Fatalf("budgetsHeld reports a Budgets with no budget set: %v", err)
	}
}
