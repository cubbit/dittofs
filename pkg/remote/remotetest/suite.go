// Package remotetest is the remote store conformance suite (RFC 4 §7.1): the
// rows that need no fault transport, run against whatever store an opener
// returns. The in-memory store runs it at every change; a store over a real
// service runs it through its own opener.
package remotetest

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
)

// Subject is one store under test, and what a row reads or plants beneath it.
type Subject struct {
	// Store is opened as the checked open leaves it: holding its health
	// object, which exists from the moment a store is returned (RFC 4 §4.7).
	Store remote.Store

	// PutRaw stores body under key, relative to the store's prefix, beneath
	// the store, as an object the store itself never writes. A row that needs
	// it skips on a subject without it.
	PutRaw func(ctx context.Context, key string, body []byte) error

	// Reopen opens a second store over Store's namespace, which the row
	// closes. A row that needs it skips on a subject without it.
	Reopen func(ctx context.Context) (remote.Store, error)

	// Raw returns every key the service holds under the store's namespace,
	// relative to its prefix, read beneath the store. It runs inside each
	// row's leak watch, so a client behind it is shared across rows or closed
	// by the opener's cleanup. The raw-namespace check after each row skips on
	// a subject without it, and so do the halves of rows that read it.
	Raw func(ctx context.Context) ([]string, error)

	// ReadOnly turns the store's namespace read-only for every store over
	// it, Reopen's included, or gives write access back, from the next call
	// on. A row that needs it skips on a subject without it.
	ReadOnly func(ctx context.Context, on bool) error
}

// Run runs every row against a fresh subject from open, one row at a time.
// Each subject's store is over an empty namespace of its own, since a listing
// row asserts the exact set of blocks it walks.
//
// After each row, its subtest RawNamespace checks the raw namespace (RFC 4
// §4.2) on that row's subject rather than once after the suite, so a
// misplaced key is blamed on the row that wrote it. It counts a block as
// stored only once its put returned nil, so a row whose put may store despite
// an error deletes that name before it returns, and a row never overlaps a
// put and a delete of one name.
//
// Each row watches for leaks before it calls open, so the cleanups open
// registers, closing the store among them, run before the leak check: a row
// fails if a goroutine, descriptor or heap growth outlives its subject. The
// watch is process-wide, so the test calling Run must not watch, and must
// not run beside a parallel test.
func Run(t *testing.T, open func(t *testing.T) Subject) {
	for _, r := range []struct {
		name string
		fn   func(*testing.T, Subject)
	}{
		{"PutThenGet", testPutThenGet},
		{"GetRanges", testGetRanges},
		{"GetAbsent", testGetAbsent},
		{"GetEmptyRange", testGetEmptyRange},
		{"AfterClose", testAfterClose},
		{"DeleteTwice", testDeleteTwice},
		{"DeleteBatch", testDeleteBatch},
		{"ListWalk", testListWalk},
		{"ListForeignCursor", testListForeignCursor},
		{"ListForeignKey", testListForeignKey},
		{"ListCancel", testListCancel},
		{"Control", testControl},
		{"ReadOnly", testReadOnly},
	} {
		t.Run(r.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			sub := open(t)
			if sub.Store == nil {
				t.Fatal("the opener returned a Subject with no Store")
			}

			w := track(&sub)
			r.fn(t, sub)

			t.Run("RawNamespace", func(t *testing.T) { checkRaw(t, sub.Raw, w) })
		})
	}
}

// blockSize is the size of the block each row puts: odd, so no range lines up
// with a power of two, and small enough for a real service.
const blockSize = 64<<10 + 7

// block returns size pseudo-random bytes from seed, so a get at the wrong
// offset returns different bytes.
func block(seed string, size int) []byte {
	out := make([]byte, 0, size+sha256.Size)
	h := sha256.Sum256([]byte(seed))

	for len(out) < size {
		out = append(out, h[:]...)
		h = sha256.Sum256(h[:])
	}

	return out[:size]
}

// name returns a block name derived from label, uniformly distributed as a
// real name is.
func name(label string) remote.Name { return sha256.Sum256([]byte("remotetest/" + label)) }

// sum returns the checksum a put of body carries in kind: the raw digest, as
// hash.Hash's Sum returns it, or nil for none.
func sum(t *testing.T, kind remote.Checksum, body []byte) []byte {
	t.Helper()

	switch kind {
	case remote.ChecksumNone:
		return nil
	case remote.ChecksumCRC32C:
		h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
		h.Write(body)

		return h.Sum(nil)
	case remote.ChecksumMD5:
		d := md5.Sum(body)
		return d[:]
	}

	t.Fatalf("the store's Checksum() is %d, which the suite does not know", kind)

	return nil
}

