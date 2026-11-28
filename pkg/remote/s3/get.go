package s3

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/cubbit/dittofs/pkg/remote"
)

// Get returns exactly the bytes of r of the name's block, or the whole block
// for the zero Range, in one GetObject (RFC 4 §4.4). A range of length 0, or
// one before the block's start, is remote.ErrInvalid with no request; one the
// service answers with a Content-Range other than the one asked for (a range
// past the end, clamped), or with the whole object, is remote.ErrInvalid
// before any byte is yielded. The reader yields exactly the bytes asked for, or
// fails: a body that ends short of them is remote.ErrTransient where it ends.
// A whole-block get asks for the service's checksum and checks it once the
// last byte is read, as it checks the ETag when the store compares ETags; a
// mismatch is remote.ErrTransient at the reader's end, and voids every byte
// it yielded; the reader's Close returns that failure too, for a caller that
// read exactly the length it knew. stored is the checksum the service holds
// for the whole block, with its kind, or kind None; a ranged get reports none.
func (s *store) Get(ctx context.Context, name remote.Name, r remote.Range) (io.ReadCloser, remote.StoredSum, error) {
	return s.get(ctx, opGet, s.blockKey(name), r)
}

// get is a get of op under key. The call is recorded once: here when it
// fails, otherwise by the reader, at its end or its Close.
func (s *store) get(ctx context.Context, op opKind, key string, r remote.Range) (io.ReadCloser, remote.StoredSum, error) {
	start := time.Now()

	body, sum, err := s.getRequest(ctx, op, key, r)
	if err != nil {
		s.record(op, err, time.Since(start), 0)
		return nil, remote.StoredSum{}, err
	}

	body.start = start

	return body, sum, nil
}

// getRequest sends the GetObject and judges its reply's headers.
func (s *store) getRequest(ctx context.Context, op opKind, key string, r remote.Range) (*getBody, remote.StoredSum, error) {
	if err := s.refuseClosed(op); err != nil {
		return nil, remote.StoredSum{}, err
	}

	whole := r == remote.Range{}
	if !whole && (r.Off < 0 || r.Len <= 0 || r.Off > math.MaxInt64-r.Len) {
		return nil, remote.StoredSum{}, fmt.Errorf("remote/s3: %s of %d bytes at %d: %w", op, r.Len, r.Off, remote.ErrInvalid)
	}

	in := &s3.GetObjectInput{Bucket: &s.c.cfg.Bucket, Key: &key}
	if whole {
		in.ChecksumMode = types.ChecksumModeEnabled
	} else {
		in.Range = new(fmt.Sprintf("bytes=%d-%d", r.Off, r.Off+r.Len-1))
	}

	out, err := s.c.api.GetObject(ctx, in)
	if err := fromSDK(ctx, op, recorded{}, nil, err); err != nil {
		return nil, remote.StoredSum{}, err
	}

	b := &getBody{s: s, op: op, ctx: ctx, rc: out.Body}

	if !whole {
		off, n, _, ok := parseContentRange(aws.ToString(out.ContentRange))
		raw, _ := awsmiddleware.GetRawResponse(out.ResultMetadata).(*smithyhttp.Response)
		if !ok || off != r.Off || n != r.Len || raw == nil || raw.StatusCode != http.StatusPartialContent {
			// A 200, or no Content-Range, is the whole object, a service
			// ignoring Range; any other range is one past the end the service
			// clamped.
			_ = out.Body.Close()
			return nil, remote.StoredSum{}, fmt.Errorf("remote/s3: %s of %d bytes at %d answered with another range, or the whole object: %w", op, r.Len, r.Off, remote.ErrInvalid)
		}

		b.left = r.Len

		return b, remote.StoredSum{}, nil
	}

	if out.ContentLength == nil {
		// decision: a whole block's reply that declares no length, sent
		// chunked, cannot be counted, and is ErrTransient, as a reply that
		// failed in transit; the services of the profile send one. Make it
		// ErrInvalid, which no retry clears, if a service or a proxy before it
		// is ever seen answering every get without one.
		_ = out.Body.Close()
		return nil, remote.StoredSum{}, fmt.Errorf("remote/s3: %s: the reply declares no length: %w", op, remote.ErrTransient)
	}

	b.left = *out.ContentLength

	var sum remote.StoredSum
	if crc, err := base64.StdEncoding.DecodeString(aws.ToString(out.ChecksumCRC32C)); err == nil && len(crc) == 4 {
		// The SDK checks it at the body's end, since ChecksumMode is enabled.
		sum = remote.StoredSum{Kind: remote.ChecksumCRC32C, Sum: crc}
	}

	if s.choice.proved && s.choice.sum == remote.ChecksumNone {
		// The check proved the ETag the MD5 of the stored bytes; the store
		// checks it itself.
		//
		// decision: an ETag that is no MD5 (weak, multipart, absent) skips the
		// check and reports no stored sum, as RFC 4 leaves a get without a
		// returned checksum unchecked in transit; the codec's hash check still
		// decides every read it decodes. A block is put in one request, so such
		// an ETag means the service changed how it computes them: refuse it as
		// ErrTransient if one is ever seen on a store the check proved.
		if etag, ok := etagMD5(out.ETag); ok {
			b.md5, b.etag = md5.New(), etag
			if sum.Kind == remote.ChecksumNone {
				sum = remote.StoredSum{Kind: remote.ChecksumMD5, Sum: etag}
			}
		}
	}

	return b, sum, nil
}

