package s3

import (
	"bytes"
	"crypto/md5"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
)

func TestParseContentRange(t *testing.T) {
	for _, tc := range []struct {
		h             string
		off, n, total int64
		ok            bool
	}{
		{"bytes 0-0/1", 0, 1, 1, true},
		{"bytes 0-99/100", 0, 100, 100, true},
		{"bytes 10-19/100", 10, 10, 100, true},
		{"bytes 99-99/100", 99, 1, 100, true},
		{"bytes 0-9223372036854775806/9223372036854775807", 0, 9223372036854775807, 9223372036854775807, true},

		{"", 0, 0, 0, false},
		{"bytes 10-19/*", 0, 0, 0, false},  // an unknown total
		{"bytes */100", 0, 0, 0, false},    // an unsatisfied range
		{"bytes 10-9/100", 0, 0, 0, false}, // last before first
		{"bytes 0-100/100", 0, 0, 0, false},
		{"bytes 0-9/9", 0, 0, 0, false},
		{"bytes 010-19/100", 0, 0, 0, false},
		{"bytes 10-19/0100", 0, 0, 0, false},
		{"bytes +10-19/100", 0, 0, 0, false},
		{"bytes -10-19/100", 0, 0, 0, false},
		{"bytes 10 -19/100", 0, 0, 0, false},
		{"bytes  10-19/100", 0, 0, 0, false},
		{"Bytes 10-19/100", 0, 0, 0, false},
		{"bytes=10-19/100", 0, 0, 0, false},
		{"bytes 10-19", 0, 0, 0, false},
		{"bytes 10/100", 0, 0, 0, false},
		{"bytes 10-19/100/2", 0, 0, 0, false},
		{"bytes 0-9223372036854775807/9223372036854775808", 0, 0, 0, false},
	} {
		off, n, total, ok := parseContentRange(tc.h)
		if off != tc.off || n != tc.n || total != tc.total || ok != tc.ok {
			t.Errorf("parseContentRange(%q) = %d, %d, %d, %v; want %d, %d, %d, %v", tc.h, off, n, total, ok, tc.off, tc.n, tc.total, tc.ok)
		}
	}
}

// FuzzContentRange holds the reader to its form: any header it accepts names
// a range inside the object, and is exactly what an independent formatter
// writes for that range, so no field is read from outside the header and no
// inclusive end is taken for a length.
func FuzzContentRange(f *testing.F) {
	for _, h := range []string{"bytes 0-0/1", "bytes 10-19/100", "bytes */100", "bytes 1-0/2", "bytes 0-9223372036854775806/9223372036854775807", "bytes 00-1/2"} {
		f.Add(h)
	}

	f.Fuzz(func(t *testing.T, h string) {
		off, n, total, ok := parseContentRange(h)
		if !ok {
			return
		}

		if off < 0 || n < 1 || total < 1 || off > total-n {
			t.Fatalf("parseContentRange(%q) = %d, %d, %d: a range outside the object", h, off, n, total)
		}

		if want := "bytes " + strconv.FormatInt(off, 10) + "-" + strconv.FormatInt(off+n-1, 10) + "/" + strconv.FormatInt(total, 10); want != h {
			t.Fatalf("parseContentRange(%q) = %d, %d, %d, which is written %q", h, off, n, total, want)
		}
	})
}

func TestGetRefusesAnEmptyRangeBeforeAnyRequest(t *testing.T) {
	prof.Watch(t, prof.Options{})
	srv, reqs := counting(t, md5ETag)
	s, rec := storeOn(t, srv, choice{}, nil)

	for _, r := range []remote.Range{{Off: 5}, {Off: -1, Len: 2}, {Off: 1, Len: -1}, {Off: 1<<63 - 1, Len: 1}} {
		if _, _, err := s.Get(t.Context(), remote.Name{1}, r); !errors.Is(err, remote.ErrInvalid) {
			t.Errorf("Get of %+v = %v, want ErrInvalid", r, err)
		}
	}

	if n := reqs.Load(); n != 0 {
		t.Errorf("%d requests reached the server, want none", n)
	}

	got := rec.all()
	if len(got) != 4 {
		t.Fatalf("recorded %+v, want four gets, invalid", got)
	}
	for i, c := range got {
		if c != (recordedCall{"get", "invalid", "", 0}) {
			t.Errorf("recorded call %d %+v, want a get, invalid, of 0 bytes", i, c)
		}
	}

	_ = s.Close()
	if _, _, err := s.Get(t.Context(), remote.Name{1}, remote.Range{}); !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("Get after Close = %v, want ErrInvalid", err)
	}
}