// put stores body under n, with the checksum the store asks for.
func put(t *testing.T, s remote.Store, n remote.Name, body []byte) {
	t.Helper()

	if err := s.Put(t.Context(), n, bytes.NewReader(body), int64(len(body)), sum(t, s.Checksum(), body)); err != nil {
		t.Fatalf("Put of %d bytes: %v", len(body), err)
	}
}

// get reads r of n whole and closes the reader.
func get(t *testing.T, s remote.Store, n remote.Name, r remote.Range) ([]byte, error) {
	t.Helper()

	body, _, err := s.Get(t.Context(), n, r)
	if err != nil {
		return nil, err
	}

	got, err := io.ReadAll(body)
	if cerr := body.Close(); err == nil {
		err = cerr
	}

	return got, err
}

// A put, then a get of the whole block, returns exactly the bytes put.
func testPutThenGet(t *testing.T, sub Subject) {
	n, want := name("put-then-get"), block("put-then-get", blockSize)
	put(t, sub.Store, n, want)

	got, err := get(t, sub.Store, n, remote.Range{})
	if err != nil {
		t.Fatalf("Get of the whole block after a Put: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Fatalf("Get of the whole block: %d bytes, want the %d bytes put (first difference at %d)", len(got), len(want), firstDiff(got, want))
	}
}

// A get of each range inside the block returns exactly its bytes; a range
// that extends past the end, or starts at or past it, is ErrInvalid, never
// fewer bytes.
func testGetRanges(t *testing.T, sub Subject) {
	n, want := name("get-ranges"), block("get-ranges", blockSize)
	put(t, sub.Store, n, want)

	const size = int64(blockSize)

	for _, r := range []remote.Range{
		{Off: 0, Len: 1},
		{Off: size - 1, Len: 1},
		{Off: 4093, Len: 8191},
		{Off: 1, Len: size - 2},
		{Off: 0, Len: size},
		{Off: size / 2, Len: size - size/2},
	} {
		got, err := get(t, sub.Store, n, r)
		if err != nil {
			t.Errorf("Get of %+v inside a %d-byte block: %v", r, size, err)
			continue
		}

		if w := want[r.Off : r.Off+r.Len]; !bytes.Equal(got, w) {
			t.Errorf("Get of %+v: %d bytes, want %d (first difference at %d)", r, len(got), len(w), firstDiff(got, w))
		}
	}

	for _, r := range []remote.Range{
		{Off: size - 1, Len: 2},
		{Off: 0, Len: size + 1},
		{Off: size, Len: 1},
		{Off: size + 4096, Len: 1},
	} {
		got, err := get(t, sub.Store, n, r)
		if !only(err, remote.ErrInvalid) {
			t.Errorf("Get of %+v of a %d-byte block: %d bytes and error %v, want ErrInvalid and no other sentinel", r, size, len(got), err)
		}
	}
}

// A get of a name never put is ErrNotFound, never an empty block.
func testGetAbsent(t *testing.T, sub Subject) {
	// A neighbour, so the store holds a block and is not merely empty.
	put(t, sub.Store, name("get-absent-neighbour"), block("get-absent-neighbour", 16))

	got, err := get(t, sub.Store, name("get-absent"), remote.Range{})
	if !only(err, remote.ErrNotFound) {
		t.Fatalf("Get of a name never put: %d bytes and error %v, want ErrNotFound and no other sentinel", len(got), err)
	}
}

// A range of no bytes other than the zero Range is ErrInvalid, never the
// whole block or nothing.
func testGetEmptyRange(t *testing.T, sub Subject) {
	n := name("get-empty-range")
	put(t, sub.Store, n, block("get-empty-range", 16))

	for _, r := range []remote.Range{{Off: 1}, {Off: 15}, {Off: 16}, {Off: 1, Len: -1}} {
		got, err := get(t, sub.Store, n, r)
		if !only(err, remote.ErrInvalid) {
			t.Errorf("Get of %+v of a 16-byte block: %d bytes and error %v, want ErrInvalid and no other sentinel", r, len(got), err)
		}
	}
}

// Every call after Close is ErrInvalid, and a put refused so stores nothing;
// Close again returns nil. On a subject that can make its namespace
// read-only, every call is made again once it is, and is still ErrInvalid,
// never a refusal of access: a closed store refuses before anything else.
func testAfterClose(t *testing.T, sub Subject) {
	n, body := name("after-close"), block("after-close", 16)
	put(t, sub.Store, n, body)

	if err := sub.Store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	callsAfterClose(t, sub, n, body, "")

	if sub.ReadOnly != nil {
		if err := sub.ReadOnly(t.Context(), true); err != nil {
			t.Fatalf("ReadOnly(true): %v", err)
		}

		callsAfterClose(t, sub, n, body, ", read-only")
	}

	if err := sub.Store.Close(); err != nil {
		t.Errorf("Close after Close: %v, want nil", err)
	}
}

// callsAfterClose makes every call but Close on sub's closed store, n stored
// before the close, and fails t on each that is not ErrInvalid alone; mode
// names the namespace's state in each message.
func callsAfterClose(t *testing.T, sub Subject, n remote.Name, body []byte, mode string) {
	t.Helper()

	// A name never stored, so a put that stores what it refuses shows in the
	// raw namespace; so does a control put, read beneath the store below.
	err := sub.Store.Put(t.Context(), name("after-close-new"), bytes.NewReader(body), int64(len(body)), sum(t, sub.Store.Checksum(), body))
	if !only(err, remote.ErrInvalid) {
		t.Errorf("Put after Close%s: error %v, want ErrInvalid and no other sentinel", mode, err)
	}

	got, err := get(t, sub.Store, n, remote.Range{})
	if !only(err, remote.ErrInvalid) {
		t.Errorf("Get after Close%s: %d bytes and error %v, want ErrInvalid and no other sentinel", mode, len(got), err)
	}

	for i, err := range sub.Store.Delete(t.Context(), []remote.Name{n}) {
		if !only(err, remote.ErrInvalid) {
			t.Errorf("Delete after Close%s: result %d is %v, want ErrInvalid and no other sentinel", mode, i, err)
		}
	}

	if page, _, _, err := sub.Store.List(t.Context(), "", 10); !only(err, remote.ErrInvalid) {
		t.Errorf("List after Close%s: %d blocks and error %v, want ErrInvalid and no other sentinel", mode, len(page), err)
	}

	if err := sub.Store.PutControl(t.Context(), remote.RoleClaim, body); !only(err, remote.ErrInvalid) {
		t.Errorf("PutControl after Close%s: error %v, want ErrInvalid and no other sentinel", mode, err)
	}

	if got, _, err := sub.Store.GetControl(t.Context(), remote.RoleHealth); !only(err, remote.ErrInvalid) {
		t.Errorf("GetControl after Close%s: %d bytes and error %v, want ErrInvalid and no other sentinel", mode, len(got), err)
	}

	for _, d := range []remote.Direction{remote.DirPut, remote.DirGet} {
		if err := sub.Store.Health(t.Context(), d); !only(err, remote.ErrInvalid) {
			t.Errorf("Health(%s) after Close%s: error %v, want ErrInvalid and no other sentinel", dirName[d], mode, err)
		}
	}

	if sub.Raw != nil && slices.Contains(rawKeys(t, sub.Raw), controlKeys[remote.RoleClaim]) {
		t.Errorf("the raw namespace holds %s after a PutControl refused after Close%s, want nothing stored", controlKeys[remote.RoleClaim], mode)
	}
}

// dirName is each probe direction's name in a row's messages.
var dirName = map[remote.Direction]string{remote.DirPut: "DirPut", remote.DirGet: "DirGet"}

// A batch deleted twice, holding a name repeated and a name never written,
// returns nil for every name each time, and leaves its blocks absent.
func testDeleteTwice(t *testing.T, sub Subject) {
	a, b := name("delete-twice-a"), name("delete-twice-b")
	put(t, sub.Store, a, block("delete-twice-a", 16))
	put(t, sub.Store, b, block("delete-twice-b", 17))

	batch := []remote.Name{a, name("delete-twice-never"), b, a}

	for _, pass := range []string{"first", "second"} {
		errs := sub.Store.Delete(t.Context(), batch)
		if len(errs) != len(batch) {
			t.Fatalf("the %s Delete of %d names returned %d results, want one per name", pass, len(batch), len(errs))
		}

		for i, err := range errs {
			if err != nil {
				t.Errorf("the %s Delete: result %d is %v, want nil", pass, i, err)
			}
		}
	}

	for _, n := range []remote.Name{a, b} {
		if got, err := get(t, sub.Store, n, remote.Range{}); !only(err, remote.ErrNotFound) {
			t.Errorf("Get of %x after its Delete: %d bytes and error %v, want ErrNotFound", n[:4], len(got), err)
		}
	}
}

// batchSize is above a service's limit on one delete or one listing page, so
// a store that does not split a batch, or a walk that does not page, shows.
const batchSize = 2500

// A batch of more names than one service request carries deletes every one
// of them, and nothing outside it.
func testDeleteBatch(t *testing.T, sub Subject) {
	names := putMany(t, sub.Store, "delete-batch", batchSize)

	keep := name("delete-batch-keep")
	put(t, sub.Store, keep, block("delete-batch-keep", 16))

	errs := sub.Store.Delete(t.Context(), names)
	if len(errs) != len(names) {
		t.Fatalf("Delete of %d names returned %d results, want one per name", len(names), len(errs))
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("Delete of %d names: result %d is %v, want nil", len(names), i, err)
		}
	}

	left := 0

	for _, n := range names {
		if _, err := get(t, sub.Store, n, remote.Range{}); !only(err, remote.ErrNotFound) {
			left++
		}
	}

	if left != 0 {
		t.Errorf("after Delete of %d names, %d of them are not ErrNotFound on Get, want 0", len(names), left)
	}

	if _, err := get(t, sub.Store, keep, remote.Range{}); err != nil {
		t.Errorf("Get of a block outside the deleted batch: %v, want it still stored", err)
	}
}

