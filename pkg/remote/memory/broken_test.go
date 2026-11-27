package memory_test

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"sync/atomic"
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

// appendsControl is a fault around the in-memory store: a control put appends
// its body to the object already there instead of replacing it.
type appendsControl struct{ *memory.Store }

func (s appendsControl) PutControl(ctx context.Context, r remote.Role, body []byte) error {
	old, _, err := s.GetControl(ctx, r)
	if err != nil && !errors.Is(err, remote.ErrNotFound) {
		return err
	}

	return s.Store.PutControl(ctx, r, append(old, body...))
}

// inventsHealth is a fault around the in-memory store, opened without its
// health object: a get of the absent health object answers an empty body
// instead of ErrNotFound.
type inventsHealth struct{ *memory.Store }

func (s inventsHealth) GetControl(ctx context.Context, r remote.Role) ([]byte, remote.Version, error) {
	body, v, err := s.Store.GetControl(ctx, r)
	if r == remote.RoleHealth && errors.Is(err, remote.ErrNotFound) {
		return []byte{}, "", nil
	}

	return body, v, err
}

// absentControlEmpty is a fault around the in-memory store: a get of any
// absent control object answers an empty body instead of ErrNotFound.
type absentControlEmpty struct{ *memory.Store }

func (s absentControlEmpty) GetControl(ctx context.Context, r remote.Role) ([]byte, remote.Version, error) {
	body, v, err := s.Store.GetControl(ctx, r)
	if errors.Is(err, remote.ErrNotFound) {
		return []byte{}, "", nil
	}

	return body, v, err
}

// controlInProcess is a fault around the in-memory store: control objects it
// is given live in its own memory, never in the namespace, so they read back
// through it alone.
type controlInProcess struct {
	*memory.Store

	mu      sync.Mutex
	objects map[remote.Role][]byte
}

func (s *controlInProcess) PutControl(ctx context.Context, r remote.Role, body []byte) error {
	// The store's own get refuses what a closed store refuses.
	if _, _, err := s.Store.GetControl(ctx, remote.RoleHealth); err != nil {
		return err
	}

	s.mu.Lock()
	s.objects[r] = bytes.Clone(body)
	s.mu.Unlock()

	return nil
}

func (s *controlInProcess) GetControl(ctx context.Context, r remote.Role) ([]byte, remote.Version, error) {
	s.mu.Lock()
	body, ok := s.objects[r]
	s.mu.Unlock()

	if ok {
		if _, _, err := s.Store.GetControl(ctx, remote.RoleHealth); err != nil {
			return nil, "", err
		}

		return bytes.Clone(body), "", nil
	}

	return s.Store.GetControl(ctx, r)
}

// controlAsBlock is a fault around the in-memory store: a control put also
// leaves a copy under a key that spells a block name, which a listing yields.
type controlAsBlock struct{ *memory.Store }

func (s controlAsBlock) PutControl(ctx context.Context, r remote.Role, body []byte) error {
	if err := s.Store.PutControl(ctx, r, body); err != nil {
		return err
	}

	n := sha256.Sum256([]byte("control/" + r.String()))
	memory.PutRaw(s.Store, "blocks/"+hex.EncodeToString(n[:]), body)

	return nil
}

// storesRefusedControl is a fault around the in-memory store: a control put
// the store refuses still leaves its body at the role's key.
type storesRefusedControl struct{ *memory.Store }

func (s storesRefusedControl) PutControl(ctx context.Context, r remote.Role, body []byte) error {
	err := s.Store.PutControl(ctx, r, body)
	if err != nil {
		memory.PutRaw(s.Store, "control/"+r.String(), body)
	}

	return err
}

// controlAfterClose is a fault around the in-memory store: its control calls
// go to a second store over the same objects, which Close leaves open.
type controlAfterClose struct {
	*memory.Store

	control *memory.Store
}

func (s controlAfterClose) PutControl(ctx context.Context, r remote.Role, body []byte) error {
	return s.control.PutControl(ctx, r, body)
}

func (s controlAfterClose) GetControl(ctx context.Context, r remote.Role) ([]byte, remote.Version, error) {
	return s.control.GetControl(ctx, r)
}

// controlUnderBlocks is a fault around the in-memory store: a control put also
// leaves a copy at blocks/control/<role>, a key a listing skips.
type controlUnderBlocks struct{ *memory.Store }

func (s controlUnderBlocks) PutControl(ctx context.Context, r remote.Role, body []byte) error {
	if err := s.Store.PutControl(ctx, r, body); err != nil {
		return err
	}

	memory.PutRaw(s.Store, "blocks/control/"+r.String(), body)

	return nil
}

// storesRefusedPut is a fault around the in-memory store: a put the store
// refuses still leaves its body under the block's key.
type storesRefusedPut struct{ *memory.Store }

func (s storesRefusedPut) Put(ctx context.Context, n remote.Name, body io.Reader, size int64, sum []byte) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	if err := s.Store.Put(ctx, n, bytes.NewReader(b), size, sum); err != nil {
		memory.PutRaw(s.Store, "blocks/"+hex.EncodeToString(n[:]), b)
		return err
	}

	return nil
}

