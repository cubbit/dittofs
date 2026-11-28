package s3

import (
	"bytes"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
)

func TestControlRefusesBeforeAnyRequest(t *testing.T) {
	prof.Watch(t, prof.Options{})
	srv, n := counting(t, md5ETag)
	crc := choice{proved: true, sum: remote.ChecksumCRC32C}

	for _, tc := range []struct {
		why    string
		ch     choice
		closed bool
		role   remote.Role
		size   int
		get    bool // GetControl is refused too
	}{
		{"no role", crc, false, remote.Role(0), 1, true},
		{"a role outside the set", crc, false, remote.RoleKeys + 1, 1, true},
		{"a closed store", crc, true, remote.RoleClaim, 1, true},
		{"a body over the cap", crc, false, remote.RoleKeys, maxControl + 1, false},
		{"nothing proved", choice{}, false, remote.RoleClaim, 1, false},
	} {
		s, rec := storeOn(t, srv, tc.ch, nil)
		if tc.closed {
			_ = s.Close()
		}

		if err := s.PutControl(t.Context(), tc.role, make([]byte, tc.size)); !errors.Is(err, remote.ErrInvalid) {
			t.Errorf("PutControl [%s] = %v, want ErrInvalid", tc.why, err)
		}

		want := []recordedCall{{"put_control", "invalid", "", 0}}

		if tc.get {
			if b, v, err := s.GetControl(t.Context(), tc.role); !errors.Is(err, remote.ErrInvalid) || b != nil || v != "" {
				t.Errorf("GetControl [%s] = %d bytes, %q, %v; want nothing, ErrInvalid", tc.why, len(b), v, err)
			}

			want = append(want, recordedCall{"get_control", "invalid", "", 0})
		}

		if got := rec.all(); !slices.Equal(got, want) {
			t.Errorf("[%s] recorded %+v, want %+v", tc.why, got, want)
		}
	}

	if got := n.Load(); got != 0 {
		t.Errorf("the service received %d requests, want 0", got)
	}

	// A body of exactly the cap is sent.
	s, rec := storeOn(t, srv, crc, nil)
	if err := s.PutControl(t.Context(), remote.RoleKeys, make([]byte, maxControl)); err != nil {
		t.Errorf("PutControl of the cap = %v, want nil", err)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("PutControl of the cap sent %d requests, want 1", got)
	}
	if got := rec.all(); len(got) != 1 || got[0] != (recordedCall{"put_control", "ok", "", maxControl}) {
		t.Errorf("PutControl of the cap recorded %+v, want one put_control, ok, of the cap", got)
	}
}

// A control object the service declares longer than the cap is refused from
// its Content-Length and recorded as refused; one of exactly the cap is read.
func TestGetControlJudgesTheCapFromTheDeclaredLength(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var size atomic.Int64

	srv := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		b := bytes.Repeat([]byte{'c'}, int(size.Load()))
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("ETag", md5ETag(b))
		_, _ = w.Write(b)
	}, false, nil)

	for _, tc := range []struct {
		size int64
		want error
		rec  recordedCall
	}{
		{maxControl, nil, recordedCall{"get_control", "ok", "", maxControl}},
		{maxControl + 1, remote.ErrInvalid, recordedCall{"get_control", "invalid", "", 0}},
	} {
		size.Store(tc.size)
		s, rec := storeOn(t, srv, choice{proved: true, sum: remote.ChecksumNone}, nil)

		b, _, err := s.GetControl(t.Context(), remote.RoleKeys)
		if tc.want == nil && (err != nil || int64(len(b)) != tc.size) {
			t.Errorf("GetControl of %d bytes = %d bytes, %v; want every byte", tc.size, len(b), err)
		}
		if tc.want != nil && (!errors.Is(err, tc.want) || b != nil) {
			t.Errorf("GetControl of %d bytes = %d bytes, %v; want nothing, %v", tc.size, len(b), err, tc.want)
		}

		if got := rec.all(); len(got) != 1 || got[0] != tc.rec {
			t.Errorf("GetControl of %d bytes recorded %+v, want %+v", tc.size, got, tc.rec)
		}
	}
}