// A walk in pages of 1,000, stopped after the second page and resumed from its
// cursor on a store opened again over the same namespace, yields every block
// exactly once, in ascending name order, with the size put and a time, and
// no page holds more than its n. Listing after the end yields nothing, done,
// at the same cursor.
func testListWalk(t *testing.T, sub Subject) {
	if sub.Reopen == nil {
		t.Skip("the subject has no Reopen hook")
	}

	const pageSize = 1000

	names := putMany(t, sub.Store, "list-walk", batchSize)

	var (
		seen []remote.Info
		next remote.Cursor
		s    = sub.Store
	)

	// Two pages on the store the blocks were put through, then the rest on a
	// store opened again; a walk that makes progress ends within one call per
	// block.
	for calls, done := 0, false; !done; calls++ {
		if calls > len(names) {
			t.Fatalf("the walk did not end after %d calls", calls)
		}

		if calls == 2 {
			var err error
			if s, err = sub.Reopen(t.Context()); err != nil {
				t.Fatalf("Reopen: %v", err)
			}

			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Errorf("Close of the reopened store: %v", err)
				}
			})
		}

		var page []remote.Info

		page, next, done = listPage(t, s, next, pageSize)
		seen = append(seen, page...)
	}

	checkWalk(t, seen, names)

	page, end, done, err := s.List(t.Context(), next, pageSize)
	if err != nil || len(page) != 0 || !done || end != next {
		t.Errorf("List after the walk's end: %d blocks, cursor %q, done %v, error %v; want no block, cursor %q, done, nil", len(page), end, done, err, next)
	}
}

