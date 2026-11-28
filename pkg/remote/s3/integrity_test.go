package s3

import (
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// A reply to a put that is no verdict on the checksum fails the check with
// its own error after that one request: it is never taken for the checksum's
// rejection, which would make every later refusal of a put ErrCorrupt.
func TestProveIntegrityFailsOnAReplyThatIsNoVerdict(t *testing.T) {
	prof.Watch(t, prof.Options{})
	srv, _ := counting(t, md5ETag)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	for _, tc := range []struct {
		rule   s3fault.Rule // the request answered; the zero Rule answers the first
		status int
		code   string
		want   error
		sent   int // requests before the check gives up
	}{
		{s3fault.Rule{Op: "PutObject"}, http.StatusUnauthorized, "", remote.ErrDenied, 1},
		{s3fault.Rule{Op: "PutObject"}, http.StatusForbidden, "AccessDenied", remote.ErrDenied, 1},
		{s3fault.Rule{Op: "PutObject"}, http.StatusForbidden, "SignatureDoesNotMatch", remote.ErrDenied, 1},
		{s3fault.Rule{Op: "PutObject"}, http.StatusMovedPermanently, "PermanentRedirect", remote.ErrDenied, 1},
		{s3fault.Rule{Op: "PutObject"}, http.StatusNotFound, "NoSuchBucket", remote.ErrDenied, 1},
		{s3fault.Rule{Op: "PutObject"}, http.StatusRequestEntityTooLarge, "EntityTooLarge", remote.ErrDenied, 1},
		{s3fault.Rule{Op: "PutObject"}, http.StatusInsufficientStorage, "", remote.ErrDenied, 1},
		{s3fault.Rule{Op: "PutObject"}, http.StatusTooManyRequests, "", remote.ErrThrottled, 1},
		{s3fault.Rule{Op: "PutObject"}, http.StatusServiceUnavailable, "SlowDown", remote.ErrThrottled, 1},
		{s3fault.Rule{Op: "PutObject"}, http.StatusInternalServerError, "InternalError", remote.ErrTransient, 1},
		{s3fault.Rule{Op: "PutObject"}, http.StatusBadRequest, "RequestTimeout", remote.ErrTransient, 1},
	} {
		var tr *s3fault.Transport

		c, err := NewClient(ClientConfig{
			Endpoint: srv.URL, Bucket: "b", AccessKeyID: "AKIDFAKE", SecretAccessKey: "fake-secret", TrustRoots: roots, Limit: 4,
		}, Deps{
			Logger:    slog.New(slog.DiscardHandler),
			Recorder:  &calls{},
			Transport: func(rt http.RoundTripper) http.RoundTripper { tr = s3fault.New(rt, nil); return tr },
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })

		tr.Inject(tc.rule, s3fault.Answer(tc.status, tc.code, nil))

		ch, v, err := proveIntegrity(t.Context(), c, "ns/check/00/", 1<<20)
		if !errors.Is(err, tc.want) {
			t.Errorf("proveIntegrity under %d %s = %v, want %v", tc.status, tc.code, err, tc.want)
		}

		if ch != (choice{}) || v != (verdict{}) {
			t.Errorf("under %d %s: choice %+v and verdict %v, want both zero", tc.status, tc.code, ch, v)
		}

		if got := len(tr.Requests()); got != tc.sent {
			t.Errorf("under %d %s: %d requests, want %d, the last the one answered, no retry", tc.status, tc.code, got, tc.sent)
		}
	}
}

func TestProveIntegrityRefusesNoLargestBlock(t *testing.T) {
	srv, n := counting(t, md5ETag)
	s, _ := storeOn(t, srv, choice{}, nil)

	for _, largest := range []int64{0, -1} {
		if _, _, err := proveIntegrity(t.Context(), s.c, "ns/check/00/", largest); !errors.Is(err, remote.ErrInvalid) {
			t.Errorf("proveIntegrity at largest %d = %v, want ErrInvalid", largest, err)
		}
	}

	if got := n.Load(); got != 0 {
		t.Errorf("the service received %d requests, want 0", got)
	}
}
