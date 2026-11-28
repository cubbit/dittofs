package memory_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
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

// failsAbsentDelete is a fault around the in-memory store: a delete of an
// absent name is ErrNotFound instead of nil.
type failsAbsentDelete struct{ *memory.Store }

func (s failsAbsentDelete) Delete(ctx context.Context, names []remote.Name) []error {
	errs := make([]error, len(names))

	for i, n := range names {
		r, _, err := s.Get(ctx, n, remote.Range{})
		if err == nil {
			_ = r.Close()
		} else if errors.Is(err, remote.ErrNotFound) {
			errs[i] = err
			continue
		}

		errs[i] = s.Store.Delete(ctx, names[i:i+1])[0]
	}

	return errs
}

// deletesOnePage is a fault around the in-memory store: it deletes only the
// first 1,000 names of a batch, as one service request would, and reports nil
// for the rest.
type deletesOnePage struct{ *memory.Store }

func (s deletesOnePage) Delete(ctx context.Context, names []remote.Name) []error {
	errs := make([]error, len(names))
	copy(errs, s.Store.Delete(ctx, names[:min(len(names), 1000)]))

	return errs
}

// instanceCursor is a fault around the in-memory store: its cursor is
// tagged with the store instance that issued it, which alone can read it, so
// a walk resumed on a store opened again fails.
type instanceCursor struct{ *memory.Store }

func (s instanceCursor) List(ctx context.Context, after remote.Cursor, n int) ([]remote.Info, remote.Cursor, bool, error) {
	tag := fmt.Sprintf("%p/", s.Store)

	real, ok := strings.CutPrefix(string(after), tag)
	if after != "" && !ok {
		return nil, after, false, fmt.Errorf("unknown cursor %q: %w", after, remote.ErrInvalid)
	}

	page, next, done, err := s.Store.List(ctx, remote.Cursor(real), n)

	return page, remote.Cursor(tag + string(next)), done, err
}

// yieldsForeign is a fault around the in-memory store: a key planted under
// blocks/ is listed as a block, its hex read into a name.
type yieldsForeign struct {
	*memory.Store

	mu      sync.Mutex
	planted []string
}

func (s *yieldsForeign) plant(key string) {
	s.mu.Lock()
	s.planted = append(s.planted, key)
	s.mu.Unlock()
}

func (s *yieldsForeign) List(ctx context.Context, after remote.Cursor, n int) ([]remote.Info, remote.Cursor, bool, error) {
	page, next, done, err := s.Store.List(ctx, after, n)
	if err != nil {
		return page, next, done, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, k := range s.planted {
		if k > string(after) && k <= string(next) {
			var name remote.Name
			_, _ = hex.Decode(name[:], []byte(strings.TrimPrefix(k, "blocks/")))
			page = append(page, remote.Info{Name: name})
		}
	}

	return page, next, done, nil
}

// ctxAtEntry is a fault around the in-memory store: List checks its context
// once, at entry, and lists the page under a context that never ends.
type ctxAtEntry struct{ *memory.Store }

func (s ctxAtEntry) List(ctx context.Context, after remote.Cursor, n int) ([]remote.Info, remote.Cursor, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, after, false, err
	}

	return s.Store.List(context.WithoutCancel(ctx), after, n)
}

// acceptsForeignCursor is a fault around the in-memory store: a cursor
// outside blocks/ starts the walk at the beginning instead of being refused.
type acceptsForeignCursor struct{ *memory.Store }

func (s acceptsForeignCursor) List(ctx context.Context, after remote.Cursor, n int) ([]remote.Info, remote.Cursor, bool, error) {
	if !strings.HasPrefix(string(after), "blocks/") {
		after = ""
	}

	return s.Store.List(ctx, after, n)
}

// broken names each fault and the one row it must fail.
var broken = map[string]struct {
	wrap func(*memory.Store) remote.Store
	row  string
}{
	"serves-empty":           {func(s *memory.Store) remote.Store { return servesEmpty{s} }, "GetEmptyRange"},
	"serves-after-close":     {func(s *memory.Store) remote.Store { return servesAfterClose{s} }, "AfterClose"},
	"fails-absent-delete":    {func(s *memory.Store) remote.Store { return failsAbsentDelete{s} }, "DeleteTwice"},
	"deletes-one-page":       {func(s *memory.Store) remote.Store { return deletesOnePage{s} }, "DeleteBatch"},
	"instance-cursor":        {func(s *memory.Store) remote.Store { return instanceCursor{s} }, "ListWalk"},
	"accepts-foreign-cursor": {func(s *memory.Store) remote.Store { return acceptsForeignCursor{s} }, "ListForeignCursor"},
	"yields-foreign":         {func(s *memory.Store) remote.Store { return &yieldsForeign{Store: s} }, "ListForeignKey"},
	"ctx-at-entry":           {func(s *memory.Store) remote.Store { return ctxAtEntry{s} }, "ListCancel"},
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
		inner := memory.New(sim.NewTestClock(epoch).Now, remote.ChecksumNone)

		sub := subject(inner)
		sub.Store = b.wrap(inner)
		sub.Reopen = func(context.Context) (remote.Store, error) { return b.wrap(memory.Reopen(inner)), nil }

		if p, ok := sub.Store.(interface{ plant(string) }); ok {
			putRaw := sub.PutRaw
			sub.PutRaw = func(ctx context.Context, key string, body []byte) error {
				p.plant(key)
				return putRaw(ctx, key, body)
			}
		}

		return sub
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