// A cursor outside the store's block namespace, one a store under another
// prefix would return or one naming a key of this store outside blocks/, is
// ErrInvalid, never a starting point.
func testListForeignCursor(t *testing.T, sub Subject) {
	n := name("list-foreign-cursor")
	put(t, sub.Store, n, block("list-foreign-cursor", 16))

	_, next, _ := listPage(t, sub.Store, "", 1)

	// The cursor holds the full key, the store's prefix then blocks/ and the
	// name.
	h := hex.EncodeToString(n[:])

	prefix, ok := strings.CutSuffix(string(next), "blocks/"+h)
	if !ok {
		t.Fatalf("List's cursor after the only block is %q, want its key, ending blocks/%s", next, h)
	}

	// The first is the cursor a store under the prefix "elsewhere/" and then
	// this store's would return for the same block.
	foreign := []remote.Cursor{
		remote.Cursor("elsewhere/" + string(next)),
		remote.Cursor(prefix + "control/" + h),
	}
	if prefix != "" {
		foreign = append(foreign, remote.Cursor("blocks/"+h))
	}

	for _, c := range foreign {
		page, _, _, err := sub.Store.List(t.Context(), c, 10)
		if !only(err, remote.ErrInvalid) {
			t.Errorf("List after the cursor %q, outside the block namespace: %d blocks and error %v, want ErrInvalid and no other sentinel", c, len(page), err)
		}
	}
}

