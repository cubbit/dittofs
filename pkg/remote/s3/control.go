package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"

	"github.com/cubbit/dittofs/pkg/remote"
)

// maxControl is the largest control object body the store puts or reads: RFC
// 3's small-body cap, so a control call always fits one small transfer.
//
// ponytail: the keys object grows with each rotation and stops at this cap,
// about 1,300 key records (580 at the longest IDs), where PutControl turns
// ErrInvalid. Raise it, which migrates nothing, before a namespace nears it,
// and move the keys object's put off the small-body path past RFC 3's cap.
const maxControl = 256 << 10

// PutControl stores body as role's control object at <prefix>control/<role>,
// replacing the whole object, in one PutObject verified in transit like a
// block's put (RFC 4 §4.13). A body over 256 KiB, or a role outside the closed
// set, is remote.ErrInvalid with no request.
func (s *store) PutControl(ctx context.Context, role remote.Role, body []byte) error {
	start := time.Now()
	n, err := s.putControl(ctx, role, body)
	s.record(opPutControl, err, time.Since(start), n)

	if errors.Is(err, remote.ErrCorrupt) {
		s.c.log.Warn("remote control put corrupt in transit", "role", role.String(), "err", err)
	}

	return err
}

func (s *store) putControl(ctx context.Context, role remote.Role, body []byte) (int64, error) {
	key, ok := s.controlKey(role)
	if !ok {
		return 0, fmt.Errorf("remote/s3: %s of %v: %w", opPutControl, role, remote.ErrInvalid)
	}

	if len(body) > maxControl {
		return 0, fmt.Errorf("remote/s3: %s of %v, %d bytes over %d: %w", opPutControl, role, len(body), maxControl, remote.ErrInvalid)
	}

	// The sum the check's mechanism sends, which put refuses on a closed
	// store; none when the store compares the ETag, taken while streaming.
	var sum []byte

	switch s.choice.sum {
	case remote.ChecksumCRC32C:
		sum = binary.BigEndian.AppendUint32(nil, crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli)))
	case remote.ChecksumMD5:
		m := md5.Sum(body)
		sum = m[:]
	}

	return s.put(ctx, opPutControl, key, bytes.NewReader(body), int64(len(body)), sum)
}

// GetControl returns role's whole control object, read into memory, and the
// empty Version, since the store is mutable (RFC 4 §4.13). An absent object is
// remote.ErrNotFound; a role outside the closed set is remote.ErrInvalid with
// no request, and an object the service declares longer than 256 KiB is
// remote.ErrInvalid before any byte is read.
func (s *store) GetControl(ctx context.Context, role remote.Role) ([]byte, remote.Version, error) {
	body, err := s.getControl(ctx, role)

	return body, "", err
}

// getControl reads role's object through the whole-block get's reader, which
// records the call at its end; a call that fails before the reader exists is
// recorded here.
func (s *store) getControl(ctx context.Context, role remote.Role) ([]byte, error) {
	start := time.Now()
	fail := func(err error) ([]byte, error) {
		s.record(opGetControl, err, time.Since(start), 0)
		return nil, err
	}

	key, ok := s.controlKey(role)
	if !ok {
		return fail(fmt.Errorf("remote/s3: %s of %v: %w", opGetControl, role, remote.ErrInvalid))
	}

	b, _, err := s.getRequest(ctx, opGetControl, key, remote.Range{})
	if err != nil {
		return fail(err)
	}

	// decision: the cap is judged from the declared length, before any byte
	// is read, so the refusal is recorded as one rather than as a read cut
	// short. A body longer than it declares fails one byte past it, and a
	// reply that declares no length is refused, both ErrTransient as the
	// whole-block get has them, never ErrInvalid. Read such replies through a
	// limit of the cap and one byte if a service is ever seen sending control
	// objects without a length.
	if b.left > maxControl {
		_ = b.rc.Close()
		return fail(fmt.Errorf("remote/s3: %s of %v, %d bytes over %d: %w", opGetControl, role, b.left, maxControl, remote.ErrInvalid))
	}

	b.start = start

	body, err := io.ReadAll(b)
	_ = b.Close() // ReadAll read to the reader's end, so its error is err

	if err != nil {
		return nil, err
	}

	return body, nil
}
