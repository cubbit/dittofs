package s3

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/pkg/remote"
)

// Put streams one whole block of exactly size bytes in one PutObject under the
// name's key, in the client's storage class, verified in transit by the one
// mechanism the capability check proved: sum sent as x-amz-checksum-crc32c or
// Content-MD5, or, when Checksum is remote.ChecksumNone, the body's MD5 taken
// while streaming and compared with the returned ETag (RFC 4 §4.3, Appendix
// C.3). It returns nil only once the service acknowledged the whole body and
// any ETag matched. A put over an existing name replaces it, and no request
// carries a condition.
//
// Before any request it refuses with remote.ErrInvalid: a closed store, a store
// whose check proved no mechanism, a negative size, and, when Checksum is not
// None, a sum missing or of the wrong length; with None, sum is not read. A
// stream shorter or longer than size is remote.ErrInvalid and the service
// stores nothing; a reader that fails, or ctx ending, comes back as itself.
func (s *store) Put(ctx context.Context, name remote.Name, body io.Reader, size int64, sum []byte) error {
	start := time.Now()
	n, err := s.put(ctx, name, body, size, sum)
	s.record(opPut, err, time.Since(start), n)

	if errors.Is(err, remote.ErrCorrupt) {
		s.c.log.Warn("remote put corrupt in transit", "block", hex.EncodeToString(name[:]), "err", err)
	}

	return err
}

// put is Put, returning the body bytes the request read.
func (s *store) put(ctx context.Context, name remote.Name, body io.Reader, size int64, sum []byte) (int64, error) {
	if err := s.refuseClosed(opPut); err != nil {
		return 0, err
	}

	if !s.choice.proved {
		return 0, fmt.Errorf("remote/s3: put before the capability check proved an integrity mechanism: %w", remote.ErrInvalid)
	}

	if size < 0 {
		return 0, fmt.Errorf("remote/s3: put of %d bytes: %w", size, remote.ErrInvalid)
	}

	key := s.blockKey(name)
	in := &s3.PutObjectInput{
		Bucket:        &s.c.cfg.Bucket,
		Key:           &key,
		ContentLength: &size,
		StorageClass:  types.StorageClass(s.c.cfg.StorageClass),
	}

	g := &guard{src: body, left: size}

	// rec is the check's recorded rejection, matched only on a put that
	// carried the checksum it was recorded for.
	var rec recorded

	switch s.choice.sum {
	case remote.ChecksumCRC32C:
		if len(sum) != 4 {
			return 0, fmt.Errorf("remote/s3: put with a %d-byte CRC32C: %w", len(sum), remote.ErrInvalid)
		}

		in.ChecksumCRC32C = new(base64.StdEncoding.EncodeToString(sum))
		rec = s.choice.rejected
	case remote.ChecksumMD5:
		if len(sum) != md5.Size {
			return 0, fmt.Errorf("remote/s3: put with a %d-byte MD5: %w", len(sum), remote.ErrInvalid)
		}

		in.ContentMD5 = new(base64.StdEncoding.EncodeToString(sum))
		rec = s.choice.rejected
	default:
		g.md5 = md5.New()
	}

	in.Body = g

	out, err := s.c.api.PutObject(ctx, in)
	read, local := g.stop()

	if err := fromSDK(ctx, opPut, rec, local, err); err != nil {
		return read, err
	}

	if g.md5 != nil {
		// decision: the ETag is compared after the service stored the object,
		// so a delayed duplicate of this attempt's put, one the client gave up
		// on and the service applies after a later retry succeeded, can store
		// bytes corrupted in transit that nothing compares; reads then report
		// the chunk corrupt. The block is committed only after a put of it
		// succeeded, so a corrupt retry the comparison catches never reaches a
		// committed block. Withdraw this exemption, by refusing services that
		// verify only the digest, if one is ever seen applying a stale put.
		if !etagIsMD5(out.ETag, g.md5.Sum(nil)) {
			return read, fmt.Errorf("remote/s3: put: the ETag is not the MD5 of the body sent: %w", remote.ErrCorrupt)
		}
	}

	return read, nil
}

// etagIsMD5 reports whether etag, as the service returned it, is the hex of
// sum: quoted or not, in either case. A multipart ETag, which ends in a part
// count, is never one.
func etagIsMD5(etag *string, sum []byte) bool {
	if etag == nil {
		return false
	}

	e := strings.TrimSuffix(strings.TrimPrefix(*etag, `"`), `"`)

	return strings.EqualFold(e, hex.EncodeToString(sum))
}

// guard is a put's body: it reads src as a stream of exactly left bytes and
// ends the request before the service receives a complete body when src is
// shorter or longer. It holds back the final byte until one more read of src
// returns its end, since net/http sends all of ContentLength before noticing a
// longer body. It records src's own error, or its refusal, for fromSDK, since
// the request's failure comes back through net/http as a transport error.
//
// ponytail: the held-back byte costs one extra read of src per put. Move the
// guard into the codec's encoder, which knows where its stream ends, if a
// profile ever shows that read.
type guard struct {
	src io.Reader
	md5 hash.Hash // the body's MD5 when the store compares the ETag; nil otherwise

	// mu is held across each read of src, so stop waits out a read net/http's
	// writer still has in flight: src is never read once Put has returned.
	mu      sync.Mutex
	left    int64 // bytes still to deliver
	read    int64 // bytes delivered
	err     error // src's error, or the refusal; every later read returns it
	ended   bool  // the whole body was delivered and src's end seen
	stopped bool
	tail    [2]byte
}

func (g *guard) Read(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	switch {
	case g.stopped:
		return 0, errors.New("remote/s3: put body read after the put returned")
	case g.err != nil:
		return 0, g.err
	case g.ended:
		return 0, io.EOF
	case len(p) == 0:
		return 0, nil
	case g.left > 1:
		n, err := g.src.Read(p[:min(int64(len(p)), g.left-1)])
		g.deliver(p[:n])

		switch {
		case err == io.EOF:
			// At most left-1 bytes were asked for, so at least one is missing.
			g.err = fmt.Errorf("remote/s3: put body ended %d bytes short of its size: %w", g.left, remote.ErrInvalid)
		case err != nil:
			g.err = err
		}

		if g.err != nil {
			return n, g.err
		}

		return n, nil
	}

	// The last byte, if any, and one read past it, which must find the end.
	want := int(g.left) + 1
	m, err := io.ReadAtLeast(g.src, g.tail[:want], want)

	switch {
	case m == want:
		g.err = fmt.Errorf("remote/s3: put body is longer than its size: %w", remote.ErrInvalid)
	case m == want-1 && (err == io.EOF || err == io.ErrUnexpectedEOF):
		g.ended = true
		if m == 1 {
			p[0] = g.tail[0]
			g.deliver(p[:1])
		}

		return m, io.EOF
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		g.err = fmt.Errorf("remote/s3: put body ended %d bytes short of its size: %w", g.left, remote.ErrInvalid)
	default:
		g.err = err
	}

	return 0, g.err
}

// deliver counts b as sent and folds it into the MD5.
func (g *guard) deliver(b []byte) {
	g.left -= int64(len(b))
	g.read += int64(len(b))

	if g.md5 != nil {
		g.md5.Write(b)
	}
}

// stop ends every later read of src and returns the bytes delivered and what
// the guard recorded.
func (g *guard) stop() (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.stopped = true

	return g.read, g.err
}
