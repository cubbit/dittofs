// Package remotetest is the remote store conformance suite (RFC 4 §7.1): the
// rows that need no fault transport, run against whatever store an opener
// returns. The in-memory store runs it at every change; a store over a real
// service runs it through its own opener.
package remotetest

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
)

// Subject is one store under test, and what a row reads or plants beneath it.
type Subject struct {
	Store remote.Store
}

// Run runs every row against a fresh subject from open, one row at a time.
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

	if err := sub.Store.Close(); err != nil {
		t.Errorf("Close after Close: %v, want nil", err)
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
