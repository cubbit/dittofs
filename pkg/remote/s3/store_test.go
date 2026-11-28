package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/cubbit/dittofs/pkg/remote"
)

// recordedCall is one call a calls sink was told of.
type recordedCall struct {
	op, result, cause string
	bytes             int64
}

// calls is a Recorder that keeps the calls it was told of.
type calls struct {
	mu   sync.Mutex
	got  []recordedCall
	conn atomic.Int64
}

func (c *calls) Call(op, result, cause string, _ time.Duration, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, recordedCall{op, result, cause, n})
}

func (c *calls) ConnOpened() { c.conn.Add(1) }
func (c *calls) ForeignKey() {}

func (c *calls) all() []recordedCall {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]recordedCall(nil), c.got...)
}

// logs is a JSON log sink at every level, its lines read back as records.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

func (l *logs) records(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []map[string]any
	for line := range bytes.Lines(l.buf.Bytes()) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}

	return out
}

// offline is a client no test request reaches: building one opens nothing.
func offline(t *testing.T) *Client {
	t.Helper()

	c, err := NewClient(ClientConfig{
		Endpoint: "https://127.0.0.1:1", Bucket: "b", AccessKeyID: "AKIDFAKE", SecretAccessKey: "fake-secret", Limit: 1,
	}, Deps{Logger: slog.New(slog.DiscardHandler), Recorder: &calls{}})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestBlockKeyAndNameFromKey(t *testing.T) {
	s, err := open(offline(t), "ns/", remote.Mutable, choice{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	var n remote.Name
	for i := range n {
		n[i] = byte(0xa0 + i%16)
	}
	h := hex.EncodeToString(n[:])

	if got, want := s.blockKey(n), "ns/blocks/"+h; got != want {
		t.Errorf("blockKey = %q, want %q", got, want)
	}

	for _, tc := range []struct{ why, key string }{
		{"a hex digit short", "ns/blocks/" + h[1:]},
		{"a hex digit long", "ns/blocks/" + h + "0"},
		{"upper case", "ns/blocks/" + strings.ToUpper(h)},
		{"one upper-case digit", "ns/blocks/" + h[:63] + strings.ToUpper(h[63:])},
		{"a byte between blocks/ and the name", "ns/blocks/x" + h[1:]},
		{"a path between blocks/ and the name", "ns/blocks/2026/" + h},
		{"not hex", "ns/blocks/" + strings.Repeat("g", 64)},
		{"another prefix", "ns2/blocks/" + h},
		{"no prefix", "blocks/" + h},
		{"a control key", "ns/control/claim"},
	} {
		if got, ok := s.nameFromKey(tc.key); ok {
			t.Errorf("nameFromKey(%q) [%s] = %x, true; want false", tc.key, tc.why, got)
		}
	}

	// Every name round-trips, and its key is the prefix, blocks/ and the
	// name, nothing else.
	r := rand.New(rand.NewChaCha8([32]byte{1}))
	for range 10000 {
		for i := range n {
			n[i] = byte(r.Uint32())
		}

		key := s.blockKey(n)
		if want := "ns/blocks/" + hex.EncodeToString(n[:]); key != want {
			t.Fatalf("blockKey(%x) = %q, want %q", n, key, want)
		}

		if got, ok := s.nameFromKey(key); !ok || got != n {
			t.Fatalf("nameFromKey(blockKey(%x)) = %x, %v; want the name, true", n, got, ok)
		}
	}
}

func TestControlKey(t *testing.T) {
	s, err := open(offline(t), "ns/", remote.Mutable, choice{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	for r, want := range map[remote.Role]string{
		remote.RoleClaim:  "ns/control/claim",
		remote.RoleHealth: "ns/control/health",
		remote.RoleKeys:   "ns/control/keys",
	} {
		if got, ok := s.controlKey(r); !ok || got != want {
			t.Errorf("controlKey(%v) = %q, %v; want %q, true", r, got, ok, want)
		}
	}

	if got, ok := s.controlKey(remote.Role(0)); ok {
		t.Errorf("controlKey(Role(0)) = %q, true; want false", got)
	}
}

func TestOpenRefuses(t *testing.T) {
	c := offline(t)

	for _, tc := range []struct {
		why    string
		c      *Client
		prefix string
		mode   remote.Mode
	}{
		{"an empty prefix", c, "", remote.Mutable},
		{"a prefix not ending in /", c, "ns", remote.Mutable},
		{"a prefix whose last segment runs on", c, "ns/a", remote.Mutable},
		{"immutable", c, "ns/", remote.Immutable},
		{"no mode", c, "ns/", 0},
		{"no client", nil, "ns/", remote.Mutable},
	} {
		s, err := Open(t.Context(), tc.c, tc.prefix, tc.mode)
		if !errors.Is(err, remote.ErrInvalid) || s != nil {
			t.Errorf("Open [%s] = %v, %v; want nil, ErrInvalid", tc.why, s, err)
		}
	}

	s, err := Open(t.Context(), c, "ns/", remote.Mutable)
	if err != nil {
		t.Fatalf("Open of ns/: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Errorf("a second Close: %v, want nil", err)
	}
}

// counting is a TLS server that counts the requests it receives and answers
// each 200 with an ETag of etag(body).
func counting(t *testing.T, etag func(body []byte) string) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var n atomic.Int64
	srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("ETag", etag(b))
	}, false, nil)

	return srv, &n
}

func md5ETag(b []byte) string {
	s := md5.Sum(b)
	return `"` + hex.EncodeToString(s[:]) + `"`
}

// storeOn opens a store under ns/ on a client of srv, the check's choice set
// to ch, logging every record to l when it is not nil.
func storeOn(t *testing.T, srv *httptest.Server, ch choice, l *logs) (*store, *calls) {
	t.Helper()

	c, _ := testClient(t, srv, nil)
	rec := &calls{}
	c.rec = rec

	if l != nil {
		c.log = slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})).With("component", "remote")
	}

	s, err := open(c, "ns/", remote.Mutable, ch)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	return s, rec
}

