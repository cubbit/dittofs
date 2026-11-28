//go:build integration

package s3fault

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// checksumHeaders are every header a put's checksum travels in.
var checksumHeaders = []string{
	"X-Amz-Sdk-Checksum-Algorithm", "X-Amz-Checksum-Crc32", "X-Amz-Checksum-Crc32c",
	"X-Amz-Checksum-Crc64nvme", "X-Amz-Checksum-Sha1", "X-Amz-Checksum-Sha256",
}

func crc32c(b []byte) string {
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))))
}

func contentMD5(b []byte) string {
	sum := md5.Sum(b)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// refusal is the status and code of a refused call; 0 and "" when it passed.
func refusal(err error) (int, string) {
	var re *awshttp.ResponseError
	if !errors.As(err, &re) {
		return 0, ""
	}
	return re.HTTPStatusCode(), code(err)
}

// get reads key through the transport, with rng as its Range when set, and
// returns the reply's status, its body and its Content-Range.
func (r *rig) get(t *testing.T, key, rng string) (status int, body []byte, contentRange string) {
	t.Helper()
	in := &s3.GetObjectInput{Bucket: &r.bucket, Key: &key}
	if rng != "" {
		in.Range = &rng
	}
	out, err := r.client.GetObject(t.Context(), in)
	if err != nil {
		t.Fatalf("GetObject %s %s: %v", key, rng, err)
	}
	defer func() { _ = out.Body.Close() }()
	body, err = io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("reading %s %s: %v", key, rng, err)
	}
	raw, _ := awsmiddleware.GetRawResponse(out.ResultMetadata).(*smithyhttp.Response)
	if raw == nil {
		t.Fatalf("GetObject %s %s: no raw response in its metadata", key, rng)
	}
	return raw.StatusCode, body, aws.ToString(out.ContentRange)
}

// flipped is body with the byte at 5 inverted.
func flipped(body []byte) []byte {
	want := bytes.Clone(body)
	want[5] ^= 0xff
	return want
}

// A flip changes the one byte it names, on the side it acts on: a request's
// body when it has one, which the service refuses when a checksum covers it
// and stores flipped when none does, and otherwise the reply's.
func TestFlipByteChangesOneByteOnItsSide(t *testing.T) {
	r := newRig(t, nil)
	body := bytes.Repeat([]byte("0123456789"), 10)

	r.tr.Inject(Rule{Op: "PutObject", KeyPrefix: "checked", Times: 1}, FlipByte(5))
	_, err := r.client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: aws.String("checked"),
		Body: bytes.NewReader(body), ChecksumCRC32C: aws.String(crc32c(body))})
	if status, code := refusal(err); status != 400 || code != "BadDigest" {
		t.Errorf("a put with its CRC32C flipped at byte 5: %v, want 400 BadDigest", err)
	}
	if _, ok := r.stored(t, "checked"); ok {
		t.Error("the refused flipped put is stored, want nothing stored")
	}

	r.tr.Inject(Rule{Op: "PutObject", KeyPrefix: "unchecked", Times: 1}, FlipByte(5))
	if err := r.put(t.Context(), "unchecked", body); err != nil {
		t.Fatalf("a put with no checksum flipped at byte 5: %v", err)
	}
	if got, _ := r.stored(t, "unchecked"); !bytes.Equal(got, flipped(body)) {
		t.Errorf("the put flipped at byte 5 stored %q, want %q: the body sent with byte 5 alone inverted", got, flipped(body))
	}

	if err := r.put(t.Context(), "read", body); err != nil {
		t.Fatal(err)
	}
	r.tr.Inject(Rule{Op: "GetObject", KeyPrefix: "read", Times: 1}, FlipByte(5))
	if _, got, _ := r.get(t, "read", ""); !bytes.Equal(got, flipped(body)) {
		t.Errorf("a get flipped at byte 5 read %q, want %q: the body stored with byte 5 alone inverted", got, flipped(body))
	}
	if stored, _ := r.stored(t, "read"); !bytes.Equal(stored, body) {
		t.Error("a get's flip reached the stored object, want the reply flipped alone")
	}
}

