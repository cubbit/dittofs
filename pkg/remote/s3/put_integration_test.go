//go:build integration

package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// lsRig is a fresh bucket on LocalStack, a client under test whose every
// request passes through a recording fault transport, and the SDK's own
// client beside it, which reads back what the service holds.
type lsRig struct {
	endpoint string
	roots    *x509.CertPool
	bucket   string
	admin    *s3.Client
}

// newLSRig watches the test for leaked goroutines and descriptors, then makes
// a fresh bucket, deleted with every object in it at cleanup.
func newLSRig(t *testing.T) *lsRig {
	t.Helper()

	// Before the watch: its handshake is the process's first connection to
	// the endpoint, which on macOS leaves a descriptor open for the life of
	// the process.
	endpoint, roots := s3fault.LocalStack(t)
	prof.Watch(t, prof.Options{})

	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(tr.CloseIdleConnections)

	r := &lsRig{endpoint: endpoint, roots: roots, admin: s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		HTTPClient:   &http.Client{Transport: tr},
	})}

	var name [6]byte
	_, _ = rand.Read(name[:])
	r.bucket = "s3-put-" + hex.EncodeToString(name[:])

	if _, err := r.admin.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: &r.bucket}); err != nil {
		t.Fatalf("CreateBucket %s: %v", r.bucket, err)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		list, err := r.admin.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &r.bucket})
		if err != nil {
			t.Errorf("listing %s to delete it: %v", r.bucket, err)
			return
		}
		for _, o := range list.Contents {
			if _, err := r.admin.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &r.bucket, Key: o.Key}); err != nil {
				t.Errorf("DeleteObject %s: %v", *o.Key, err)
			}
		}
		if _, err := r.admin.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &r.bucket}); err != nil {
			t.Errorf("DeleteBucket %s: %v", r.bucket, err)
		}
	})

	return r
}

// open opens a store under a fresh prefix on a new client in class (""
// for the default), the check's choice set to ch. It returns the store, the
// transport every request of the client passes through, the recorder and the
// log sink.
func (r *lsRig) open(t *testing.T, class string, ch choice) (*store, *s3fault.Transport, *calls, *logs) {
	t.Helper()

	var tr *s3fault.Transport
	rec, l := &calls{}, &logs{}

	c, err := NewClient(ClientConfig{
		Endpoint:        r.endpoint,
		Bucket:          r.bucket,
		StorageClass:    class,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		TrustRoots:      r.roots,
		Limit:           4,
	}, Deps{
		Logger:    slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Recorder:  rec,
		Transport: func(rt http.RoundTripper) http.RoundTripper { tr = s3fault.New(rt, nil); return tr },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	var p [4]byte
	_, _ = rand.Read(p[:])

	s, err := open(c, "ns-"+hex.EncodeToString(p[:])+"/", remote.Mutable, ch)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	return s, tr, rec, l
}

// stored is what the service holds at key, ok false when it holds nothing.
func (r *lsRig) stored(t *testing.T, key string) (body []byte, ok bool) {
	t.Helper()

	out, err := r.admin.GetObject(t.Context(), &s3.GetObjectInput{Bucket: &r.bucket, Key: &key})
	if _, missing := errors.AsType[*types.NoSuchKey](err); missing {
		return nil, false
	}
	if err != nil {
		t.Fatalf("GetObject %s: %v", key, err)
	}
	defer func() { _ = out.Body.Close() }()

	b, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", key, err)
	}

	return b, true
}

// modes are the three integrity mechanisms a check can prove, each with the
// sum a put of b carries.
func modes() []struct {
	name string
	ch   choice
	sum  func(b []byte) []byte
} {
	return []struct {
		name string
		ch   choice
		sum  func(b []byte) []byte
	}{
		{"crc32c", choice{proved: true, sum: remote.ChecksumCRC32C}, crc32cOf},
		{"content-md5", choice{proved: true, sum: remote.ChecksumMD5}, func(b []byte) []byte { s := md5.Sum(b); return s[:] }},
		{"etag", choice{proved: true, sum: remote.ChecksumNone}, func([]byte) []byte { return nil }},
	}
}

func crc32cOf(b []byte) []byte {
	return binary.BigEndian.AppendUint32(nil, crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)))
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return b
}

// integrityHeaders are the headers of h that carry a body's checksum.
func integrityHeaders(h http.Header) []string {
	var out []string
	for k := range h {
		if k == "Content-Md5" || strings.HasPrefix(k, "X-Amz-Checksum-") || k == "X-Amz-Sdk-Checksum-Algorithm" {
			out = append(out, k)
		}
	}

	return out
}

