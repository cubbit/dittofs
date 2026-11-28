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

// A put that fails rather than answers the checksum, refused on its first
// request, fails the check with its own error after that one request: it is
// never taken for the checksum's rejection, which would make every later
// refusal of a put ErrCorrupt.
func TestProveIntegrityFailsOnAReplyThatIsNoVerdict(t *testing.T) {
	prof.Watch(t, prof.Options{})
	srv, n := counting(t, md5ETag)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	for _, tc := range []struct {
		status int
		code   string
		want   error
	}{
		{http.StatusUnauthorized, "", remote.ErrDenied},
		{http.StatusForbidden, "AccessDenied", remote.ErrDenied},
		{http.StatusForbidden, "SignatureDoesNotMatch", remote.ErrDenied},
		{http.StatusMovedPermanently, "PermanentRedirect", remote.ErrDenied},
		{http.StatusNotFound, "NoSuchBucket", remote.ErrDenied},
		{http.StatusRequestEntityTooLarge, "EntityTooLarge", remote.ErrDenied},
		{http.StatusInsufficientStorage, "", remote.ErrDenied},
		{http.StatusTooManyRequests, "", remote.ErrThrottled},
		{http.StatusServiceUnavailable, "SlowDown", remote.ErrThrottled},
		{http.StatusInternalServerError, "InternalError", remote.ErrTransient},
		{http.StatusBadRequest, "RequestTimeout", remote.ErrTransient},
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

		tr.Inject(s3fault.Rule{Op: "PutObject"}, s3fault.Answer(tc.status, tc.code, nil))

		ch, v, err := proveIntegrity(t.Context(), c, "ns/check/00/", 1<<20)
		if !errors.Is(err, tc.want) {
			t.Errorf("proveIntegrity under %d %s = %v, want %v", tc.status, tc.code, err, tc.want)
		}

		if ch != (choice{}) || v != (verdict{}) {
			t.Errorf("under %d %s: choice %+v and verdict %v, want both zero", tc.status, tc.code, ch, v)
		}

		if got := len(tr.Requests()); got != 1 {
			t.Errorf("under %d %s: %d requests, want the first put alone", tc.status, tc.code, got)
		}
	}

	if got := n.Load(); got != 0 {
		t.Errorf("the service received %d requests, want 0: every put was answered by the fault", got)
	}
}