// A key under the block namespace that is not a block name is skipped: a walk
// yields the blocks put and only them, in pages of any size, a page of one
// that ends on such a key included. One key is short, sorting just before the
// block whose name it would pad to, so a cursor read as a name skips that
// block; one is a name in uppercase hex, which is not the key of any name.
func testListForeignKey(t *testing.T, sub Subject) {
	if sub.PutRaw == nil {
		t.Skip("the subject has no PutRaw hook")
	}

	lo, hi := name("list-foreign-lo"), name("list-foreign-hi")
	lo[0], hi[0] = 0x00, 0xff
	padded := remote.Name{0x7f}

	want := []remote.Name{lo, padded, hi}
	for i, n := range want {
		put(t, sub.Store, n, block(fmt.Sprintf("list-foreign/%d", i), 16+i))
	}

	upper := remote.Name{0xab}
	for _, k := range []string{"blocks/7f", "blocks/" + strings.ToUpper(hex.EncodeToString(upper[:]))} {
		if err := sub.PutRaw(t.Context(), k, []byte("not a block")); err != nil {
			t.Fatalf("PutRaw of %s: %v", k, err)
		}
	}

	for _, n := range []int{1000, 1} {
		if got := walkNames(t, sub.Store, n, 5); !slices.Equal(got, want) {
			t.Errorf("a walk in pages of %d yielded %d blocks %x, want the %d blocks put %x", n, len(got), got, len(want), want)
		}
	}
}

// A context that ends mid-page ends the call with the context's error.
func testListCancel(t *testing.T, sub Subject) {
	putMany(t, sub.Store, "list-cancel", 1000)

	ctx := &endsOnSecondCheck{Context: t.Context()}

	page, _, _, err := sub.Store.List(ctx, "", 1000)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("List under a context ended mid-page: %d blocks and error %v, want context.Canceled", len(page), err)
	}
}

// endsOnSecondCheck is a fault around a context: it reports itself ended from
// its second check on, through Err or Done, so a call that checks it only once,
// at entry, runs to its end.
//
// ponytail: a store that derives a context from it first, as
// context.WithTimeout does, is attached to the parent's own cancellation and
// never asks it again, so the row fails such a store though it honours an
// ended context. Cancel a real context from a subject hook mid-page when a
// store derives one.
type endsOnSecondCheck struct {
	context.Context

	checks atomic.Int32
}

// ended is the channel Done returns once the context reports itself ended.
var ended = func() chan struct{} {
	c := make(chan struct{})
	close(c)

	return c
}()

func (c *endsOnSecondCheck) Err() error {
	if c.checks.Add(1) > 1 {
		return context.Canceled
	}

	return c.Context.Err()
}

func (c *endsOnSecondCheck) Done() <-chan struct{} {
	if c.checks.Add(1) > 1 {
		return ended
	}

	return c.Context.Done()
}

// roles is the closed set of control roles, and controlKeys each role's key
// as RFC 4 Appendix C.2 writes it, spelled out rather than derived from the
// role's String, which a store keys by.
var (
	roles       = []remote.Role{remote.RoleClaim, remote.RoleHealth, remote.RoleKeys}
	controlKeys = map[remote.Role]string{
		remote.RoleClaim:  "control/claim",
		remote.RoleHealth: "control/health",
		remote.RoleKeys:   "control/keys",
	}
)

