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
// and code the service rejected that checksum with, and whether the service
// has a batch delete. The zero choice proves nothing, and deletes one name per
// request.
type choice struct {
	proved   bool
	sum      remote.Checksum
	rejected recorded
	batch    bool
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

// Claim is the namespace claim the open read (RFC 4 §4.11, RFC 27 §2.1).
// Err is nil when Body holds it, remote.ErrNotFound when none is stored,
// and any other error when it could not be read; none of them fails the open.
type Claim struct {
	Body []byte
	Err  error
}

// Open opens the store of one namespace, under prefix, on a client it shares,
// once the capability check (RFC 4 §4.11, Appendix C.4) has proved the
// service: it returns the store, holding its health object, and the claim the
// check read. A service the check finds lacking is refused with
// remote.ErrInvalid naming every missing feature, logged at Error once; a
// call of the check that fails rather than answers fails the open with its
// own error, for the caller to retry. Every check that ran to its end, refused
// or not, records each feature's verdict through the Recorder. A prefix that
// is empty or does not end in "/", and any mode but remote.Mutable, are
// refused with remote.ErrInvalid before any request.
func Open(ctx context.Context, c *Client, prefix string, mode remote.Mode) (remote.Store, Claim, error) {
	if mode == remote.Immutable {
		// decision: an immutable store is refused at open until the backup
		// location's calls land with its copier (RFC 26), since none of what
		// such a store must check, keep or refuse is built. Withdraw the
		// refusal with those calls.
		return nil, Claim{}, fmt.Errorf("remote/s3: immutable mode waits for the backup location's copier (RFC 26): %w", remote.ErrInvalid)
	}

	s, err := open(c, prefix, mode, choice{})
	if err != nil {
		return nil, Claim{}, err
	}

	claim, v, err := check(ctx, s)
	if err != nil {
		return nil, Claim{}, err
	}

	for f := range featureCount {
		c.rec.Capability(prefix, f.String(), v[f] == statePresent)
	}

	if missing := v.refusals(); missing != "" {
		c.log.Error("remote open refused", "prefix", prefix, "missing", missing)
		return nil, Claim{}, &refusedError{missing}
	}

	ch := s.choice
	c.log.Info("remote store opened", "prefix", prefix,
		"mechanism", mechanism(ch.sum), "rejection_status", ch.rejected.status, "rejection_code", ch.rejected.code,
		"batch_delete", ch.batch, "storage_class", c.cfg.StorageClass, "failure_domain", failureDomain(c.cfg.StorageClass))

	return s, claim, nil
}

// mechanism names an integrity mechanism for the open's log line.
func mechanism(sum remote.Checksum) string {
	switch sum {
	case remote.ChecksumCRC32C:
		return "crc32c"
	case remote.ChecksumMD5:
		return "content-md5"
	}

	return "etag"
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

func (s *store) Health(context.Context, remote.Direction) error {
	return fmt.Errorf("remote/s3: health is not served: %w", remote.ErrInvalid)
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
