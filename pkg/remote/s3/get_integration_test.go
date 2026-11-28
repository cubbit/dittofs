//go:build integration

package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// newGetRig watches the test for leaked goroutines and descriptors, then
// makes a fresh bucket of its own.
func newGetRig(t *testing.T) *lsRig {
	t.Helper()

	// Before the watch, as newLSRig says.
	endpoint, roots := s3fault.LocalStack(t)
	prof.Watch(t, prof.Options{})
	b := s3fault.FreshBucket(t)

	return &lsRig{endpoint: endpoint, roots: roots, bucket: b.Name, admin: b.Admin}
}

// openFaulted is lsRig.open in the default class with f scheduling each
// request's fault.
func (r *lsRig) openFaulted(t *testing.T, ch choice, f s3fault.Fault) *store {
	t.Helper()

	c, err := NewClient(ClientConfig{
		Endpoint:        r.endpoint,
		Bucket:          r.bucket,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		TrustRoots:      r.roots,
		Limit:           4,
	}, Deps{
		Logger:    slog.New(slog.DiscardHandler),
		Recorder:  &calls{},
		Transport: func(rt http.RoundTripper) http.RoundTripper { return s3fault.New(rt, f) },
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

	return s
}

// tearPuts cuts every PutObject's body after its first after bytes.
type tearPuts struct{ after int }

var errTorn = errors.New("the connection was cut part-way through the put")

func (f tearPuts) At(op string, _ []byte) (time.Duration, int, error) {
	if op == "PutObject" {
		return 0, f.after, errTorn
	}

	return 0, 0, nil
}

// put stores b under name through s, in s's mode.
func put(t *testing.T, s *store, name remote.Name, b []byte, sum func([]byte) []byte) {
	t.Helper()

	if err := s.Put(t.Context(), name, bytes.NewReader(b), int64(len(b)), sum(b)); err != nil {
		t.Fatalf("Put of %d bytes: %v", len(b), err)
	}
}

// read is every byte r's reader yields, and the error it ended with: nil once
// it reached its end.
func read(t *testing.T, s *store, name remote.Name, r remote.Range) ([]byte, remote.StoredSum, error) {
	t.Helper()

	rc, sum, err := s.Get(t.Context(), name, r)
	if err != nil {
		return nil, sum, err
	}
	defer func() { _ = rc.Close() }()

	b, err := io.ReadAll(rc)

	return b, sum, err
}

// serviceSum is the checksum LocalStack holds for key, as a whole-block get
// in mode ch must report it: its CRC32C when it holds one, else the ETag's MD5
// when the store compares ETags, else none.
func (r *lsRig) serviceSum(t *testing.T, key string, ch choice) remote.StoredSum {
	t.Helper()

	out, err := r.admin.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: &r.bucket, Key: &key, ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		t.Fatalf("HeadObject %s: %v", key, err)
	}

	if c := aws.ToString(out.ChecksumCRC32C); c != "" {
		b, err := base64.StdEncoding.DecodeString(c)
		if err != nil {
			t.Fatalf("HeadObject %s: CRC32C %q: %v", key, c, err)
		}

		return remote.StoredSum{Kind: remote.ChecksumCRC32C, Sum: b}
	}

	if ch.proved && ch.sum == remote.ChecksumNone {
		b, ok := etagMD5(out.ETag)
		if !ok {
			t.Fatalf("HeadObject %s: the ETag %q is no MD5", key, aws.ToString(out.ETag))
		}

		return remote.StoredSum{Kind: remote.ChecksumMD5, Sum: b}
	}

	return remote.StoredSum{}
}

func sameSum(a, b remote.StoredSum) bool { return a.Kind == b.Kind && bytes.Equal(a.Sum, b.Sum) }

func TestGetReturnsWhatThePutStored(t *testing.T) {
	r := newGetRig(t)

	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			s, tr, rec, _ := r.open(t, "", m.ch)
			name := remote.Name{7}
			b := randomBytes(64 << 10)
			put(t, s, name, b, m.sum)

			got, sum, err := read(t, s, name, remote.Range{})
			if err != nil || !bytes.Equal(got, b) {
				t.Fatalf("whole get read %d bytes, %v; want the %d bytes put", len(got), err, len(b))
			}

			want := r.serviceSum(t, s.blockKey(name), m.ch)
			if !sameSum(sum, want) {
				t.Errorf("stored sum %v %x, want LocalStack's %v %x", sum.Kind, sum.Sum, want.Kind, want.Sum)
			}

			if m.ch.sum == remote.ChecksumCRC32C && !bytes.Equal(sum.Sum, crc32cOf(b)) {
				t.Errorf("stored sum %x, want the CRC32C put, %x", sum.Sum, crc32cOf(b))
			}

			got, sum, err = read(t, s, name, remote.Range{Off: 100, Len: 1000})
			if err != nil || !bytes.Equal(got, b[100:1100]) || sum.Kind != remote.ChecksumNone {
				t.Errorf("ranged get read %d bytes, %v, stored sum %v; want bytes 100 to 1099 and no sum", len(got), err, sum.Kind)
			}

			reqs := tr.Requests()
			if len(reqs) != 3 || reqs[1].Op != "GetObject" || reqs[2].Op != "GetObject" {
				t.Fatalf("requests %+v, want a PutObject and two GetObject", reqs)
			}

			if got := reqs[1].Header.Get("X-Amz-Checksum-Mode"); got != "ENABLED" {
				t.Errorf("whole get: x-amz-checksum-mode %q, want ENABLED", got)
			}
			if got := reqs[1].Header.Get("Range"); got != "" {
				t.Errorf("whole get: Range %q, want none", got)
			}
			if got := reqs[2].Header.Get("X-Amz-Checksum-Mode"); got != "" {
				t.Errorf("ranged get: x-amz-checksum-mode %q, want none", got)
			}
			if got := reqs[2].Header.Get("Range"); got != "bytes=100-1099" {
				t.Errorf("ranged get: Range %q, want bytes=100-1099", got)
			}

			calls := rec.all()
			if len(calls) != 3 || calls[1] != (recordedCall{"get", "ok", "", int64(len(b))}) || calls[2] != (recordedCall{"get", "ok", "", 1000}) {
				t.Errorf("recorded %+v, want a put, then two gets, ok, of %d and 1000 bytes", calls, len(b))
			}
		})
	}
}

