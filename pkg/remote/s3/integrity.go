package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/pkg/remote"
)

// checkBody is the small object step 1 puts, and the known body of its ETag
// proof.
var checkBody = []byte("dittofs capability check\n")

// proveIntegrity is the capability check's step 1 (RFC 4 Appendix C.4), in
// the check namespace ns, over the SDK client of c: for x-amz-checksum-crc32c,
// then Content-MD5, a small put with the correct value must succeed and one
// with a wrong value must be refused with nothing stored; the first that
// passes both is chosen, with the status and code of its refusal recorded.
// If neither does, a known body put with a wrong Content-MD5 must answer an
// ETag that is the MD5 of the body sent, and the store compares the ETag.
// The chosen mechanism is then proved again at largest bytes, the largest
// encoded block the store will be put, with no fallback to another.
//
// It returns the choice and a verdict on put_integrity, with nothing proved
// when that is missing. A call that fails rather than answers, a refused
// credential, a 5xx or a throttle among them, fails the check with the call's
// error as fromSDK maps it. largest is the bound the store's Put enforces,
// the client's largest block; below 1 it is refused with remote.ErrInvalid.
func proveIntegrity(ctx context.Context, c *Client, ns string, largest int64) (choice, verdict, error) {
	if largest < 1 {
		return choice{}, verdict{}, fmt.Errorf("remote/s3: integrity proof at %d bytes: %w", largest, remote.ErrInvalid)
	}

	var v verdict

	ch, ok, err := smallProof(ctx, c, ns)
	if err == nil && ok {
		ok, err = largeProof(ctx, c, ns+"integrity/large", ch, largest)
	}

	if err != nil {
		return choice{}, verdict{}, err
	}

	v.judge(featPutIntegrity, ok)
	if !ok {
		return choice{}, v, nil
	}

	return ch, v, nil
}

// smallProof tries each checksum header in turn on checkBody, then the ETag.
func smallProof(ctx context.Context, c *Client, ns string) (choice, bool, error) {
	n := int64(len(checkBody))

	for _, m := range []struct {
		sum  remote.Checksum
		name string
	}{{remote.ChecksumCRC32C, "crc32c"}, {remote.ChecksumMD5, "md5"}} {
		key, right := ns+"integrity/"+m.name, sumOf(m.sum, bytes.NewReader(checkBody))

		_, refused, err := putSum(ctx, c, key, bytes.NewReader(checkBody), n, m.sum, right)
		if err != nil {
			return choice{}, false, err
		}

		if refused != nil {
			continue // the service refuses even the correct value
		}

		refused, err = refusesWrong(ctx, c, key+"-wrong", bytes.NewReader(checkBody), n, m.sum, right)
		if err != nil {
			return choice{}, false, err
		}

		if refused != nil {
			status, code := reply(refused)

			return choice{proved: true, sum: m.sum, rejected: recorded{status, code}}, true, nil
		}
	}

	right := sumOf(remote.ChecksumMD5, bytes.NewReader(checkBody))

	ok, err := etagIsMD5Of(ctx, c, ns+"integrity/etag", bytes.NewReader(checkBody), n, right)
	if err != nil || !ok {
		return choice{}, false, err
	}

	return choice{proved: true, sum: remote.ChecksumNone}, true, nil
}

// largeProof proves ch again at key with size bytes of blockOf: a correct
// put, then one with a wrong checksum, refused with nothing stored, that the
// store's own reading of the refusal, the recorded rejection included, reads
// as ErrCorrupt; under the ETag, one put with no checksum, as the store's
// puts are sent, whose ETag must be the MD5 of the bytes sent.
//
// decision: the large refusal must read as ErrCorrupt under the code the small
// one recorded, not be recorded afresh, since a service that refuses a large
// block under another code would see every corrupt block put come back
// ErrInvalid and never retried. The cost is refusing such a service outright;
// record a code per size if one is ever measured.
//
// ponytail: two puts of the largest block per open, one under the ETag, a few
// MiB at the defaults. Prove once per client if opens of many namespaces on one
// configuration show it.
func largeProof(ctx context.Context, c *Client, key string, ch choice, size int64) (bool, error) {
	if ch.sum == remote.ChecksumNone {
		etag, refused, err := putSum(ctx, c, key, blockOf(size), size, remote.ChecksumNone, nil)
		if err != nil || refused != nil {
			return false, err
		}

		return etagIsMD5(etag, sumOf(remote.ChecksumMD5, blockOf(size))), nil
	}

	right := sumOf(ch.sum, blockOf(size))

	_, refused, err := putSum(ctx, c, key, blockOf(size), size, ch.sum, right)
	if err != nil || refused != nil {
		return false, err
	}

	refused, err = refusesWrong(ctx, c, key+"-wrong", blockOf(size), size, ch.sum, right)
	if err != nil || refused == nil {
		return false, err
	}

	return errors.Is(fromSDK(ctx, opPut, ch.rejected, nil, refused), remote.ErrCorrupt), nil
}