// Each role's object round-trips, and a second, shorter put replaces it
// whole, at its fixed key, so a store opened again reads it too; an absent
// role is ErrNotFound, but for the health object, which exists from the
// store's open and so is read before any put of it; and a block listing
// yields the one block put and no control object.
func testControl(t *testing.T, sub Subject) {
	s := sub.Store

	if _, _, err := s.GetControl(t.Context(), remote.RoleHealth); err != nil {
		t.Errorf("GetControl of the health object before any put of it: %v, want the object the store holds from its open", err)
	}

	if sub.Raw != nil && !slices.Contains(rawKeys(t, sub.Raw), controlKeys[remote.RoleHealth]) {
		t.Errorf("the raw namespace lacks %s before any put of it, want the health object the store holds from its open", controlKeys[remote.RoleHealth])
	}

	last := make(map[remote.Role][]byte, len(roles))

	for _, r := range roles {
		if r != remote.RoleHealth {
			if got, _, err := s.GetControl(t.Context(), r); !only(err, remote.ErrNotFound) {
				t.Errorf("GetControl of %v before any put of it: %d bytes and error %v, want ErrNotFound and no other sentinel", r, len(got), err)
			}
		}

		for i, size := range []int{4099, 1000} {
			want := block(fmt.Sprintf("control/%v/%d", r, i), size)
			if err := s.PutControl(t.Context(), r, want); err != nil {
				t.Fatalf("PutControl of %v, %d bytes: %v", r, size, err)
			}

			got, _, err := s.GetControl(t.Context(), r)
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("GetControl of %v after put %d of %d bytes: %d bytes and error %v, want exactly the bytes put (first difference at %d)", r, i+1, size, len(got), err, firstDiff(got, want))
			}

			last[r] = want
		}
	}

	if sub.Raw != nil {
		keys := rawKeys(t, sub.Raw)
		for _, r := range roles {
			if !slices.Contains(keys, controlKeys[r]) {
				t.Errorf("the raw namespace lacks %s after a put of %v, want the object at its fixed key", controlKeys[r], r)
			}
		}
	}

	if sub.Reopen != nil {
		again, err := sub.Reopen(t.Context())
		if err != nil {
			t.Fatalf("Reopen: %v", err)
		}

		for _, r := range roles {
			if got, _, err := again.GetControl(t.Context(), r); err != nil || !bytes.Equal(got, last[r]) {
				t.Errorf("GetControl of %v on a store opened again: %d bytes and error %v, want the %d bytes put last", r, len(got), err, len(last[r]))
			}
		}

		if err := again.Close(); err != nil {
			t.Errorf("Close of the reopened store: %v", err)
		}
	}

	n := name("control-neighbour")
	put(t, s, n, block("control-neighbour", 16))

	if got := walkNames(t, s, 1000, 4); !slices.Equal(got, []remote.Name{n}) {
		t.Errorf("a walk after a put of every role yielded %d blocks %x, want only the block put %x", len(got), got, n)
	}
}

// On a fresh store both probes pass. While the namespace is read-only the put
// probe is ErrDenied of cause access, on a store opened again over it too,
// and the get probe and every read pass; the first put probe once write
// access returns passes.
func testReadOnly(t *testing.T, sub Subject) {
	if sub.ReadOnly == nil {
		t.Skip("the subject has no ReadOnly hook")
	}

	s := sub.Store
	n, want := name("read-only"), block("read-only", 16)
	put(t, s, n, want)

	for _, d := range []remote.Direction{remote.DirPut, remote.DirGet} {
		if err := s.Health(t.Context(), d); err != nil {
			t.Errorf("Health(%s) on a fresh store: %v, want nil", dirName[d], err)
		}
	}

	if err := sub.ReadOnly(t.Context(), true); err != nil {
		t.Fatalf("ReadOnly(true): %v", err)
	}

	stores := map[string]remote.Store{"": s}

	if sub.Reopen != nil {
		again, err := sub.Reopen(t.Context())
		if err != nil {
			t.Fatalf("Reopen: %v", err)
		}

		t.Cleanup(func() {
			if err := again.Close(); err != nil {
				t.Errorf("Close of the reopened store: %v", err)
			}
		})

		stores[" on a store opened again"] = again
	}

	// Each probe twice, so a refusal that lasts one call, rather than the
	// namespace's state, shows.
	for on, s := range stores {
		for range 2 {
			var de *remote.DeniedError
			if err := s.Health(t.Context(), remote.DirPut); !only(err, remote.ErrDenied) || !errors.As(err, &de) || de.Cause != remote.CauseAccess {
				t.Errorf("Health(DirPut) while read-only%s: error %v, want ErrDenied alone, a *remote.DeniedError of cause access", on, err)
			}

			if err := s.Health(t.Context(), remote.DirGet); err != nil {
				t.Errorf("Health(DirGet) while read-only%s: %v, want nil", on, err)
			}
		}
	}

	if got, err := get(t, s, n, remote.Range{}); err != nil || !bytes.Equal(got, want) {
		t.Errorf("Get while read-only: %d bytes and error %v, want the %d bytes put", len(got), err, len(want))
	}

	if _, _, err := s.GetControl(t.Context(), remote.RoleHealth); err != nil {
		t.Errorf("GetControl of the health object while read-only: %v, want nil", err)
	}

	if page, _, _, err := s.List(t.Context(), "", 10); err != nil || len(page) != 1 {
		t.Errorf("List while read-only: %d blocks and error %v, want the one block put", len(page), err)
	}

	if err := sub.ReadOnly(t.Context(), false); err != nil {
		t.Fatalf("ReadOnly(false): %v", err)
	}

	for on, s := range stores {
		if err := s.Health(t.Context(), remote.DirPut); err != nil {
			t.Errorf("Health(DirPut) after read-only ended%s: %v, want nil on the first call once write access returns", on, err)
		}

		if err := s.Health(t.Context(), remote.DirGet); err != nil {
			t.Errorf("Health(DirGet) after read-only ended%s: %v, want nil", on, err)
		}
	}
}