func TestGetOfEachRange(t *testing.T) {
	r := newGetRig(t)
	s, _, _, _ := r.open(t, "", crc)
	name := remote.Name{1}
	b := randomBytes(100)
	put(t, s, name, b, crc32cOf)

	for off := int64(0); off < 100; off++ {
		for n := int64(1); off+n <= 100; n++ {
			got, _, err := read(t, s, name, remote.Range{Off: off, Len: n})
			if err != nil || !bytes.Equal(got, b[off:off+n]) {
				t.Fatalf("get of %d bytes at %d read %d bytes, %v; want exactly the range", n, off, len(got), err)
			}
		}
	}

	for _, rg := range []remote.Range{{Off: 100, Len: 1}, {Off: 150, Len: 10}} {
		if _, _, err := read(t, s, name, rg); !errors.Is(err, remote.ErrInvalid) {
			t.Errorf("get of %d bytes at %d, past the end = %v, want ErrInvalid", rg.Len, rg.Off, err)
		}
	}
}

// LocalStack clamps a range ending past the block's end, as some services do,
// and answers with the bytes it has.
func TestGetOfAClampedRangeIsInvalid(t *testing.T) {
	r := newGetRig(t)
	s, _, _, _ := r.open(t, "", crc)
	name := remote.Name{1}
	put(t, s, name, randomBytes(100), crc32cOf)

	for _, rg := range []remote.Range{{Off: 90, Len: 20}, {Off: 0, Len: 101}} {
		rc, _, err := s.Get(t.Context(), name, rg)
		if !errors.Is(err, remote.ErrInvalid) || rc != nil {
			t.Errorf("get of %d bytes at %d = %v, %v; want ErrInvalid and no reader", rg.Len, rg.Off, rc, err)
		}
	}
}

// LocalStack itself answers a get whose Range was stripped with the whole
// object, as a service that ignores Range does.
func TestGetOfAnIgnoredRangeIsInvalid(t *testing.T) {
	r := newGetRig(t)
	s, tr, _, _ := r.open(t, "", crc)
	name := remote.Name{1}
	put(t, s, name, randomBytes(100), crc32cOf)
	tr.Inject(s3fault.Rule{Op: "GetObject"}, s3fault.StripHeaders("Range"))

	rc, _, err := s.Get(t.Context(), name, remote.Range{Off: 10, Len: 20})
	if !errors.Is(err, remote.ErrInvalid) || rc != nil {
		t.Errorf("Get = %v, %v; want ErrInvalid and no reader", rc, err)
	}
}

