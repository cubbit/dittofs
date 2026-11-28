package s3

import (
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

	if got := rec.all(); len(got) != 4 || got[0] != (recordedCall{"get", "invalid", "", 0}) {
		t.Errorf("recorded %+v, want four gets, invalid", got)
	}

	_ = s.Close()
	if _, _, err := s.Get(t.Context(), remote.Name{1}, remote.Range{}); !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("Get after Close = %v, want ErrInvalid", err)
	}
}

// A reply whose body runs past the Content-Range it declares yields the range
// and then fails, never the bytes after it.
func TestGetOfABodyLongerThanItsRangeFails(t *testing.T) {
	prof.Watch(t, prof.Options{})
	body := []byte("0123456789abcdefghij")
	srv := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}, false, nil)
	s, rec := storeOn(t, srv, choice{}, nil)

	rc, _, err := s.Get(t.Context(), remote.Name{1}, remote.Range{Off: 0, Len: 10})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if string(got) != "0123456789" || !errors.Is(err, remote.ErrTransient) {
		t.Errorf("read %q, %v; want the range's 10 bytes then ErrTransient", got, err)
	}

	if calls := rec.all(); len(calls) != 1 || calls[0] != (recordedCall{"get", "transient", "", 10}) {
		t.Errorf("recorded %+v, want one get, transient, of 10 bytes", calls)
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

	for _, tc := range []struct {
		name string
		ch   choice
		want error
	}{
		{"etag", choice{proved: true, sum: remote.ChecksumNone}, remote.ErrTransient},
		{"crc32c", choice{proved: true, sum: remote.ChecksumCRC32C}, nil},
		{"unproved", choice{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := storeOn(t, srv, tc.ch, nil)

			rc, sum, err := s.Get(t.Context(), remote.Name{1}, remote.Range{})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer func() { _ = rc.Close() }()

			got, err := io.ReadAll(rc)
			if string(got) != string(sent) || !errors.Is(err, tc.want) || tc.want == nil && err != nil {
				t.Errorf("read %q, %v; want every byte sent, then %v", got, err, tc.want)
			}

			wantSum := remote.StoredSum{}
			if tc.name == "etag" {
				m := md5.Sum(stored)
				wantSum = remote.StoredSum{Kind: remote.ChecksumMD5, Sum: m[:]}
			}
			if sum.Kind != wantSum.Kind || string(sum.Sum) != string(wantSum.Sum) {
				t.Errorf("stored sum %v %x, want %v %x", sum.Kind, sum.Sum, wantSum.Kind, wantSum.Sum)
			}
		})
	}
}