func TestPutRefusesBeforeAnyRequest(t *testing.T) {
	srv, n := counting(t, md5ETag)
	crc := choice{proved: true, sum: remote.ChecksumCRC32C}
	mdc := choice{proved: true, sum: remote.ChecksumMD5}

	for _, tc := range []struct {
		why    string
		ch     choice
		closed bool
		size   int64
		sum    []byte
	}{
		{"nothing proved", choice{}, false, 1, nil},
		{"nothing proved, with a sum", choice{sum: remote.ChecksumCRC32C}, false, 1, []byte{1, 2, 3, 4}},
		{"a closed store", crc, true, 1, []byte{1, 2, 3, 4}},
		{"a negative size", crc, false, -1, []byte{1, 2, 3, 4}},
		{"no CRC32C", crc, false, 1, nil},
		{"a short CRC32C", crc, false, 1, []byte{1, 2, 3}},
		{"a CRC32C as hex", crc, false, 1, []byte("e3069283")},
		{"no MD5", mdc, false, 1, nil},
		{"a long MD5", mdc, false, 1, make([]byte, 17)},
	} {
		s, rec := storeOn(t, srv, tc.ch, nil)
		if tc.closed {
			_ = s.Close()
		}

		err := s.Put(t.Context(), remote.Name{1}, strings.NewReader("x"), tc.size, tc.sum)
		if !errors.Is(err, remote.ErrInvalid) {
			t.Errorf("Put [%s] = %v, want ErrInvalid", tc.why, err)
		}

		if got := rec.all(); len(got) != 1 || got[0] != (recordedCall{"put", "invalid", "", 0}) {
			t.Errorf("Put [%s] recorded %+v, want one put, invalid", tc.why, got)
		}
	}

	if got := n.Load(); got != 0 {
		t.Errorf("the service received %d requests, want 0", got)
	}
}