func TestGetOfATruncatedBodyIsTransient(t *testing.T) {
	r := newGetRig(t)
	s, tr, rec, _ := r.open(t, "", crc)
	name := remote.Name{1}
	b := randomBytes(1000)
	put(t, s, name, b, crc32cOf)
	tr.Inject(s3fault.Rule{Op: "GetObject"}, s3fault.TruncateReply(40))

	for _, tc := range []struct {
		why  string
		r    remote.Range
		want []byte
	}{
		{"ranged, under a matching Content-Range", remote.Range{Off: 10, Len: 500}, b[10:50]},
		{"whole, under its Content-Length", remote.Range{}, b[:40]},
	} {
		got, _, err := read(t, s, name, tc.r)
		if !bytes.Equal(got, tc.want) || !errors.Is(err, remote.ErrTransient) {
			t.Errorf("get [%s] read %d bytes, %v; want the 40 sent, then ErrTransient", tc.why, len(got), err)
		}
	}

	if calls := rec.all(); len(calls) != 3 || calls[1] != (recordedCall{"get", "transient", "", 40}) || calls[2] != (recordedCall{"get", "transient", "", 40}) {
		t.Errorf("recorded %+v, want a put, then two gets, transient, of 40 bytes", calls)
	}
}

func TestGetAfterAPutAnsweredNotFound(t *testing.T) {
	r := newGetRig(t)
	s, tr, _, _ := r.open(t, "", crc)
	name := remote.Name{1}
	b := randomBytes(100)
	put(t, s, name, b, crc32cOf)
	tr.Inject(s3fault.Rule{Op: "GetObject", Times: 1}, s3fault.Answer(http.StatusNotFound, "NoSuchKey", nil))

	if _, _, err := read(t, s, name, remote.Range{}); !errors.Is(err, remote.ErrNotFound) {
		t.Errorf("the first get = %v, want ErrNotFound", err)
	}

	if got, _, err := read(t, s, name, remote.Range{}); err != nil || !bytes.Equal(got, b) {
		t.Errorf("the next get read %d bytes, %v; want the block", len(got), err)
	}

	if _, _, err := read(t, s, remote.Name{2}, remote.Range{}); !errors.Is(err, remote.ErrNotFound) {
		t.Errorf("a get of a name never put = %v, want ErrNotFound", err)
	}
}

// A byte flipped in a whole-block get's reply, on a service returning a
// response checksum, or under the ETag the store compares: every byte is
// yielded, then the reader fails, its error reaching no SDK type.
func TestGetOfAFlippedWholeBlock(t *testing.T) {
	r := newGetRig(t)

	for _, m := range []int{0, 2} { // crc32c and etag
		mode := modes[m]
		t.Run(mode.name, func(t *testing.T) {
			s, tr, _, _ := r.open(t, "", mode.ch)
			name := remote.Name{1}
			b := randomBytes(64 << 10)
			put(t, s, name, b, mode.sum)
			tr.Inject(s3fault.Rule{Op: "GetObject"}, s3fault.FlipByte(7))

			got, sum, err := read(t, s, name, remote.Range{})
			if !errors.Is(err, remote.ErrTransient) {
				t.Errorf("the reader ended with %v, want ErrTransient", err)
			}
			noSDKType(t, "the reader's end", err)

			want := bytes.Clone(b)
			want[7] ^= 0xff
			if !bytes.Equal(got, want) {
				t.Errorf("read %d bytes, want every one of the %d sent", len(got), len(b))
			}

			if ss := r.serviceSum(t, s.blockKey(name), mode.ch); !sameSum(sum, ss) || sum.Kind == remote.ChecksumNone {
				t.Errorf("stored sum %v %x, want LocalStack's %v %x", sum.Kind, sum.Sum, ss.Kind, ss.Sum)
			}
		})
	}
}