// A strip under MinBody strips only at or above it: a put with a wrong CRC32C
// is stored at 2 MiB, its checksum stripped, and refused at 1 KiB, its
// checksum kept. The record keeps the stripped header as the client built it.
// The stored put also pins that LocalStack checks no signature, since the
// stripped header was signed.
func TestStripHeadersHonoursItsThreshold(t *testing.T) {
	r := newRig(t, nil)
	r.tr.Inject(Rule{Op: "PutObject", MinBody: 1 << 20}, StripHeaders(checksumHeaders...))
	wrong := crc32c([]byte("not the body"))
	for _, c := range []struct {
		key    string
		size   int
		stored bool
	}{
		{"large", 2 << 20, true},
		{"small", 1 << 10, false},
	} {
		body := bytes.Repeat([]byte{'s'}, c.size)
		_, err := r.client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: &c.key,
			Body: bytes.NewReader(body), ChecksumCRC32C: &wrong})
		switch {
		case c.stored && err != nil:
			t.Errorf("a %d-byte put with a wrong CRC32C, stripped above 1 MiB: %v, want it stored; "+
				"if the refusal is about the signature, LocalStack now checks signatures and StripHeaders needs the request signed again", c.size, err)
		case !c.stored:
			if status, code := refusal(err); status != 400 || code != "BadDigest" {
				t.Errorf("a %d-byte put with a wrong CRC32C, below the 1 MiB threshold: %v, want 400 BadDigest", c.size, err)
			}
		}
		if got, ok := r.stored(t, c.key); ok != c.stored || (ok && !bytes.Equal(got, body)) {
			t.Errorf("after the %d-byte put the service holds %d bytes (stored %v), want stored %v", c.size, len(got), ok, c.stored)
		}
	}
	for _, req := range r.tr.Requests() {
		if req.Op == "PutObject" && req.Header.Get("X-Amz-Checksum-Crc32c") != wrong {
			t.Errorf("the record of the put of %s holds x-amz-checksum-crc32c %q, want %q as the client built it", req.Key, req.Header.Get("X-Amz-Checksum-Crc32c"), wrong)
		}
	}
}

// LocalStack clamps a range that ends past the object's end, as every service
// measured does, so no effect has to; an in-range get is served as asked, and
// a range starting past the end, or on an absent key, is refused.
func TestLocalStackClampsAnOverlongRange(t *testing.T) {
	r := newRig(t, nil)
	if err := r.put(t.Context(), "ten", []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rng, want, contentRange string
	}{
		{"bytes=8-15", "89", "bytes 8-9/10"},
		{"bytes=2-5", "2345", "bytes 2-5/10"},
	} {
		if status, got, cr := r.get(t, "ten", c.rng); status != 206 || string(got) != c.want || cr != c.contentRange {
			t.Errorf("%s of a 10-byte object: %d %q under %q, want 206 %q under %q", c.rng, status, got, cr, c.want, c.contentRange)
		}
	}
	for _, c := range []struct {
		key, rng, code string
		status         int
	}{
		{"ten", "bytes=12-15", "InvalidRange", http.StatusRequestedRangeNotSatisfiable},
		{"absent", "bytes=2-5", "NoSuchKey", http.StatusNotFound},
	} {
		_, err := r.client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: &r.bucket, Key: &c.key, Range: &c.rng})
		if status, code := refusal(err); status != c.status || code != c.code {
			t.Errorf("%s of %s: %v, want %d %s", c.rng, c.key, err, c.status, c.code)
		}
	}
}

