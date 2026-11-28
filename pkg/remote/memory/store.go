// Package memory is the in-memory remote store: a verified implementation of
// remote.Store (RFC 4 §4.1) that the conformance suite in remotetest holds to
// the real one, for other components' tests to run against (RFC 0 §1.2).
//
// It lays its objects out as the S3 store's bucket does (RFC 4 Appendix C.2)
// under an empty prefix, and refuses what the contract refuses. It loses no
// acknowledged write and never delays, so a property of a real service is
// never proven against it alone. It reads time only through the clock it is
// given, and imports the contract and the standard library only.
package memory

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/remote"
)

// Store is the in-memory remote store. Its methods are safe for concurrent
// use, two puts of one name overlapping included.
type Store struct {
	now    func() time.Time
	sum    remote.Checksum
	closed atomic.Bool
	ns     *namespace
}

// namespace is the objects a store holds, shared by every store opened over
// it, as a service's bucket is shared by its clients.
type namespace struct {
	mu      sync.Mutex
	objects map[string]object // by key relative to the prefix
}

// object is one stored object. Its body is never changed once stored: a put
// replaces the object whole.
type object struct {
	body    []byte
	modTime time.Time
}

var _ remote.Store = (*Store)(nil)

// New returns a store that checks each put's checksum in kind sum, as a
// service that checks the put does, or compares nothing for
// remote.ChecksumNone. It holds only its health object.
// now is the node's clock's Now, read only for Info.ModTime. A kind outside
// the three panics, since a store that cannot check it would accept every put
// unverified.
func New(now func() time.Time, sum remote.Checksum) *Store {
	switch sum {
	case remote.ChecksumNone, remote.ChecksumCRC32C, remote.ChecksumMD5:
	default:
		panic(fmt.Sprintf("memory: unknown checksum kind %d", sum))
	}

	// The health object exists from the moment the store is returned, as a
	// real store's capability check writes it before the open returns
	// (RFC 4 §4.7).
	objects := map[string]object{healthKey: {body: []byte{}, modTime: now()}}

	return &Store{now: now, sum: sum, ns: &namespace{objects: objects}}
}

// blockKey is a block's key: blocks/ and the name in lowercase hex, nothing
// between them (RFC 4 §4.2, Appendix C.2).
func blockKey(n remote.Name) string { return blockPrefix + hex.EncodeToString(n[:]) }

// blockPrefix is the block namespace's key prefix.
const blockPrefix = "blocks/"

// Checksum is the kind the store was built with.
func (s *Store) Checksum() remote.Checksum { return s.sum }

// readChunk is how much of a body Put asks for in one Read.
const readChunk = 32 << 10

// Put reads body once, as a stream of exactly size bytes, checking ctx before
// each read, verifies sum over it in the store's mode, and only then stores it
// under name, replacing any block there. A put that fails stores nothing and
// leaves a block already under name as it was. A stream shorter or longer than
// size is remote.ErrInvalid; a sum that does not match is remote.ErrCorrupt. A
// cancelled ctx or a failing reader is returned as itself. In
// remote.ChecksumNone mode sum is not read.
func (s *Store) Put(ctx context.Context, name remote.Name, body io.Reader, size int64, sum []byte) error {
	if size < 0 {
		return fmt.Errorf("memory: put of %d bytes: %w", size, remote.ErrInvalid)
	}

	if err := s.open("Put"); err != nil {
		return err
	}

	// A sum missing or of the wrong length is the caller's error, not a
	// mismatch over the body, so it is refused before a byte is read.
	h := s.hash()
	if h != nil && len(sum) != h.Size() {
		return fmt.Errorf("memory: put with a %d-byte checksum, want %d: %w", len(sum), h.Size(), remote.ErrInvalid)
	}

	// The buffer grows as the body is read, never from size, so a size the
	// body does not back allocates nothing. One byte past size shows a stream
	// longer than declared; size+1 overflowing reads nothing, a short stream.
	var buf []byte

	lr, scratch := io.LimitReader(body, size+1), make([]byte, readChunk)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		n, err := lr.Read(scratch)
		buf = append(buf, scratch[:n]...)

		if h != nil {
			h.Write(scratch[:n])
		}

		if err == io.EOF {
			break
		}

		if err != nil {
			return err
		}
	}

	switch {
	case int64(len(buf)) > size:
		return fmt.Errorf("memory: put declared %d bytes, body is longer: %w", size, remote.ErrInvalid)
	case int64(len(buf)) < size:
		return fmt.Errorf("memory: put declared %d bytes, body held %d: %w", size, len(buf), remote.ErrInvalid)
	}

	if h != nil {
		if got := h.Sum(nil); !bytes.Equal(got, sum) {
			return fmt.Errorf("memory: put checksum %x, body's %x: %w", sum, got, remote.ErrCorrupt)
		}
	}

	s.ns.mu.Lock()
	s.ns.objects[blockKey(name)] = object{body: buf, modTime: s.now()}
	s.ns.mu.Unlock()

	return nil
}

// hash returns a new hash of the store's checksum kind, or nil for none.
func (s *Store) hash() hash.Hash {
	switch s.sum {
	case remote.ChecksumCRC32C:
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case remote.ChecksumMD5:
		return md5.New()
	}

	return nil
}

// Get returns a reader over a copy of the bytes of r, or of the whole block for
// the zero Range. A range that is empty, negative or extends past the end is
// remote.ErrInvalid, never fewer bytes; an absent block is remote.ErrNotFound.
//
// ponytail: a whole-block get reports no stored checksum (StoredSum.Kind is
// remote.ChecksumNone, as RFC 4 §4.1 allows when the service reports none), so
// a raw copy's test cannot read one here. Keep the sum the put was checked
// against and report it when a raw copy's test needs one without a service.
func (s *Store) Get(ctx context.Context, name remote.Name, r remote.Range) (io.ReadCloser, remote.StoredSum, error) {
	if err := s.open("Get"); err != nil {
		return nil, remote.StoredSum{}, err
	}

	if err := ctx.Err(); err != nil {
		return nil, remote.StoredSum{}, err
	}

	s.ns.mu.Lock()
	o, ok := s.ns.objects[blockKey(name)]
	s.ns.mu.Unlock()

	if !ok {
		return nil, remote.StoredSum{}, fmt.Errorf("memory: get of block %x: %w", name, remote.ErrNotFound)
	}

	b := o.body
	if r != (remote.Range{}) {
		size := int64(len(b))
		if r.Off < 0 || r.Len <= 0 || r.Len > size-r.Off {
			return nil, remote.StoredSum{}, fmt.Errorf("memory: get of %+v of a %d-byte block: %w", r, size, remote.ErrInvalid)
		}

		b = b[r.Off : r.Off+r.Len]
	}

	return io.NopCloser(bytes.NewReader(bytes.Clone(b))), remote.StoredSum{}, nil
}

// Close marks the store closed: every later call but Close is refused with
// remote.ErrInvalid. Its objects are dropped by no call.
func (s *Store) Close() error {
	s.closed.Store(true)
	return nil
}

// open refuses call once the store is closed.
func (s *Store) open(call string) error {
	if s.closed.Load() {
		return fmt.Errorf("memory: %s after Close: %w", call, remote.ErrInvalid)
	}

	return nil
}