func TestPutComparesTheETag(t *testing.T) {
	body := []byte("a block's bytes")

	for _, tc := range []struct {
		why  string
		etag func([]byte) string
		want error
	}{
		{"the body's MD5", md5ETag, nil},
		{"another body's MD5", func([]byte) string { return md5ETag([]byte("other")) }, remote.ErrCorrupt},
		{"no ETag", func([]byte) string { return "" }, remote.ErrCorrupt},
	} {
		srv, _ := counting(t, tc.etag)
		l := &logs{}
		s, rec := storeOn(t, srv, choice{proved: true}, l)

		err := s.Put(t.Context(), remote.Name{7}, bytes.NewReader(body), int64(len(body)), nil)
		if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("Put [%s] = %v, want %v", tc.why, err, tc.want)
		}

		result := "ok"
		if tc.want != nil {
			result = "corrupt"
		}

		if got := rec.all(); len(got) != 1 || got[0] != (recordedCall{"put", result, "", int64(len(body))}) {
			t.Errorf("Put [%s] recorded %+v, want one put, %s, %d bytes", tc.why, got, result, len(body))
		}

		got := l.records(t)
		switch {
		case tc.want == nil && len(got) != 0:
			t.Errorf("Put [%s] logged %v, want nothing", tc.why, got)
		case tc.want != nil && (len(got) != 1 || got[0]["level"] != "WARN" ||
			got[0]["block"] != "07"+strings.Repeat("00", 31) || got[0]["component"] != "remote"):
			t.Errorf("Put [%s] logged %v, want one Warn naming block 07…00, component remote", tc.why, got)
		}
	}
}

func TestPutMatchesTheRecordedRejectionOnlyWhenItCarriedTheChecksum(t *testing.T) {
	srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, xmlError("InvalidRequest"))
	}, false, nil)
	rejected := recorded{http.StatusBadRequest, "InvalidRequest"}
	b := []byte("x")

	for _, tc := range []struct {
		why  string
		ch   choice
		sum  []byte
		want error
	}{
		{"CRC32C carried", choice{proved: true, sum: remote.ChecksumCRC32C, rejected: rejected}, []byte{1, 2, 3, 4}, remote.ErrCorrupt},
		{"Content-MD5 carried", choice{proved: true, sum: remote.ChecksumMD5, rejected: rejected}, make([]byte, 16), remote.ErrCorrupt},
		{"the ETag compared, nothing carried", choice{proved: true, rejected: rejected}, nil, remote.ErrInvalid},
	} {
		s, _ := storeOn(t, srv, tc.ch, nil)
		if err := s.Put(t.Context(), remote.Name{}, bytes.NewReader(b), 1, tc.sum); !errors.Is(err, tc.want) {
			t.Errorf("Put [%s] answered 400 InvalidRequest = %v, want %v", tc.why, err, tc.want)
		}
	}
}

func TestETagIsMD5(t *testing.T) {
	sum := md5.Sum([]byte("x"))
	h := hex.EncodeToString(sum[:])

	for _, tc := range []struct {
		etag *string
		want bool
	}{
		{new(`"` + h + `"`), true},
		{new(h), true},
		{new(strings.ToUpper(h)), true},
		{new(`"` + strings.ToUpper(h) + `"`), true},
		{new(`"` + h + `-2"`), false},
		{new(h + "-1"), false},
		{new(`"` + h[1:] + `"`), false},
		{new(""), false},
		{nil, false},
	} {
		if got := etagIsMD5(tc.etag, sum[:]); got != tc.want {
			e := "<nil>"
			if tc.etag != nil {
				e = *tc.etag
			}
			t.Errorf("etagIsMD5(%s) = %v, want %v", e, got, tc.want)
		}
	}
}

var errReader = errors.New("the body's reader failed")

