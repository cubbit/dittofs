package s3

import (
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
)

// A probe of no direction, of one outside the two, or on a closed store is
// ErrInvalid with no request, recorded once as health; a probe the service
// answers is recorded once as health, never as the control call it rides on.
func TestHealthRecordsOnceAsHealth(t *testing.T) {
	prof.Watch(t, prof.Options{})
	srv, n := counting(t, md5ETag)
	crc := choice{proved: true, sum: remote.ChecksumCRC32C}

	for _, tc := range []struct {
		why    string
		d      remote.Direction
		closed bool
	}{
		{"no direction", 0, false},
		{"a direction outside the two", remote.DirGet + 1, false},
		{"DirPut on a closed store", remote.DirPut, true},
		{"DirGet on a closed store", remote.DirGet, true},
	} {
		s, rec := storeOn(t, srv, crc, nil)
		if tc.closed {
			_ = s.Close()
		}

		if err := s.Health(t.Context(), tc.d); !errors.Is(err, remote.ErrInvalid) {
			t.Errorf("Health [%s] = %v, want ErrInvalid", tc.why, err)
		}

		if got, want := rec.all(), []recordedCall{{"health", "invalid", "", 0}}; !slices.Equal(got, want) {
			t.Errorf("Health [%s] recorded %+v, want %+v", tc.why, got, want)
		}
	}

	if got := n.Load(); got != 0 {
		t.Errorf("the refused probes sent %d requests, want 0", got)
	}

	s, rec := storeOn(t, srv, crc, nil)
	for _, d := range []remote.Direction{remote.DirPut, remote.DirGet} {
		if err := s.Health(t.Context(), d); err != nil {
			t.Errorf("Health(%d) answered 200 = %v, want nil", d, err)
		}
	}

	want := []recordedCall{{"health", "ok", "", int64(len(healthBody))}, {"health", "ok", "", 0}}
	if got := rec.all(); !slices.Equal(got, want) {
		t.Errorf("the two probes recorded %+v, want %+v, each once as health", got, want)
	}

	if got := n.Load(); got != 2 {
		t.Errorf("the two probes sent %d requests, want 2", got)
	}
}
