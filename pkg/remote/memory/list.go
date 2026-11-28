package memory

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/pkg/remote"
)

// Delete removes each name's block and returns one result per name, in order:
// nil for a block now gone, an absent one included, so a batch of any size, a
// name repeated in it too, succeeds whole. A cancelled ctx is every name's
// result, and nothing is deleted.
func (s *Store) Delete(ctx context.Context, names []remote.Name) []error {
	defer s.enter("delete", remote.Name{})()

	errs := make([]error, len(names))

	err := s.open("Delete")
	if err == nil {
		err = ctx.Err()
	}

	if err != nil {
		for i := range errs {
			errs[i] = err
		}

		return errs
	}

	s.ns.mu.Lock()
	for _, n := range names {
		delete(s.ns.objects, blockKey(n))
	}
	s.ns.mu.Unlock()

	return errs
}

// List reads one page of at most n keys under blocks/ after the cursor's key,
// in ascending key order, which is ascending name order: n counts the keys
// listed, as a service's page size does, so a page may hold fewer blocks than
// keys. next is the page's last key, or after when it holds none, and done is
// true once no key under blocks/ sorts past next. A non-zero cursor outside
// blocks/, or an n of zero or less, is remote.ErrInvalid. ctx is checked
// before each key, so a context ended mid-page ends the call with its error.
//
// ponytail: each call scans every object and sorts the keys under blocks/
// after the cursor, O(k log k) per page over k objects. Keep a sorted index if a test lists over enough blocks
// for its pages to show in its run time.
func (s *Store) List(ctx context.Context, after remote.Cursor, n int) ([]remote.Info, remote.Cursor, bool, error) {
	defer s.enter("list", remote.Name{})()

	if err := s.open("List"); err != nil {
		return nil, after, false, err
	}

	if n <= 0 {
		return nil, after, false, fmt.Errorf("memory: list of %d keys: %w", n, remote.ErrInvalid)
	}

	if after != "" && !strings.HasPrefix(string(after), blockPrefix) {
		return nil, after, false, fmt.Errorf("memory: list after %q, outside %s: %w", after, blockPrefix, remote.ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return nil, after, false, err
	}

	s.ns.mu.Lock()
	defer s.ns.mu.Unlock()

	var keys []string

	for k := range s.ns.objects {
		if strings.HasPrefix(k, blockPrefix) && k > string(after) {
			keys = append(keys, k)
		}
	}

	slices.Sort(keys)

	var page []remote.Info

	next := after

	for _, k := range keys[:min(n, len(keys))] {
		if err := ctx.Err(); err != nil {
			return nil, after, false, err
		}

		next = remote.Cursor(k)

		name, ok := parseName(k[len(blockPrefix):])
		if !ok {
			// decision: a key under blocks/ that is not a name is skipped and
			// counted in the load record alone, with no log line and no
			// series, since the node serves this store only in tests, which
			// have no operator to read either. Withdraw this if the node ever
			// serves the in-memory store outside tests.
			s.ns.load.foreign++
			continue
		}

		o := s.ns.objects[k]
		page = append(page, remote.Info{Name: name, Size: int64(len(o.body)), ModTime: o.modTime})
	}

	return page, next, len(keys) <= n, nil
}

// parseName returns the name s spells as blockKey writes it, lowercase hex of
// exactly a name's length, and false for anything else.
func parseName(s string) (remote.Name, bool) {
	var n remote.Name

	if len(s) != hex.EncodedLen(len(n)) {
		return n, false
	}

	_, err := hex.Decode(n[:], []byte(s))

	return n, err == nil && hex.EncodeToString(n[:]) == s
}

// putRaw stores body under key, relative to the prefix, beneath the contract:
// how a test plants an object the store itself never writes.
func (s *Store) putRaw(key string, body []byte) {
	s.ns.mu.Lock()
	s.ns.objects[key] = object{body: bytes.Clone(body), modTime: s.now()}
	s.ns.mu.Unlock()
}

// reopen returns a second store over s's objects, open even once s is closed,
// as a client opened again over the same namespace is.
func (s *Store) reopen() *Store { return &Store{now: s.now, sum: s.sum, ns: s.ns} }