func TestGuard(t *testing.T) {
	data := []byte("0123456789")

	for _, tc := range []struct {
		why  string
		src  io.Reader
		size int64
		want error // nil: the whole body, then EOF
		sent int64 // bytes delivered
	}{
		{"exact", bytes.NewReader(data), 10, nil, 10},
		{"exact, a byte per read", iotest.OneByteReader(bytes.NewReader(data)), 10, nil, 10},
		{"exact, data with its EOF", iotest.DataErrReader(bytes.NewReader(data)), 10, nil, 10},
		{"empty", bytes.NewReader(nil), 0, nil, 0},
		{"one byte", bytes.NewReader(data[:1]), 1, nil, 1},
		{"one short", bytes.NewReader(data[:9]), 10, remote.ErrInvalid, 9},
		{"one short, a byte per read", iotest.OneByteReader(bytes.NewReader(data[:9])), 10, remote.ErrInvalid, 9},
		{"empty for one", bytes.NewReader(nil), 1, remote.ErrInvalid, 0},
		// The final byte is held back, so a longer body never reaches the
		// service whole.
		{"one long", bytes.NewReader(data), 9, remote.ErrInvalid, 8},
		{"one long, a byte per read", iotest.OneByteReader(bytes.NewReader(data)), 9, remote.ErrInvalid, 8},
		{"a byte for none", bytes.NewReader(data[:1]), 0, remote.ErrInvalid, 0},
		{"the reader fails", io.MultiReader(bytes.NewReader(data[:4]), iotest.ErrReader(errReader)), 10, errReader, 4},
		{"the reader fails at the last byte", io.MultiReader(bytes.NewReader(data[:9]), iotest.ErrReader(errReader)), 10, errReader, 9},
	} {
		g := &guard{src: tc.src, left: tc.size, md5: md5.New()}

		got, err := io.ReadAll(g)
		if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("guard [%s]: read error %v, want %v", tc.why, err, tc.want)
		}

		if int64(len(got)) != tc.sent || !bytes.Equal(got, data[:len(got)]) {
			t.Errorf("guard [%s]: delivered %q, want the first %d bytes", tc.why, got, tc.sent)
		}

		read, rec := g.stop()
		if read != tc.sent || !errors.Is(rec, tc.want) || (tc.want == nil) != (rec == nil) {
			t.Errorf("guard [%s]: stop = %d, %v; want %d, %v", tc.why, read, rec, tc.sent, tc.want)
		}

		if tc.want == nil {
			if sum := md5.Sum(data[:tc.sent]); !bytes.Equal(g.md5.Sum(nil), sum[:]) {
				t.Errorf("guard [%s]: MD5 %x, want the body's %x", tc.why, g.md5.Sum(nil), sum)
			}
		}

		if n, err := g.Read(make([]byte, 1)); n != 0 || err == nil {
			t.Errorf("guard [%s]: a read after stop = %d, %v; want 0 and an error", tc.why, n, err)
		}
	}
}

func TestOutcome(t *testing.T) {
	for _, tc := range []struct {
		err           error
		result, cause string
	}{
		{nil, "ok", ""},
		{fmt.Errorf("x: %w", remote.ErrNotFound), "not_found", ""},
		{fmt.Errorf("x: %w", remote.ErrInvalid), "invalid", ""},
		{fmt.Errorf("x: %w", &remote.DeniedError{Cause: remote.CauseAccess}), "denied", "access"},
		{fmt.Errorf("x: %w", &remote.DeniedError{Cause: remote.CauseQuota}), "denied", "quota"},
		{fmt.Errorf("x: %w", &remote.DeniedError{Cause: remote.CauseSize}), "denied", "size"},
		{fmt.Errorf("x: %w", remote.ErrThrottled), "throttled", ""},
		{fmt.Errorf("x: %w", remote.ErrTransient), "transient", ""},
		{fmt.Errorf("x: %w", remote.ErrCorrupt), "corrupt", ""},
		{context.Canceled, "local", ""},
		{context.DeadlineExceeded, "local", ""},
		{errReader, "local", ""},
	} {
		if r, c := outcome(tc.err); r != tc.result || c != tc.cause {
			t.Errorf("outcome(%v) = %q, %q; want %q, %q", tc.err, r, c, tc.result, tc.cause)
		}
	}
}

func TestUnservedCallsSendNothing(t *testing.T) {
	srv, n := counting(t, md5ETag)
	s, _ := storeOn(t, srv, choice{proved: true, batch: true}, nil)
	ctx := t.Context()

	if _, _, err := s.Get(ctx, remote.Name{}, remote.Range{}); !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("Get = %v, want ErrInvalid", err)
	}
	if errs := s.Delete(ctx, []remote.Name{{1}, {2}}); len(errs) != 2 || !errors.Is(errs[0], remote.ErrInvalid) || !errors.Is(errs[1], remote.ErrInvalid) {
		t.Errorf("Delete = %v, want two ErrInvalid", errs)
	}
	if _, next, _, err := s.List(ctx, "c", 10); !errors.Is(err, remote.ErrInvalid) || next != "c" {
		t.Errorf("List = %q, %v; want the cursor back, ErrInvalid", next, err)
	}
	if err := s.Health(ctx, remote.DirGet); !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("Health = %v, want ErrInvalid", err)
	}
	if err := s.PutControl(ctx, remote.RoleClaim, []byte("x")); !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("PutControl = %v, want ErrInvalid", err)
	}
	if _, _, err := s.GetControl(ctx, remote.RoleClaim); !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("GetControl = %v, want ErrInvalid", err)
	}

	if got := n.Load(); got != 0 {
		t.Errorf("the service received %d requests, want 0", got)
	}
}