// healthAfterClose is a fault around the in-memory store: its probes go to a
// second store over the same objects, which Close leaves open.
type healthAfterClose struct {
	*memory.Store

	health *memory.Store
}

func (s healthAfterClose) Health(ctx context.Context, d remote.Direction) error {
	return s.health.Health(ctx, d)
}

// probeThrough is a fault around the wrapper: its put probe goes to the store
// beneath it, so it passes while the namespace is read-only.
type probeThrough struct {
	*memory.Wrapper

	s *memory.Store
}

func (s probeThrough) Health(ctx context.Context, d remote.Direction) error {
	if d == remote.DirPut {
		return s.s.Health(ctx, d)
	}

	return s.Wrapper.Health(ctx, d)
}

// readOnlyFault is a fault around the wrapper that keeps its own copy of the
// read-only setting: refuseGet refuses a get probe that passed while
// read-only, and
// denyFirst refuses a put with access denied before the closed store's own
// refusal.
type readOnlyFault struct {
	*memory.Wrapper

	on                   atomic.Bool
	refuseGet, denyFirst bool
}

func (s *readOnlyFault) Health(ctx context.Context, d remote.Direction) error {
	err := s.Wrapper.Health(ctx, d)
	if err == nil && s.refuseGet && d == remote.DirGet && s.on.Load() {
		return &remote.DeniedError{Cause: remote.CauseAccess, Code: "AccessDenied"}
	}

	return err
}

func (s *readOnlyFault) Put(ctx context.Context, n remote.Name, body io.Reader, size int64, sum []byte) error {
	if s.denyFirst && s.on.Load() {
		return &remote.DeniedError{Cause: remote.CauseAccess, Code: "AccessDenied"}
	}

	return s.Wrapper.Put(ctx, n, body, size, sum)
}

// withReadOnlyFault returns the in-memory subject over s with its store
// behind f, which follows the subject's read-only setting.
func withReadOnlyFault(s *memory.Store, f *readOnlyFault) remotetest.Subject {
	sub := subject(s)
	f.Wrapper = sub.Store.(*memory.Wrapper)
	sub.Store = f

	ro := sub.ReadOnly
	sub.ReadOnly = func(ctx context.Context, on bool) error {
		f.on.Store(on)
		return ro(ctx, on)
	}

	return sub
}

// readOnlyBroken names each fault of the read-only namespace, the one row it
// must fail, and a piece of the failure that row reports. Each builds the
// whole subject, since a fault there may sit in a hook rather than the store.
var readOnlyBroken = map[string]struct {
	sub func(*memory.Store) remotetest.Subject
	row string
	msg string
}{
	"probe-through-read-only": {func(s *memory.Store) remotetest.Subject {
		sub := subject(s)
		sub.Store = probeThrough{sub.Store.(*memory.Wrapper), s}

		return sub
	}, "ReadOnly", "Health(DirPut) while read-only: error <nil>"},
	"read-only-latches": {func(s *memory.Store) remotetest.Subject {
		sub := subject(s)
		ro := sub.ReadOnly
		sub.ReadOnly = func(ctx context.Context, on bool) error {
			if !on {
				return nil
			}

			return ro(ctx, on)
		}

		return sub
	}, "ReadOnly", "Health(DirPut) after read-only ended: remote: denied"},
	"read-only-refuses-reads": {func(s *memory.Store) remotetest.Subject {
		return withReadOnlyFault(s, &readOnlyFault{refuseGet: true})
	}, "ReadOnly", "Health(DirGet) while read-only: remote: denied"},
	"reopen-unwrapped": {func(s *memory.Store) remotetest.Subject {
		sub := subject(s)
		sub.Reopen = func(context.Context) (remote.Store, error) { return memory.Reopen(s), nil }

		return sub
	}, "ReadOnly", "Health(DirPut) while read-only on a store opened again: error <nil>"},
	"denies-after-close": {func(s *memory.Store) remotetest.Subject {
		return withReadOnlyFault(s, &readOnlyFault{denyFirst: true})
	}, "AfterClose", "Put after Close, read-only: error remote: denied"},
}

