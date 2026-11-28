package s3

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/remote"
)

// choice is what the capability check proves about the service: whether it
// proved an integrity mechanism at all, the checksum a put sends (CRC32C,
// Content-MD5, or none, when the store compares the returned ETag), the status
// and code the service rejected that checksum with. The zero choice proves
// nothing.
type choice struct {
	proved   bool
	sum      remote.Checksum
	rejected recorded
}

// store is one namespace's store: a prefix over a client it shares with every
// other namespace of its store configuration (RFC 4 §4.2).
type store struct {
	c      *Client
	prefix string
	choice choice
	closed atomic.Bool
}

var _ remote.Store = (*store)(nil)

// Open opens the store of one namespace, under prefix, on a client it shares.
// It refuses a prefix that is empty or does not end in "/", and any mode but
// remote.Mutable, with remote.ErrInvalid. Until the capability check proves an
// integrity mechanism, Put refuses with remote.ErrInvalid and sends nothing.
func Open(ctx context.Context, c *Client, prefix string, mode remote.Mode) (remote.Store, error) {
	s, err := open(c, prefix, mode, choice{})
	if err != nil {
		return nil, err // not a nil *store, which a caller's nil check would miss
	}

	return s, nil
}

// open is Open over what the capability check proved.
func open(c *Client, prefix string, mode remote.Mode, ch choice) (*store, error) {
	switch err := checkPrefix(prefix); {
	case c == nil:
		return nil, fmt.Errorf("remote/s3: open without a client: %w", remote.ErrInvalid)
	case err != nil:
		return nil, fmt.Errorf("remote/s3: prefix %w: %w", err, remote.ErrInvalid)
	case mode != remote.Mutable:
		return nil, fmt.Errorf("remote/s3: open in mode %d: %w", mode, remote.ErrInvalid)
	}

	return &store{c: c, prefix: prefix, choice: ch}, nil
}

// blockKey is a block's key: the prefix, blocks/ and the name in lowercase
// hex, nothing between them (RFC 4 §4.2, Appendix C.2).
func (s *store) blockKey(n remote.Name) string {
	return s.prefix + "blocks/" + hex.EncodeToString(n[:])
}

// nameFromKey is the name a block key carries, false for any other key: one
// outside the prefix's blocks/, of another length, or not lowercase hex.
func (s *store) nameFromKey(key string) (remote.Name, bool) {
	var n remote.Name

	h, ok := strings.CutPrefix(key, s.prefix+"blocks/")
	if !ok || len(h) != hex.EncodedLen(len(n)) || strings.ContainsFunc(h, func(r rune) bool { return 'A' <= r && r <= 'F' }) {
		return remote.Name{}, false
	}

	if _, err := hex.Decode(n[:], []byte(h)); err != nil {
		return remote.Name{}, false
	}

	return n, true
}

// controlKey is a control object's key under the prefix's control/, false for
// a role outside the closed set (RFC 4 §4.13, Appendix C.2).
func (s *store) controlKey(r remote.Role) (string, bool) {
	switch r {
	case remote.RoleClaim, remote.RoleHealth, remote.RoleKeys:
		return s.prefix + "control/" + r.String(), true
	}

	return "", false
}

// Checksum is the checksum the capability check chose, remote.ChecksumNone
// when the store compares the service's ETag instead.
func (s *store) Checksum() remote.Checksum { return s.choice.sum }

// Close marks the store closed: every later call is refused with
// remote.ErrInvalid, and a second Close returns nil. It releases nothing of
// the client's, whose holder closes it once every store on it has closed.
func (s *store) Close() error {
	s.closed.Store(true)

	return nil
}

// refuseClosed refuses op once the store is closed.
func (s *store) refuseClosed(op opKind) error {
	if s.closed.Load() {
		return fmt.Errorf("remote/s3: %s after Close: %w", op, remote.ErrInvalid)
	}

	return nil
}

// The calls below are not served yet: each refuses with remote.ErrInvalid
// and sends nothing.

func (s *store) Delete(_ context.Context, names []remote.Name) []error {
	errs := make([]error, len(names))
	for i := range errs {
		errs[i] = unserved(opDelete)
	}

	return errs
}

func (s *store) List(_ context.Context, after remote.Cursor, _ int) ([]remote.Info, remote.Cursor, bool, error) {
	return nil, after, false, unserved(opList)
}

func (s *store) Health(context.Context, remote.Direction) error {
	return fmt.Errorf("remote/s3: health is not served: %w", remote.ErrInvalid)
}

func (s *store) PutControl(context.Context, remote.Role, []byte) error {
	return unserved(opPutControl)
}

func (s *store) GetControl(context.Context, remote.Role) ([]byte, remote.Version, error) {
	return nil, "", unserved(opGetControl)
}

// unserved is the refusal of a call the store does not serve yet.
func unserved(op opKind) error {
	return fmt.Errorf("remote/s3: %s is not served: %w", op, remote.ErrInvalid)
}

// record records one call through the client's Recorder: its op, its result
// and cause labels, how long it took and the body bytes it moved.
func (s *store) record(op opKind, err error, took time.Duration, n int64) {
	result, cause := outcome(err)
	s.c.rec.Call(op.String(), result, cause, took, n)
}

// outcome is a call's result and cause labels (RFC 4 §4.12): ok, the
// sentinel's name, or local for a failure that is not the service's (the
// context ended, or the body's reader failed); cause is a denial's cause,
// empty otherwise.
func outcome(err error) (result, cause string) {
	var d *remote.DeniedError

	switch {
	case err == nil:
		return "ok", ""
	case errors.As(err, &d):
		return "denied", d.Cause.String()
	case errors.Is(err, remote.ErrDenied):
		return "denied", ""
	case errors.Is(err, remote.ErrNotFound):
		return "not_found", ""
	case errors.Is(err, remote.ErrInvalid):
		return "invalid", ""
	case errors.Is(err, remote.ErrThrottled):
		return "throttled", ""
	case errors.Is(err, remote.ErrTransient):
		return "transient", ""
	case errors.Is(err, remote.ErrCorrupt):
		return "corrupt", ""
	}

	return "local", ""
}
