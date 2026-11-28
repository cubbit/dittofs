package s3

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
)

// Steps 2 to 4 run only once step 1 proved a mechanism: on a choice that
// proves nothing the store's put refuses the probe's first put, unsent.
func TestProbeRefusesAnUnprovedChoice(t *testing.T) {
	prof.Watch(t, prof.Options{})
	srv, n := counting(t, md5ETag)
	s, _ := storeOn(t, srv, choice{}, nil)

	ch, v, err := probe(t.Context(), s.c, "ns/check/00/", choice{})
	if !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("probe on an unproved choice = %v, want ErrInvalid", err)
	}

	if ch != (choice{}) || v != (verdict{}) {
		t.Errorf("choice %+v and verdict %v, want both zero", ch, v)
	}

	if got := n.Load(); got != 0 {
		t.Errorf("the service received %d requests, want 0", got)
	}
}