// etagMD5 is the MD5 an ETag holds, quoted or not, in either case; false for
// one that is not 32 hex digits, a multipart ETag among them.
func etagMD5(etag *string) ([]byte, bool) {
	b, err := hex.DecodeString(strings.TrimSuffix(strings.TrimPrefix(aws.ToString(etag), `"`), `"`))
	if err != nil || len(b) != md5.Size {
		return nil, false
	}

	return b, true
}

// getBody is a get's reader: it yields at most left bytes, fails short of
// them, reads on to the reply's end so the SDK's checksum check runs, and
// checks the ETag's MD5 when it has one. Its first error, or io.EOF, is
// returned by every later read. It records the call once, at its end or at
// Close, whichever comes first. It keeps the call's ctx, which lives exactly as
// long as the request, to tell the context ending from a failed read. Like
// the reply body it reads, it is for one goroutine: Read and Close are not
// called at once.
type getBody struct {
	s     *store
	op    opKind
	ctx   context.Context
	rc    io.ReadCloser
	start time.Time

	left int64     // bytes still to yield
	read int64     // bytes yielded
	md5  hash.Hash // of the bytes yielded, when etag is set
	etag []byte

	err      error
	recorded bool
}

func (b *getBody) Read(p []byte) (int, error) {
	switch {
	case b.err != nil:
		return 0, b.err
	case len(p) == 0:
		return 0, nil
	}

	var (
		n      int
		err    error
		longer bool
	)

	if b.left > 0 {
		n, err = b.rc.Read(p[:min(int64(len(p)), b.left)])
		b.left -= int64(n)
		b.read += int64(n)

		if b.md5 != nil {
			b.md5.Write(p[:n])
		}
	}

	if err == nil && b.left == 0 {
		// Every byte was yielded: in this same read, so that a caller reading
		// exactly the length it knows still gets the checks, one more read of
		// the reply must find its end, where the SDK reports its checksum's
		// mismatch.
		var one [1]byte
		var m int
		m, err = b.rc.Read(one[:])
		longer = m > 0
	}

	switch {
	case longer:
		b.end(fmt.Errorf("remote/s3: %s: the body is longer than its declared length: %w", b.op, remote.ErrTransient))
	case err == io.EOF && b.left > 0:
		b.end(fmt.Errorf("remote/s3: %s: the body ended %d bytes short: %w", b.op, b.left, remote.ErrTransient))
	case err == io.EOF:
		b.end(b.verify())
	case err != nil:
		b.end(b.failed())
	}

	return n, b.err
}

// verify is the reader's end once every byte was read and the reply ended:
// io.EOF, or ErrTransient when the ETag's MD5 differs.
func (b *getBody) verify() error {
	if b.md5 != nil && string(b.md5.Sum(nil)) != string(b.etag) {
		return fmt.Errorf("remote/s3: %s: the body is not the ETag's MD5: %w", b.op, remote.ErrTransient)
	}

	return io.EOF
}

// failed is a failure reading the reply: the context's error once it has
// ended, otherwise ErrTransient, the SDK's checksum mismatch among them, whose
// error is never wrapped.
func (b *getBody) failed() error {
	if err := b.ctx.Err(); err != nil {
		return err
	}

	return fmt.Errorf("remote/s3: %s: reading the body failed: %w", b.op, remote.ErrTransient)
}

// end records err as the reader's end and records the call.
func (b *getBody) end(err error) {
	b.err = err
	b.record()
}

func (b *getBody) record() {
	if b.recorded {
		return
	}

	b.recorded = true

	err := b.err
	if err == io.EOF {
		err = nil
	}

	b.s.record(b.op, err, time.Since(b.start), b.read)
}

// Close closes the reply's body and returns the failure the reader ended
// with, if any, so that a caller which read exactly the length it knew, and
// so never saw an error its last read returned with its last bytes (as
// io.ReadFull drops one), still learns that the response is void.
//
// decision: a reader closed before its end records the call ok, with the
// bytes it yielded, since the caller chose to stop and the service failed
// nothing; the checksum of a whole block read so is never checked, which the
// caller that stopped did not need. Record such a call apart if a caller is
// ever seen stopping early to hide a failure.
func (b *getBody) Close() error {
	b.record()

	end := b.err
	if b.err == nil {
		b.err = fmt.Errorf("remote/s3: %s: read after Close: %w", b.op, remote.ErrInvalid)
	}

	if err := b.rc.Close(); end == nil || end == io.EOF {
		return err
	}

	return end
}

// parseContentRange reads a Content-Range of the form "bytes first-last/total",
// as RFC 9110 §14.4 writes a satisfied range: off is first, n the range's
// length, total the object's; S3 always sends the total, so a range of an
// unknown one ("bytes 0-9/*") is refused like any other form. ok is false for
// anything else: an unsatisfied
// range ("bytes */total"), an unknown total, last before first or not before
// total, a sign, a space or a leading zero in a number, and a value past int64.
func parseContentRange(h string) (off, n, total int64, ok bool) {
	rest, found := strings.CutPrefix(h, "bytes ")
	if !found {
		return 0, 0, 0, false
	}

	span, t, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, 0, false
	}

	f, l, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, 0, false
	}

	first, ok1 := decimal(f)
	last, ok2 := decimal(l)
	total, ok3 := decimal(t)

	if !ok1 || !ok2 || !ok3 || last < first || last >= total {
		return 0, 0, 0, false
	}

	return first, last - first + 1, total, true
}

// decimal is s as a non-negative int64 written in its one canonical form:
// digits only, no leading zero but in "0".
func decimal(s string) (int64, bool) {
	if s == "" || len(s) > 1 && s[0] == '0' || strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, false
	}

	v, err := strconv.ParseInt(s, 10, 64)

	return v, err == nil
}
