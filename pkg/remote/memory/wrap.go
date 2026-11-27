package memory

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/remote"
)

// Wrapper is a fault around the in-memory store: it passes every call through
// to the store, but while read-only it refuses every write and the put probe
// as a namespace its credentials cannot write refuses them, with ErrDenied of
// cause access, without reaching the store. It never answers a call in the
// store's place with a success.
//
// decision: Wrapper writes out every method of remote.Store rather than
// embedding the store, and read-only refuses each write call it lists: Put,
// PutControl, every name of Delete and Health(DirPut). A call added to the
// contract later does not compile here until it is added to that list or
// passed through, since an embedded store would let a new write through while
// read-only.
type Wrapper struct {
	s        *Store
	readOnly atomic.Bool
}

var _ remote.Store = (*Wrapper)(nil)

// Wrap returns a wrapper around s that passes every call through.
func Wrap(s *Store) *Wrapper { return &Wrapper{s: s} }

// SetReadOnly turns the read-only namespace on or off, from the next call on.
func (w *Wrapper) SetReadOnly(on bool) { w.readOnly.Store(on) }

// denied is the refusal of a write while read-only. A closed store refuses
// first, so every call after Close is remote.ErrInvalid, read-only or not.
func (w *Wrapper) denied(call string) error {
	if err := w.s.open(call); err != nil {
		return err
	}

	if w.readOnly.Load() {
		return &remote.DeniedError{Cause: remote.CauseAccess, Code: "AccessDenied"}
	}

	return nil
}

func (w *Wrapper) Checksum() remote.Checksum { return w.s.Checksum() }

func (w *Wrapper) Put(ctx context.Context, name remote.Name, body io.Reader, size int64, sum []byte) error {
	if err := w.denied("Put"); err != nil {
		return err
	}

	return w.s.Put(ctx, name, body, size, sum)
}

func (w *Wrapper) Get(ctx context.Context, name remote.Name, r remote.Range) (io.ReadCloser, remote.StoredSum, error) {
	return w.s.Get(ctx, name, r)
}

func (w *Wrapper) Delete(ctx context.Context, names []remote.Name) []error {
	if err := w.denied("Delete"); err != nil {
		errs := make([]error, len(names))
		for i := range errs {
			errs[i] = err
		}

		return errs
	}

	return w.s.Delete(ctx, names)
}

func (w *Wrapper) List(ctx context.Context, after remote.Cursor, n int) ([]remote.Info, remote.Cursor, bool, error) {
	return w.s.List(ctx, after, n)
}

func (w *Wrapper) Health(ctx context.Context, d remote.Direction) error {
	if d == remote.DirPut {
		if err := w.denied("Health"); err != nil {
			return err
		}
	}

	return w.s.Health(ctx, d)
}

func (w *Wrapper) PutControl(ctx context.Context, role remote.Role, body []byte) error {
	if err := w.denied("PutControl"); err != nil {
		return err
	}

	return w.s.PutControl(ctx, role, body)
}

func (w *Wrapper) GetControl(ctx context.Context, role remote.Role) ([]byte, remote.Version, error) {
	return w.s.GetControl(ctx, role)
}

func (w *Wrapper) Close() error { return w.s.Close() }