// putSum puts size bytes of body at key in the client's class, with sum under
// mech's header, none for remote.ChecksumNone. A reply the table reads as
// ErrInvalid or ErrCorrupt is the service's verdict on the put, returned as
// refused, the SDK's error; any other failure is err, as fromSDK maps it.
func putSum(ctx context.Context, c *Client, key string, body io.Reader, size int64, mech remote.Checksum, sum []byte) (etag *string, refused, err error) {
	in := &s3.PutObjectInput{
		Bucket:        &c.cfg.Bucket,
		Key:           &key,
		Body:          body,
		ContentLength: &size,
		StorageClass:  types.StorageClass(c.cfg.StorageClass),
	}

	switch mech {
	case remote.ChecksumCRC32C:
		in.ChecksumCRC32C = new(base64.StdEncoding.EncodeToString(sum))
	case remote.ChecksumMD5:
		in.ContentMD5 = new(base64.StdEncoding.EncodeToString(sum))
	}

	out, sdkErr := c.api.PutObject(ctx, in)
	if sdkErr == nil {
		return out.ETag, nil, nil
	}

	// decision: only a reply the table reads as ErrInvalid or ErrCorrupt is a
	// verdict on the checksum. A 401, a 403, a 3xx, a size or quota refusal, a
	// throttle and a 5xx fail the check instead of being recorded as the
	// checksum's rejection, which would turn every later denial of a put into
	// ErrCorrupt, retried for ever. The price: a service that refuses a wrong
	// checksum only with one of those fails every open rather than falling
	// through to the next mechanism; widen the set if one is ever measured.
	e := fromSDK(ctx, opPut, recorded{}, nil, sdkErr)
	if errors.Is(e, remote.ErrInvalid) || errors.Is(e, remote.ErrCorrupt) {
		return nil, sdkErr, nil
	}

	return nil, nil, e
}

// refusesWrong puts body at key with sum altered under mech, and returns the
// SDK's error refusing it; nil when the service stored it, or refused it and
// stored it anyway.
func refusesWrong(ctx context.Context, c *Client, key string, body io.Reader, size int64, mech remote.Checksum, sum []byte) (error, error) {
	_, refused, err := putSum(ctx, c, key, body, size, mech, wrong(sum))
	if err != nil || refused == nil {
		return nil, err
	}

	stored, err := holds(ctx, c, key)
	if err != nil || stored {
		return nil, err
	}

	return refused, nil
}

// etagIsMD5Of puts body at key with a wrong Content-MD5 and reports whether
// the service stored it and answered right, the MD5 of the bytes sent: the
// ETag is computed from the bytes, not echoed from the header.
func etagIsMD5Of(ctx context.Context, c *Client, key string, body io.Reader, size int64, right []byte) (bool, error) {
	etag, refused, err := putSum(ctx, c, key, body, size, remote.ChecksumMD5, wrong(right))
	if err != nil || refused != nil {
		return false, err
	}

	return etagIsMD5(etag, right), nil
}

// holds reports whether the service holds an object at key, reading its first
// byte only.
//
// decision: only 404 NoSuchKey reads as nothing stored; any other answer, a
// 403 from a credential without the list right among them, fails the check
// with the get's error, since without it nothing shows the refused put left
// nothing behind. Such a credential is then refused as denied at step 1, not
// named as list_bucket at step 2. Name it there instead, by treating a 403
// here as the mechanism unproved, if operators are seen misreading the denial.
func holds(ctx context.Context, c *Client, key string) (bool, error) {
	out, err := c.api.GetObject(ctx, &s3.GetObjectInput{Bucket: &c.cfg.Bucket, Key: &key, Range: aws.String("bytes=0-0")})
	if err == nil {
		_ = out.Body.Close()
		return true, nil
	}

	if e := fromSDK(ctx, opGet, recorded{}, nil, err); !errors.Is(e, remote.ErrNotFound) {
		return false, e
	}

	return false, nil
}

// sumOf is r's raw digest under mech, CRC32C or MD5, read once without
// holding it.
func sumOf(mech remote.Checksum, r io.Reader) []byte {
	h := hash.Hash(md5.New())
	if mech == remote.ChecksumCRC32C {
		h = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	}

	_, _ = io.Copy(h, r)

	return h.Sum(nil)
}

// wrong is sum with its first byte changed.
func wrong(sum []byte) []byte {
	w := bytes.Clone(sum)
	w[0] ^= 0xff

	return w
}

// blockOf is a stream of size bytes, the same on every call, that allocates
// nothing per read, so the largest block is hashed and then put without ever
// being held.
func blockOf(size int64) io.Reader { return &block{left: size, x: 0x9e3779b97f4a7c15} }

type block struct {
	left int64
	x    uint64 // xorshift64 state
}

func (b *block) Read(p []byte) (int, error) {
	if b.left == 0 {
		return 0, io.EOF
	}

	n := int(min(int64(len(p)), b.left))
	for i := range p[:n] {
		b.x ^= b.x << 13
		b.x ^= b.x >> 7
		b.x ^= b.x << 17
		p[i] = byte(b.x)
	}

	b.left -= int64(n)

	return n, nil
}
