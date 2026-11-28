//go:build integration

package s3

import (
	"bytes"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// healthKey is the health object's key of the store openNS opens.
const healthKey = openNS + "control/health"

// openForHealth opens openNS through the checked Open, the call the node
// makes.
func openForHealth(t *testing.T, r *openRig) remote.Store {
	t.Helper()

	s, _, err := r.open(t)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return s
}

// probeOp is the one SDK operation each probe direction sends.
var probeOp = map[remote.Direction]string{remote.DirPut: "PutObject", remote.DirGet: "GetObject"}

// healthProbe makes one probe of d and checks it sent exactly one request, of
// d's op on the health object, and recorded exactly one call, as health with
// result and, when ok, the health body's bytes; it returns the probe's error.
func healthProbe(t *testing.T, r *openRig, s remote.Store, d remote.Direction, result string) error {
	t.Helper()

	reqs, recs := len(r.tr.Requests()), len(r.rec.all())
	err := s.Health(t.Context(), d)

	if got := r.tr.Requests()[reqs:]; len(got) != 1 || got[0].Op != probeOp[d] || got[0].Key != healthKey {
		sent := make([]string, len(got))
		for i, q := range got {
			sent[i] = q.Op + " " + q.Key
		}
		t.Errorf("Health(%s) sent %v, want one %s of %s", probeOp[d], sent, probeOp[d], healthKey)
	}

	var bytes int64
	if result == "ok" {
		bytes = int64(len(healthBody))
	}

	if got := r.rec.all()[recs:]; len(got) != 1 || got[0] != (recordedCall{"health", result, "", bytes}) {
		t.Errorf("Health(%s) recorded %+v, want one health call, %s, of %d bytes", probeOp[d], got, result, bytes)
	}

	return err
}

// On a store fresh from its open, holding no block and never probed for a
// put, the get probe passes; then the put probe does, storing the health
// body. Each probe is one request on the health object, recorded once as
// health, and a probe's failure is its request's, as the error table maps it.
func TestHealthProbesTheHealthObject(t *testing.T) {
	r := newOpenRig(t, nil)
	s := openForHealth(t, r)

	if err := healthProbe(t, r, s, remote.DirGet, "ok"); err != nil {
		t.Errorf("Health(DirGet) before any put probe: %v, want nil", err)
	}

	if err := healthProbe(t, r, s, remote.DirPut, "ok"); err != nil {
		t.Errorf("Health(DirPut): %v, want nil", err)
	}

	if b, ok := r.stored(t, healthKey); !ok || !bytes.Equal(b, healthBody) {
		t.Errorf("the health object holds %q (held %v), want %q", b, ok, healthBody)
	}

	if keys := r.rawKeys(t, openNS); !slices.Equal(keys, []string{healthKey}) {
		t.Errorf("the namespace holds %v after the probes, want only %s", keys, healthKey)
	}

	for _, tc := range []struct {
		status int
		code   string
		result string
		want   error
	}{
		{http.StatusServiceUnavailable, "SlowDown", "throttled", remote.ErrThrottled},
		{http.StatusInternalServerError, "InternalError", "transient", remote.ErrTransient},
	} {
		for d, op := range probeOp {
			remove := r.tr.Inject(s3fault.Rule{Op: op, KeyPrefix: healthKey}, s3fault.Answer(tc.status, tc.code, nil))
			if err := healthProbe(t, r, s, d, tc.result); !errors.Is(err, tc.want) {
				t.Errorf("Health(%s) answered %d %s: %v, want %v", op, tc.status, tc.code, err, tc.want)
			}
			remove()
		}
	}

	// Nothing is logged per probe, a failed one included.
	for _, level := range []string{"WARN", "ERROR"} {
		if l := r.lines(t, level); len(l) != 0 {
			t.Errorf("%s lines %v after the probes, want none", level, l)
		}
	}
}

// The health object deleted behind the store: the get probe is ErrNotFound;
// a reopen's check writes it again, and the get probe passes.
func TestHealthAfterTheObjectIsDeleted(t *testing.T) {
	r := newOpenRig(t, nil)
	s := openForHealth(t, r)

	if _, err := r.admin.DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: &r.bucket, Key: new(healthKey)}); err != nil {
		t.Fatalf("DeleteObject of the health object: %v", err)
	}

	if err := healthProbe(t, r, s, remote.DirGet, "not_found"); !errors.Is(err, remote.ErrNotFound) {
		t.Errorf("Health(DirGet) with the health object deleted: %v, want ErrNotFound", err)
	}

	// The seeded source is spent; the reopen's check draws the same namespace.
	r.c.setCheckSources(nil, bytes.NewReader(checkSeed))

	again, _, err := Open(t.Context(), r.c, openNS, remote.Mutable)
	if err != nil {
		t.Fatalf("Open again: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })

	for name, st := range map[string]remote.Store{"the reopened store": again, "the first store": s} {
		if err := healthProbe(t, r, st, remote.DirGet, "ok"); err != nil {
			t.Errorf("Health(DirGet) on %s after the reopen: %v, want nil", name, err)
		}
	}
}