// A stripped Range turns a ranged get into a whole one, so a strip acts on a
// get's signed header as it does on a put's.
func TestStripHeadersActsOnAGet(t *testing.T) {
	r := newRig(t, nil)
	if err := r.put(t.Context(), "ten", []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	r.tr.Inject(Rule{Op: "GetObject", Times: 1}, StripHeaders("Range"))
	if status, got, _ := r.get(t, "ten", "bytes=2-5"); status != 200 || string(got) != "0123456789" {
		t.Errorf("bytes=2-5 with Range stripped: %d %q, want 200 and all 10 bytes", status, got)
	}
	for _, req := range r.tr.Requests() {
		if got := req.Header.Get("Range"); req.Op == "GetObject" && got != "bytes=2-5" {
			t.Errorf("the record of the stripped get holds Range %q, want bytes=2-5 as the client built it", got)
		}
	}
}

// Cubbit DS3, as Appendix B measured it, ignores every put checksum and
// If-None-Match: a wrong CRC32C and a wrong Content-MD5 are both stored, the
// ETag is the MD5 of the bytes stored, a conditional put overwrites, and a
// batch delete still succeeds. LocalStack also accepts a batch delete with its
// checksum stripped, so the composition runs over the rig's transport, whose
// record shows what reached the service: the batch delete still carries its
// checksum, and no put does.
func TestCubbitDS3sComposition(t *testing.T) {
	r := newRig(t, nil)
	cubbit := New(r.tr, nil)
	cubbit.Inject(Rule{Op: "PutObject"}, StripHeaders(append([]string{"Content-Md5", "If-None-Match"}, checksumHeaders...)...))
	r.client = rawClient(r.endpoint, cubbit)
	body := []byte("stored as sent")
	wrong := []byte("not the body")

	if _, err := r.client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: aws.String("crc"),
		Body: bytes.NewReader(body), ChecksumCRC32C: aws.String(crc32c(wrong))}); err != nil {
		t.Errorf("a put with a wrong CRC32C: %v, want it stored", err)
	}
	out, err := r.client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: aws.String("md5"),
		Body: bytes.NewReader(body), ContentMD5: aws.String(contentMD5(wrong))})
	if err != nil {
		t.Fatalf("a put with a wrong Content-MD5: %v, want it stored", err)
	}
	if sum := md5.Sum(body); aws.ToString(out.ETag) != fmt.Sprintf("%q", fmt.Sprintf("%x", sum)) {
		t.Errorf("the ETag of the put with a wrong Content-MD5: %s, want the MD5 of the bytes stored, %x", aws.ToString(out.ETag), sum)
	}
	for _, key := range []string{"crc", "md5"} {
		if got, _ := r.stored(t, key); !bytes.Equal(got, body) {
			t.Errorf("the service holds %q at %s, want %q", got, key, body)
		}
	}

	over := []byte("overwritten")
	if _, err := r.client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: aws.String("md5"),
		Body: bytes.NewReader(over), IfNoneMatch: aws.String("*")}); err != nil {
		t.Errorf("a put with If-None-Match: * on an existing key: %v, want it to overwrite", err)
	}
	if got, _ := r.stored(t, "md5"); !bytes.Equal(got, over) {
		t.Errorf("after the conditional put the service holds %q, want %q", got, over)
	}

	del, err := r.client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{Bucket: &r.bucket,
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("crc")}, {Key: aws.String("md5")}}}})
	if err != nil || len(del.Errors) != 0 || len(del.Deleted) != 2 {
		t.Fatalf("a batch delete of 2 keys: %v, %d deleted, errors %v, want both deleted", err, len(del.Deleted), del.Errors)
	}
	for _, key := range []string{"crc", "md5"} {
		if _, ok := r.stored(t, key); ok {
			t.Errorf("%s is stored after the batch delete, want it deleted", key)
		}
	}
	for _, req := range r.tr.Requests() {
		sent := req.Header.Get("Content-Md5") != "" || req.Header.Get("If-None-Match") != "" ||
			slices.ContainsFunc(checksumHeaders, func(h string) bool { return req.Header.Get(h) != "" })
		if req.Op == "PutObject" && sent || req.Op == "DeleteObjects" && !sent {
			t.Errorf("the service received the %s of %q with a checksum or If-None-Match: %v, want %v", req.Op, req.Key, sent, req.Op == "DeleteObjects")
		}
	}
}

// Scaleway, as Appendix B measured it, enforces only CRC32C: a wrong one is
// refused with 400 InvalidRequest and nothing is stored, and a wrong
// Content-MD5 is stored.
func TestScalewaysComposition(t *testing.T) {
	r := newRig(t, nil)
	r.tr.Inject(Rule{Op: "PutObject"}, StripHeaders("Content-Md5"))
	r.tr.Inject(Rule{Op: "PutObject"}, RecodeError("BadDigest", http.StatusBadRequest, "InvalidRequest"))
	body := []byte("stored as sent")
	wrong := []byte("not the body")

	_, err := r.client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: aws.String("crc"),
		Body: bytes.NewReader(body), ChecksumCRC32C: aws.String(crc32c(wrong))})
	if status, code := refusal(err); status != 400 || code != "InvalidRequest" {
		t.Errorf("a put with a wrong CRC32C: %v, want 400 InvalidRequest", err)
	}
	if _, ok := r.stored(t, "crc"); ok {
		t.Error("the put with a wrong CRC32C is stored, want nothing stored")
	}
	if _, err := r.client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: aws.String("md5"),
		Body: bytes.NewReader(body), ContentMD5: aws.String(contentMD5(wrong))}); err != nil {
		t.Errorf("a put with a wrong Content-MD5: %v, want it stored", err)
	}
	if got, _ := r.stored(t, "md5"); !bytes.Equal(got, body) {
		t.Errorf("the service holds %q at md5, want %q", got, body)
	}
}