// broken names each fault, the one row it must fail, and a piece of the
// failure that row reports, so the row fails on the assertion that holds the
// rule the fault breaks.
var broken = map[string]struct {
	wrap func(*memory.Store) remote.Store
	row  string
	msg  string
}{
	"serves-empty":           {func(s *memory.Store) remote.Store { return servesEmpty{s} }, "GetEmptyRange", "of a 16-byte block: 0 bytes and error <nil>"},
	"serves-after-close":     {func(s *memory.Store) remote.Store { return servesAfterClose{s} }, "AfterClose", "Put after Close: error <nil>"},
	"fails-absent-delete":    {func(s *memory.Store) remote.Store { return failsAbsentDelete{s} }, "DeleteTwice", "the first Delete: result 1 is"},
	"deletes-one-page":       {func(s *memory.Store) remote.Store { return deletesOnePage{s} }, "DeleteBatch", "of them are not ErrNotFound on Get"},
	"instance-cursor":        {func(s *memory.Store) remote.Store { return instanceCursor{s} }, "ListWalk", "unknown cursor"},
	"accepts-foreign-cursor": {func(s *memory.Store) remote.Store { return acceptsForeignCursor{s} }, "ListForeignCursor", "outside the block namespace: "},
	"yields-foreign":         {func(s *memory.Store) remote.Store { return &yieldsForeign{Store: s} }, "ListForeignKey", "a walk in pages of 1000 yielded"},
	"ctx-at-entry":           {func(s *memory.Store) remote.Store { return ctxAtEntry{s} }, "ListCancel", "List under a context ended mid-page"},
	"appends-control":        {func(s *memory.Store) remote.Store { return appendsControl{s} }, "Control", "after put 2 of 1000 bytes"},
	"absent-control-empty":   {func(s *memory.Store) remote.Store { return absentControlEmpty{s} }, "Control", "GetControl of claim before any put of it"},
	"without-health":         {withoutHealth, "Control", "GetControl of the health object before any put of it"},
	"invents-health":         {func(s *memory.Store) remote.Store { return inventsHealth{withoutHealth(s).(*memory.Store)} }, "Control", "the raw namespace lacks control/health"},
	"control-in-process": {func(s *memory.Store) remote.Store {
		return &controlInProcess{Store: s, objects: map[remote.Role][]byte{}}
	}, "Control", "the raw namespace lacks control/claim"},
	"control-as-block":       {func(s *memory.Store) remote.Store { return controlAsBlock{s} }, "Control", "a walk after a put of every role yielded"},
	"control-under-blocks":   {func(s *memory.Store) remote.Store { return controlUnderBlocks{s} }, "Control", `the raw namespace holds the key "blocks/control/claim"`},
	"control-after-close":    {func(s *memory.Store) remote.Store { return controlAfterClose{s, memory.Reopen(s)} }, "AfterClose", "PutControl after Close: error <nil>"},
	"stores-refused-put":     {func(s *memory.Store) remote.Store { return storesRefusedPut{s} }, "AfterClose", "which the row's puts did not store"},
	"stores-refused-control": {func(s *memory.Store) remote.Store { return storesRefusedControl{s} }, "AfterClose", "after a PutControl refused after Close"},
	"health-after-close":     {func(s *memory.Store) remote.Store { return healthAfterClose{s, memory.Reopen(s)} }, "AfterClose", "Health(DirPut) after Close: error <nil>"},
}

// withoutHealth opens the in-memory store without its health object, as a
// store whose open skipped it.
func withoutHealth(s *memory.Store) remote.Store {
	memory.DeleteRaw(s, "control/health")
	return s
}

// brokenEnv selects the fault TestBrokenStore runs the suite over, in the
// child process TestTheSuiteFailsABrokenStoreOnItsRow starts.
const brokenEnv = "MEMORY_SUITE_BROKEN"

// TestBrokenStore runs the suite over the fault brokenEnv names, and is
// skipped without it: the failures it reports are the parent test's evidence.
func TestBrokenStore(t *testing.T) {
	fault := os.Getenv(brokenEnv)

	b, ok := broken[fault]
	ro, rok := readOnlyBroken[fault]

	if !ok && !rok {
		t.Skip("runs only as the child of TestTheSuiteFailsABrokenStoreOnItsRow")
	}

	remotetest.Run(t, func(t *testing.T) remotetest.Subject {
		inner := memory.New(sim.NewTestClock(epoch).Now, remote.ChecksumNone)

		if rok {
			return ro.sub(inner)
		}

		// The fault sits around the store itself, so no wrapper is in front
		// of it to turn the namespace read-only.
		sub := subject(inner)
		sub.Store = b.wrap(inner)
		sub.Reopen = func(context.Context) (remote.Store, error) { return b.wrap(memory.Reopen(inner)), nil }
		sub.ReadOnly = nil

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
	type want struct{ row, msg string }

	faults := make(map[string]want, len(broken)+len(readOnlyBroken))
	for f, b := range broken {
		faults[f] = want{b.row, b.msg}
	}

	for f, b := range readOnlyBroken {
		faults[f] = want{b.row, b.msg}
	}

	for fault, b := range faults {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBrokenStore$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), brokenEnv+"="+fault)
		out, _ := cmd.CombinedOutput()

		var rows []string
		for _, m := range failedRow.FindAllStringSubmatch(string(out), -1) {
			if !strings.Contains(m[1], "/") {
				rows = append(rows, m[1])
			}
		}

		if !slices.Equal(rows, []string{b.row}) || !strings.Contains(string(out), b.msg) {
			t.Errorf("the suite over a store that %s failed rows %q, want exactly %q failing with %q; output:\n%s", fault, rows, b.row, b.msg, out)
		}
	}
}