func TestGetAfterAPutCutOffFindsNothing(t *testing.T) {
	r := newGetRig(t)
	b := randomBytes(64 << 10)
	name := remote.Name{1}

	s := r.openFaulted(t, crc, tearPuts{after: 1000})
	if err := s.Put(t.Context(), name, bytes.NewReader(b), int64(len(b)), crc32cOf(b)); err == nil {
		t.Fatal("a put cut off part-way returned nil")
	}

	if _, _, err := read(t, s, name, remote.Range{}); !errors.Is(err, remote.ErrNotFound) {
		t.Errorf("a get after the cut put = %v, want ErrNotFound", err)
	}

	// A stream shorter or longer than its size fails the put, and leaves
	// nothing to get.
	s, _, _, _ = r.open(t, "", crc)
	for i, body := range [][]byte{b[:len(b)-1], append(bytes.Clone(b), 0)} {
		n := remote.Name{byte(i + 2)}
		if err := s.Put(t.Context(), n, bytes.NewReader(body), int64(len(b)), crc32cOf(b)); !errors.Is(err, remote.ErrInvalid) {
			t.Errorf("a put of %d bytes declared %d = %v, want ErrInvalid", len(body), len(b), err)
		}

		if _, _, err := read(t, s, n, remote.Range{}); !errors.Is(err, remote.ErrNotFound) {
			t.Errorf("a get after the put of %d bytes declared %d = %v, want ErrNotFound", len(body), len(b), err)
		}
	}
}

// A byte flipped in a put of a new name: a service that checked the sum
// stored nothing; under the ETag compared, the corrupt object is stored until
// a put without the fault replaces it.
func TestGetAfterAFlippedPut(t *testing.T) {
	r := newGetRig(t)

	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			s, tr, _, _ := r.open(t, "", m.ch)
			name := remote.Name{1}
			b := randomBytes(64 << 10)
			tr.Inject(s3fault.Rule{Op: "PutObject", Times: 1}, s3fault.FlipByte(5))

			if err := s.Put(t.Context(), name, bytes.NewReader(b), int64(len(b)), m.sum(b)); !errors.Is(err, remote.ErrCorrupt) {
				t.Fatalf("the flipped put = %v, want ErrCorrupt", err)
			}

			got, _, err := read(t, s, name, remote.Range{})
			if m.ch.sum != remote.ChecksumNone {
				if !errors.Is(err, remote.ErrNotFound) {
					t.Errorf("a get after the refused put read %d bytes, %v; want ErrNotFound", len(got), err)
				}
				return
			}

			corrupt := bytes.Clone(b)
			corrupt[5] ^= 0xff
			if err != nil || !bytes.Equal(got, corrupt) {
				t.Errorf("a get after the flipped put read %d bytes, %v; want the corrupt object", len(got), err)
			}

			put(t, s, name, b, m.sum)
			if got, _, err := read(t, s, name, remote.Range{}); err != nil || !bytes.Equal(got, b) {
				t.Errorf("a get after the retried put read %d bytes, %v; want the original", len(got), err)
			}
		})
	}
}

// TestGetAllocatesFlat bounds the bytes a whole get allocates: at 8 MiB no
// more than a fixed slack over 1 MiB, so neither the store nor the SDK holds
// the block.
func TestGetAllocatesFlat(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector's allocations make the count meaningless")
	}

	r := newGetRig(t)
	s, _, _, _ := r.open(t, "", crc)
	buf := make([]byte, 32<<10)

	perGet := func(size int64) uint64 {
		name := remote.Name{byte(size >> 20)}
		b, _ := io.ReadAll(&pattern{size})
		put(t, s, name, b, crc32cOf)

		const gets = 4
		var before, after runtime.MemStats

		// One get first, so the connection and the pool are in place.
		get := func() {
			rc, _, err := s.Get(context.Background(), name, remote.Range{})
			if err != nil {
				t.Fatalf("Get of %d bytes: %v", size, err)
			}
			n, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, struct{ io.Reader }{rc}, buf)
			_ = rc.Close()
			if err != nil || n != size {
				t.Fatalf("Get of %d bytes read %d, %v", size, n, err)
			}
		}
		get()

		runtime.GC()
		runtime.ReadMemStats(&before)
		for range gets {
			get()
		}
		runtime.ReadMemStats(&after)

		return (after.TotalAlloc - before.TotalAlloc) / gets
	}

	small, large := perGet(1<<20), perGet(8<<20)

	const slack = 256 << 10
	if small > slack {
		t.Errorf("a get of 1 MiB allocates %d bytes, more than %d: the block is buffered", small, slack)
	}
	if large > small+slack {
		t.Errorf("a get allocates %d bytes at 8 MiB and %d at 1 MiB, more than %d apart: the block is buffered", large, small, slack)
	}

	t.Logf("bytes allocated per get: %d at 1 MiB, %d at 8 MiB", small, large)
}
