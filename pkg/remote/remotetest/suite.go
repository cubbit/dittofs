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
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
)

// Subject is one store under test, and what a row reads or plants beneath it.
type Subject struct {
	Store remote.Store

	// PutRaw stores body under key, relative to the store's prefix, beneath
	// the store, as an object the store itself never writes. A row that needs
	// it skips on a subject without it.
	PutRaw func(ctx context.Context, key string, body []byte) error

	// Reopen opens a second store over Store's namespace, which the row
	// closes. A row that needs it skips on a subject without it.
	Reopen func(ctx context.Context) (remote.Store, error)
}

// Run runs every row against a fresh subject from open, one row at a time.
// Each subject's store is over an empty namespace of its own, since a listing
// row asserts the exact set of blocks it walks.
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
	} {
		t.Run(r.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			sub := open(t)
			if sub.Store == nil {
				t.Fatal("the opener returned a Subject with no Store")
			}

			r.fn(t, sub)
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

// Every call after Close is ErrInvalid; Close again returns nil.
func testAfterClose(t *testing.T, sub Subject) {
	n, body := name("after-close"), block("after-close", 16)
	put(t, sub.Store, n, body)

	if err := sub.Store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := sub.Store.Put(t.Context(), n, bytes.NewReader(body), int64(len(body)), sum(t, sub.Store.Checksum(), body))
	if !only(err, remote.ErrInvalid) {
		t.Errorf("Put after Close: error %v, want ErrInvalid and no other sentinel", err)
	}

	got, err := get(t, sub.Store, n, remote.Range{})
	if !only(err, remote.ErrInvalid) {
		t.Errorf("Get after Close: %d bytes and error %v, want ErrInvalid and no other sentinel", len(got), err)
	}

	for i, err := range sub.Store.Delete(t.Context(), []remote.Name{n}) {
		if !only(err, remote.ErrInvalid) {
			t.Errorf("Delete after Close: result %d is %v, want ErrInvalid and no other sentinel", i, err)
		}
	}

	if page, _, _, err := sub.Store.List(t.Context(), "", 10); !only(err, remote.ErrInvalid) {
		t.Errorf("List after Close: %d blocks and error %v, want ErrInvalid and no other sentinel", len(page), err)
	}

	if err := sub.Store.Close(); err != nil {
		t.Errorf("Close after Close: %v, want nil", err)
	}
}

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
		var (
			got  []remote.Name
			next remote.Cursor
		)

		for calls, done := 0, false; !done; calls++ {
			if calls > 5 {
				t.Fatalf("a walk in pages of %d over 5 keys did not end after %d calls", n, calls)
			}

			var page []remote.Info

			page, next, done = listPage(t, sub.Store, next, n)
			for _, i := range page {
				got = append(got, i.Name)
			}
		}

		if !slices.Equal(got, want) {
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