// A reply whose body runs past the Content-Range it declares yields the
// range and then fails, never the bytes after it; a ranged get answered 200
// with a matching Content-Range, whose bytes are the object's first, and a
// whole get answered with no length, fail before any byte.
func TestGetOfAReplyOtherThanDeclared(t *testing.T) {
	prof.Watch(t, prof.Options{})
	body := []byte("0123456789abcdefghij")

	for _, tc := range []struct {
		name   string
		status int
		header map[string]string
		r      remote.Range
		open   error // Get's own error
		read   string
		end    error
	}{
		{"ranged, longer", http.StatusPartialContent, map[string]string{"Content-Range": "bytes 0-9/100"}, remote.Range{Off: 0, Len: 10}, nil, "0123456789", remote.ErrTransient},
		{"ranged, answered 200", http.StatusOK, map[string]string{"Content-Range": "bytes 5-9/100"}, remote.Range{Off: 5, Len: 5}, remote.ErrInvalid, "", nil},
		{"whole, chunked", http.StatusOK, nil, remote.Range{}, remote.ErrTransient, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				switch tc.name {
				case "whole, chunked":
					w.WriteHeader(tc.status)
					w.(http.Flusher).Flush() // no Content-Length: the body goes chunked
				default:
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write(body)
			}, false, nil)
			s, _ := storeOn(t, srv, choice{}, nil)

			rc, _, err := s.Get(t.Context(), remote.Name{1}, tc.r)
			if tc.open != nil {
				if !errors.Is(err, tc.open) || rc != nil {
					t.Fatalf("Get = %v, %v; want %v and no reader", rc, err, tc.open)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer func() { _ = rc.Close() }()

			got, err := io.ReadAll(rc)
			if string(got) != tc.read || !errors.Is(err, tc.end) {
				t.Errorf("read %q, %v; want %q then %v", got, err, tc.read, tc.end)
			}
		})
	}
}

// The server answers a whole-object get with the MD5 of its stored bytes as
// the ETag, and sends other bytes: a store comparing ETags fails at the end.
func TestGetChecksTheETagOfAWholeBlock(t *testing.T) {
	prof.Watch(t, prof.Options{})
	stored, sent := []byte("the stored block"), []byte("the stoned block")

	srv := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", md5ETag(stored))
		_, _ = w.Write(sent)
	}, false, nil)

	stored5 := md5.Sum(stored)
	etagSum := remote.StoredSum{Kind: remote.ChecksumMD5, Sum: stored5[:]}

	for _, tc := range []struct {
		name string
		ch   choice
		want error
		sum  remote.StoredSum
	}{
		{"etag", choice{proved: true, sum: remote.ChecksumNone}, remote.ErrTransient, etagSum},
		{"crc32c", choice{proved: true, sum: remote.ChecksumCRC32C}, nil, remote.StoredSum{}},
		{"unproved", choice{}, nil, remote.StoredSum{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := storeOn(t, srv, tc.ch, nil)

			rc, sum, err := s.Get(t.Context(), remote.Name{1}, remote.Range{})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer func() { _ = rc.Close() }()

			got, err := io.ReadAll(rc)
			if string(got) != string(sent) || !errors.Is(err, tc.want) {
				t.Errorf("read %q, %v; want every byte sent, then %v", got, err, tc.want)
			}

			if sum.Kind != tc.sum.Kind || string(sum.Sum) != string(tc.sum.Sum) {
				t.Errorf("stored sum %v %x, want %v %x", sum.Kind, sum.Sum, tc.sum.Kind, tc.sum.Sum)
			}
		})
	}
}

// eofApart returns its last bytes with a nil error and its end only on the
// next read, as an io.Reader may.
type eofApart struct{ r io.Reader }

func (e eofApart) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if n > 0 && err == io.EOF {
		err = nil
	}

	return n, err
}

func (eofApart) Close() error { return nil }

// A caller reading exactly the length it knows still gets the end's checks,
// from a body that holds its end back past the last byte.
func TestGetChecksTheEndOfAnExactRead(t *testing.T) {
	prof.Watch(t, prof.Options{})
	srv, _ := counting(t, md5ETag)
	s, rec := storeOn(t, srv, choice{}, nil)
	stored, sent := []byte("the stored block"), []byte("the stoned block")
	sum := md5.Sum(stored)

	b := &getBody{s: s, op: opGet, ctx: t.Context(), rc: eofApart{bytes.NewReader(sent)}, left: int64(len(sent)), md5: md5.New(), etag: sum[:]}

	got := make([]byte, len(sent))
	if n, err := b.Read(got); n != len(sent) || !errors.Is(err, remote.ErrTransient) {
		t.Errorf("reading exactly %d bytes = %d, %v; want them all with ErrTransient: the ETag's MD5 differs", len(sent), n, err)
	}

	if err := b.Close(); !errors.Is(err, remote.ErrTransient) {
		t.Errorf("Close after the exact read = %v, want ErrTransient", err)
	}

	if c := rec.all(); len(c) != 1 || c[0] != (recordedCall{"get", "transient", "", int64(len(sent))}) {
		t.Errorf("recorded %+v, want one get, transient, of %d bytes", c, len(sent))
	}
}

// On a store comparing ETags, a reply whose ETag is no MD5 is read unchecked
// and reports no stored sum.
func TestGetOfANonMD5ETagChecksNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	srv, _ := counting(t, func([]byte) string { return `"d41d8cd98f00b204e9800998ecf8427e-2"` })
	s, _ := storeOn(t, srv, choice{proved: true, sum: remote.ChecksumNone}, nil)

	rc, sum, err := s.Get(t.Context(), remote.Name{1}, remote.Range{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if _, err := io.ReadAll(rc); err != nil || sum.Kind != remote.ChecksumNone {
		t.Errorf("read ended %v with the stored sum %v, want nil and none", err, sum.Kind)
	}
}
