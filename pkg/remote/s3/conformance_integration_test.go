//go:build integration

package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/remotetest"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// The S3 store passes the conformance suite against LocalStack, in the CRC32C
// mode LocalStack enforces, each row over a fresh bucket and prefix, and
// every request of the run is signed with an unsigned payload and carries no
// conditional header (RFC 4 Appendix C.3, §4.3).
//
// The subject backs PutRaw, Reopen and Raw. ReadOnly and Load are left unset,
// so their rows skip: the first needs Health, which the capability check
// brings, and the second a record of every call the store took, which only
// the in-memory store keeps.
func TestConformance(t *testing.T) {
	// Before the suite's watch, for the reason newLSRig gives.
	endpoint, roots := s3fault.LocalStack(t)

	var (
		mu  sync.Mutex
		trs []*s3fault.Transport
	)

	remotetest.Run(t, func(t *testing.T) remotetest.Subject {
		b := s3fault.FreshBucket(t)

		var tr *s3fault.Transport

		c, err := NewClient(ClientConfig{
			Endpoint:        endpoint,
			Bucket:          b.Name,
			AccessKeyID:     "test",
			SecretAccessKey: "test",
			TrustRoots:      roots,
			Limit:           4,
		}, Deps{
			Logger:    slog.New(slog.DiscardHandler),
			Recorder:  &calls{},
			Transport: func(rt http.RoundTripper) http.RoundTripper { tr = s3fault.New(rt, nil); return tr },
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })

		mu.Lock()
		trs = append(trs, tr)
		mu.Unlock()

		prefix := "ns-" + strings.ToLower(rand.Text()) + "/"

		openStore := func() (*store, error) { return open(c, prefix, remote.Mutable, crc) }

		s, err := openStore()
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })

		putRaw := func(ctx context.Context, key string, body []byte) error {
			k := prefix + key
			_, err := b.Admin.PutObject(ctx, &s3.PutObjectInput{Bucket: &b.Name, Key: &k, Body: bytes.NewReader(body)})
			return err
		}

		// A store is returned holding its health object, which the capability
		// check puts; this open has no check, so the object is planted.
		if err := putRaw(t.Context(), "control/health", []byte("planted by the conformance opener")); err != nil {
			t.Fatalf("planting control/health: %v", err)
		}

		return remotetest.Subject{
			Store:  s,
			PutRaw: putRaw,
			Reopen: func(context.Context) (remote.Store, error) { return openStore() },
			Raw: func(ctx context.Context) ([]string, error) {
				var keys []string

				pages := s3.NewListObjectsV2Paginator(b.Admin, &s3.ListObjectsV2Input{Bucket: &b.Name, Prefix: &prefix})
				for pages.HasMorePages() {
					page, err := pages.NextPage(ctx)
					if err != nil {
						return nil, err
					}

					for _, o := range page.Contents {
						keys = append(keys, strings.TrimPrefix(*o.Key, prefix))
					}
				}

				return keys, nil
			},
		}
	})

	mu.Lock()
	defer mu.Unlock()

	checkSigned(t, trs)
}

// conditional are the request headers that make a request conditional.
var conditional = []string{
	"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since",
	"X-Amz-Copy-Source-If-Match", "X-Amz-Copy-Source-If-None-Match",
	"X-Amz-Copy-Source-If-Modified-Since", "X-Amz-Copy-Source-If-Unmodified-Since",
}

// checkSigned fails t on each request the transports saw that was not signed
// UNSIGNED-PAYLOAD or carried a conditional header, and when they saw none.
func checkSigned(t *testing.T, trs []*s3fault.Transport) {
	t.Helper()

	seen, bad := 0, 0

	for _, tr := range trs {
		for _, req := range tr.Requests() {
			seen++

			var why []string
			if v := req.Header.Values("X-Amz-Content-Sha256"); len(v) != 1 || v[0] != "UNSIGNED-PAYLOAD" {
				why = append(why, "x-amz-content-sha256 "+strings.Join(v, ","))
			}

			for _, h := range conditional {
				if v := req.Header.Values(h); len(v) > 0 {
					why = append(why, h+" "+strings.Join(v, ","))
				}
			}

			if len(why) > 0 {
				if bad++; bad <= 10 {
					t.Errorf("%s of %q carried %s, want x-amz-content-sha256 UNSIGNED-PAYLOAD and no conditional header", req.Op, req.Key, strings.Join(why, "; "))
				}
			}
		}
	}

	if bad > 10 {
		t.Errorf("%d of %d requests carried a wrong payload signature or a conditional header, %d more than listed", bad, seen, bad-10)
	}

	if seen == 0 {
		t.Error("the transports saw no request, want every request of the suite")
	}
}