func TestPutStoresTheBlockInEachMode(t *testing.T) {
	r := newLSRig(t)

	for _, m := range modes() {
		t.Run(m.name, func(t *testing.T) {
			s, tr, rec, l := r.open(t, "", m.ch)
			name := remote.Name{1, 2, 3}

			first, second := randomBytes(64<<10), randomBytes(1000)
			for _, b := range [][]byte{first, second} {
				if err := s.Put(t.Context(), name, bytes.NewReader(b), int64(len(b)), m.sum(b)); err != nil {
					t.Fatalf("Put of %d bytes: %v", len(b), err)
				}
			}

			// The second put replaced the first.
			if got, ok := r.stored(t, s.blockKey(name)); !ok || !bytes.Equal(got, second) {
				t.Errorf("LocalStack holds %d bytes (present %v) under the name, want the second put's %d", len(got), ok, len(second))
			}

			reqs := tr.Requests()
			if len(reqs) != 2 {
				t.Fatalf("%d requests reached the transport, want 2 PutObject", len(reqs))
			}

			want := 1
			if m.ch.sum == remote.ChecksumNone {
				want = 0
			}

			for i, q := range reqs {
				if q.Op != "PutObject" || q.Key != s.blockKey(name) {
					t.Errorf("request %d: %s of %q, want PutObject of %q", i, q.Op, q.Key, s.blockKey(name))
				}

				if got := integrityHeaders(q.Header); len(got) != want {
					t.Errorf("request %d carried integrity headers %v, want %d", i, got, want)
				}

				for k := range q.Header {
					if strings.HasPrefix(k, "If-") {
						t.Errorf("request %d carried the conditional header %s", i, k)
					}
				}

				if got := q.Header.Get("X-Amz-Storage-Class"); got != "STANDARD" {
					t.Errorf("request %d: x-amz-storage-class %q, want STANDARD", i, got)
				}
			}

			if got := rec.all(); len(got) != 2 || got[0] != (recordedCall{"put", "ok", "", int64(len(first))}) || got[1] != (recordedCall{"put", "ok", "", int64(len(second))}) {
				t.Errorf("recorded %+v, want two puts, ok, of %d and %d bytes", got, len(first), len(second))
			}

			if got := l.records(t); len(got) != 0 {
				t.Errorf("a put that succeeded logged %v", got)
			}
		})
	}
}

func TestPutWithNothingProvedSendsNothing(t *testing.T) {
	r := newLSRig(t)
	s, tr, _, _ := r.open(t, "", choice{})

	if err := s.Put(t.Context(), remote.Name{1}, strings.NewReader("x"), 1, crc32cOf([]byte("x"))); !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("Put = %v, want ErrInvalid", err)
	}

	if got := tr.Requests(); len(got) != 0 {
		t.Errorf("%d requests reached the transport, want none", len(got))
	}

	if _, ok := r.stored(t, s.blockKey(remote.Name{1})); ok {
		t.Error("LocalStack holds an object under the name")
	}
}

func TestPutWritesTheConfiguredStorageClass(t *testing.T) {
	r := newLSRig(t)
	s, _, _, _ := r.open(t, "STANDARD_IA", modes()[0].ch)
	b := []byte("a block")

	if err := s.Put(t.Context(), remote.Name{1}, bytes.NewReader(b), int64(len(b)), crc32cOf(b)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	key := s.blockKey(remote.Name{1})
	out, err := r.admin.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: &r.bucket, Key: &key})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if out.StorageClass != types.StorageClassStandardIa {
		t.Errorf("LocalStack reports the class %q, want STANDARD_IA", out.StorageClass)
	}
}

// LocalStack answers a wrong sum under either header 400 BadDigest, so the
// check's recorded rejection is not what makes these ErrCorrupt; a unit test
// covers that.
func TestPutOfAWrongSumIsCorrupt(t *testing.T) {
	r := newLSRig(t)
	b := []byte("a block whose sum is wrong")

	for _, m := range modes()[:2] {
		t.Run(m.name, func(t *testing.T) {
			s, _, rec, l := r.open(t, "", m.ch)
			name := remote.Name{9}

			sum := m.sum(b)
			sum[0] ^= 1

			if err := s.Put(t.Context(), name, bytes.NewReader(b), int64(len(b)), sum); !errors.Is(err, remote.ErrCorrupt) {
				t.Errorf("Put = %v, want ErrCorrupt", err)
			}

			if _, ok := r.stored(t, s.blockKey(name)); ok {
				t.Error("LocalStack stored the object whose sum it refused")
			}

			if got := rec.all(); len(got) != 1 || got[0].op != "put" || got[0].result != "corrupt" {
				t.Errorf("recorded %+v, want one put, corrupt", got)
			}

			got := l.records(t)
			if len(got) != 1 || got[0]["level"] != "WARN" || got[0]["block"] != hex.EncodeToString(name[:]) {
				t.Errorf("logged %v, want one Warn naming the block", got)
			}
		})
	}
}