// walkNames lists s from the start in pages of n and returns the names
// yielded, failing t if the walk does not end within calls calls.
func walkNames(t *testing.T, s remote.Store, n, calls int) []remote.Name {
	t.Helper()

	var (
		got  []remote.Name
		next remote.Cursor
	)

	for c, done := 0, false; !done; c++ {
		if c > calls {
			t.Fatalf("a walk in pages of %d did not end after %d calls", n, c)
		}

		var page []remote.Info

		page, next, done = listPage(t, s, next, n)
		for _, i := range page {
			got = append(got, i.Name)
		}
	}

	return got
}

// rawKeys returns the raw namespace's keys, failing t if raw does.
func rawKeys(t *testing.T, raw func(context.Context) ([]string, error)) []string {
	t.Helper()

	keys, err := raw(t.Context())
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}

	return keys
}

// written is what a row wrote through its subject: the blocks its puts stored
// and its deletes did not remove, and the keys it planted beneath the store.
type written struct {
	mu      sync.Mutex
	blocks  map[remote.Name]bool
	planted map[string]bool
}

// track points sub's Store, Reopen and PutRaw at wrappers that pass every
// call through and record what it wrote, and returns the record.
func track(sub *Subject) *written {
	w := &written{blocks: make(map[remote.Name]bool), planted: make(map[string]bool)}

	sub.Store = tracked{s: sub.Store, w: w}

	if reopen := sub.Reopen; reopen != nil {
		sub.Reopen = func(ctx context.Context) (remote.Store, error) {
			s, err := reopen(ctx)
			if err != nil {
				return nil, err
			}

			return tracked{s: s, w: w}, nil
		}
	}

	if putRaw := sub.PutRaw; putRaw != nil {
		sub.PutRaw = func(ctx context.Context, key string, body []byte) error {
			w.mu.Lock()
			w.planted[key] = true
			w.mu.Unlock()

			return putRaw(ctx, key, body)
		}
	}

	return w
}

// tracked passes every call through to its store, and records in w the
// blocks each put stored and each delete removed.
//
// decision: tracked writes out every method of remote.Store rather than
// embedding it, so a call added to the contract later does not compile here
// until the tracker decides what that call writes; an embedded store would
// pass a new write through unrecorded, and the raw check would blame it.
type tracked struct {
	s remote.Store
	w *written
}

func (s tracked) Checksum() remote.Checksum { return s.s.Checksum() }

func (s tracked) Get(ctx context.Context, n remote.Name, r remote.Range) (io.ReadCloser, remote.StoredSum, error) {
	return s.s.Get(ctx, n, r)
}

func (s tracked) List(ctx context.Context, after remote.Cursor, n int) ([]remote.Info, remote.Cursor, bool, error) {
	return s.s.List(ctx, after, n)
}

func (s tracked) Health(ctx context.Context, d remote.Direction) error { return s.s.Health(ctx, d) }

func (s tracked) PutControl(ctx context.Context, r remote.Role, body []byte) error {
	return s.s.PutControl(ctx, r, body)
}

func (s tracked) GetControl(ctx context.Context, r remote.Role) ([]byte, remote.Version, error) {
	return s.s.GetControl(ctx, r)
}

func (s tracked) Close() error { return s.s.Close() }

func (s tracked) Put(ctx context.Context, n remote.Name, body io.Reader, size int64, sum []byte) error {
	err := s.s.Put(ctx, n, body, size, sum)
	if err == nil {
		s.w.mu.Lock()
		s.w.blocks[n] = true
		s.w.mu.Unlock()
	}

	return err
}

func (s tracked) Delete(ctx context.Context, names []remote.Name) []error {
	errs := s.s.Delete(ctx, names)

	s.w.mu.Lock()
	defer s.w.mu.Unlock()

	for i, err := range errs {
		if err == nil && i < len(names) {
			delete(s.w.blocks, names[i])
		}
	}

	return errs
}

// checkRaw fails t on each key of the raw namespace that is not a block w
// holds at blocks/ and its name in lowercase hex, a control object, a check
// object or a key the row planted.
func checkRaw(t *testing.T, raw func(context.Context) ([]string, error), w *written) {
	if raw == nil {
		t.Skip("the subject has no Raw hook")
	}

	keys := rawKeys(t, raw)

	control := make(map[string]bool, len(controlKeys))
	for _, k := range controlKeys {
		control[k] = true
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	var bad []string

	for _, k := range keys {
		if w.planted[k] || control[k] || isCheckKey(k) {
			continue
		}

		h, ok := strings.CutPrefix(k, "blocks/")
		if b, err := hex.DecodeString(h); ok && err == nil && len(b) == len(remote.Name{}) && hex.EncodeToString(b) == h {
			if !w.blocks[remote.Name(b)] {
				bad = append(bad, fmt.Sprintf("block %s, which the row's puts did not store or its deletes removed", h))
			}

			continue
		}

		bad = append(bad, fmt.Sprintf("the key %q, neither blocks/ and a name in lowercase hex, control/ and a role, nor check/<random>/ and a name", k))
	}

	for _, b := range bad[:min(len(bad), 10)] {
		t.Errorf("the raw namespace holds %s", b)
	}

	if len(bad) > 10 {
		t.Errorf("the raw namespace holds %d keys it should not, %d more than listed", len(bad), len(bad)-10)
	}
}

// isCheckKey reports whether k is a capability-check object's key,
// check/<random>/ and at least one more byte (RFC 4 Appendix C.2).
func isCheckKey(k string) bool {
	parts := strings.SplitN(k, "/", 3)
	return len(parts) == 3 && parts[0] == "check" && parts[1] != "" && parts[2] != ""
}

// putMany puts count blocks of sizes 1 to 61 bytes, derived from label, and
// returns their names.
func putMany(t *testing.T, s remote.Store, label string, count int) []remote.Name {
	t.Helper()

	names := make([]remote.Name, count)

	for i := range names {
		l := fmt.Sprintf("%s/%d", label, i)
		names[i] = name(l)
		put(t, s, names[i], block(l, blockSizeOf(i)))
	}

	return names
}

// blockSizeOf is the size putMany gives its i-th block, varied so a size
// reported for the wrong block shows.
func blockSizeOf(i int) int { return 1 + i%61 }

// listPage lists one page and fails t on an error or a page over n.
func listPage(t *testing.T, s remote.Store, after remote.Cursor, n int) ([]remote.Info, remote.Cursor, bool) {
	t.Helper()

	page, next, done, err := s.List(t.Context(), after, n)
	if err != nil {
		t.Fatalf("List of %d after %q: %v", n, after, err)
	}

	if len(page) > n {
		t.Fatalf("List of %d returned %d blocks", n, len(page))
	}

	return page, next, done
}

// checkWalk fails t unless seen is the blocks of putMany's names, each once,
// in ascending name order, with its size and a time.
func checkWalk(t *testing.T, seen []remote.Info, names []remote.Name) {
	t.Helper()

	size := make(map[remote.Name]int, len(names))
	for i, n := range names {
		size[n] = blockSizeOf(i)
	}

	want := slices.Clone(names)
	slices.SortFunc(want, func(a, b remote.Name) int { return bytes.Compare(a[:], b[:]) })

	if len(seen) != len(want) {
		t.Errorf("the walk yielded %d blocks, want the %d put", len(seen), len(want))
	}

	for i := range min(len(seen), len(want)) {
		got := seen[i]
		if got.Name != want[i] {
			t.Fatalf("the walk's block %d is %x, want %x: a block dropped, repeated or out of order", i, got.Name[:4], want[i][:4])
		}

		if got.Size != int64(size[got.Name]) {
			t.Errorf("the walk's block %d (%x) has Size %d, want the %d bytes put", i, got.Name[:4], got.Size, size[got.Name])
		}

		if got.ModTime.IsZero() {
			t.Errorf("the walk's block %d (%x) has a zero ModTime, want the time it was stored", i, got.Name[:4])
		}
	}
}

// sentinels is RFC 4 §4.8's set. ErrThrottled is matched as ErrTransient and
// ErrMalformed as ErrCorrupt, which each wraps.
var sentinels = []error{remote.ErrNotFound, remote.ErrInvalid, remote.ErrDenied, remote.ErrTransient, remote.ErrCorrupt}

// only reports whether err matches want and no other sentinel.
func only(err, want error) bool {
	for _, s := range sentinels {
		if errors.Is(err, s) != (s == want) {
			return false
		}
	}

	return true
}

// firstDiff returns the first offset at which a and b differ, or the shorter
// length when one is a prefix of the other.
func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}

	return min(len(a), len(b))
}