func TestPutOfAWrongLengthStreamStoresNothing(t *testing.T) {
	r := newLSRig(t)
	s, _, rec, _ := r.open(t, "", modes()[0].ch)
	b := randomBytes(100 << 10)

	for _, tc := range []struct {
		why  string
		body []byte
		size int64
	}{
		{"a byte short", b[:len(b)-1], int64(len(b))},
		{"a byte long", b, int64(len(b) - 1)},
	} {
		name := remote.Name{byte(len(tc.why))}

		// The sum is of the declared bytes, so only the length is wrong.
		err := s.Put(t.Context(), name, bytes.NewReader(tc.body), tc.size, crc32cOf(b[:tc.size]))
		if !errors.Is(err, remote.ErrInvalid) {
			t.Errorf("Put [%s] = %v, want ErrInvalid", tc.why, err)
		}

		if _, ok := r.stored(t, s.blockKey(name)); ok {
			t.Errorf("Put [%s]: LocalStack holds an object under the name", tc.why)
		}
	}

	for i, c := range rec.all() {
		if c.result != "invalid" {
			t.Errorf("call %d recorded %+v, want invalid", i, c)
		}
	}
}

func TestPutReturnsTheReadersOwnError(t *testing.T) {
	r := newLSRig(t)
	s, _, rec, _ := r.open(t, "", modes()[0].ch)
	b := randomBytes(64 << 10)
	errBody := errors.New("the journal's reader failed")
	name := remote.Name{5}

	body := io.MultiReader(bytes.NewReader(b[:1000]), iotest.ErrReader(errBody))
	err := s.Put(t.Context(), name, body, int64(len(b)), crc32cOf(b))
	if !errors.Is(err, errBody) || errors.Is(err, remote.ErrTransient) {
		t.Errorf("Put = %v, want the reader's own error, no sentinel", err)
	}

	if _, ok := r.stored(t, s.blockKey(name)); ok {
		t.Error("LocalStack holds an object under the name")
	}

	if got := rec.all(); len(got) != 1 || got[0].result != "local" {
		t.Errorf("recorded %+v, want one put, local", got)
	}
}

func TestPutRecordsADenialsCause(t *testing.T) {
	r := newLSRig(t)
	s, tr, rec, _ := r.open(t, "", modes()[0].ch)
	tr.Inject(s3fault.Rule{Op: "PutObject"}, s3fault.Answer(http.StatusForbidden, "AccessDenied", nil))
	b := []byte("x")

	err := s.Put(t.Context(), remote.Name{1}, bytes.NewReader(b), 1, crc32cOf(b))
	if d, ok := errors.AsType[*remote.DeniedError](err); !ok || d.Cause != remote.CauseAccess {
		t.Errorf("Put = %v, want ErrDenied of cause access", err)
	}

	if got := rec.all(); len(got) != 1 || got[0].result != "denied" || got[0].cause != "access" {
		t.Errorf("recorded %+v, want one put, denied, access", got)
	}
}

// pattern is a reader of n bytes that allocates nothing per read.
type pattern struct{ n int64 }

func (p *pattern) Read(b []byte) (int, error) {
	if p.n == 0 {
		return 0, io.EOF
	}

	k := min(int64(len(b)), p.n)
	for i := range b[:k] {
		b[i] = byte(p.n - int64(i))
	}
	p.n -= k

	return int(k), nil
}

// TestPutAllocatesFlat bounds the bytes a put allocates: at 8 MiB no more than
// a fixed slack over 1 MiB, so neither the store nor the SDK holds the block.
func TestPutAllocatesFlat(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector's allocations make the count meaningless")
	}

	r := newLSRig(t)
	s, _, _, _ := r.open(t, "", modes()[0].ch)

	perPut := func(size int64) uint64 {
		b, _ := io.ReadAll(&pattern{size})
		sum := crc32cOf(b)

		const puts = 4
		var before, after runtime.MemStats

		// One put first, so the connection and the pool are in place.
		put := func() {
			if err := s.Put(t.Context(), remote.Name{1}, &pattern{size}, size, sum); err != nil {
				t.Fatalf("Put of %d bytes: %v", size, err)
			}
		}
		put()

		runtime.GC()
		runtime.ReadMemStats(&before)
		for range puts {
			put()
		}
		runtime.ReadMemStats(&after)

		return (after.TotalAlloc - before.TotalAlloc) / puts
	}

	small, large := perPut(1<<20), perPut(8<<20)

	const slack = 256 << 10
	if large > small+slack {
		t.Errorf("a put allocates %d bytes at 8 MiB and %d at 1 MiB, more than %d apart: the block is buffered", large, small, slack)
	}

	t.Logf("bytes allocated per put: %d at 1 MiB, %d at 8 MiB", small, large)
}
