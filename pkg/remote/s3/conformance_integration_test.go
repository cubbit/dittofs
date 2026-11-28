//go:build integration

package s3

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/remotetest"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// The S3 store passes the conformance suite against LocalStack, opened through
// the checked Open as the node opens it, deleting in batches and, with the
// batch refused as unsupported so the check records none, one name per
// request, each row over a fresh bucket and prefix; and every request of the
// run is signed with an unsigned payload and carries no conditional header
// but the one each open's check sends into its check namespace (RFC 4
// Appendix C.3, §4.3).
//
// The subject backs PutRaw, Reopen and Raw. ReadOnly and Load are left unset,
// so their rows skip: the first needs Health, and the second a record of
// every call the store took, which only the in-memory store keeps.
func TestConformance(t *testing.T) {
	// Before the suite's watch, for the reason newLSRig gives.
	endpoint, roots := s3fault.LocalStack(t)

	for _, mode := range []struct {
		name  string
		batch bool
	}{{"batch", true}, {"one_name", false}} {
		t.Run(mode.name, func(t *testing.T) {
			// Run's rows run one after another, so the opener appends unlocked.
			var (
				trs   []*s3fault.Transport
				opens atomic.Int64
			)

			remotetest.Run(t, func(t *testing.T) remotetest.Subject {
				b := s3fault.FreshBucket(t)

				var tr *s3fault.Transport

				c, err := NewClient(ClientConfig{
					Endpoint: endpoint, Bucket: b.Name, AccessKeyID: "test", SecretAccessKey: "test",
					TrustRoots: roots, Limit: 4, MaxBlockSize: openBlock,
				}, Deps{
					Logger:    slog.New(slog.DiscardHandler),
					Recorder:  &calls{},
					Transport: func(rt http.RoundTripper) http.RoundTripper { tr = s3fault.New(rt, nil); return tr },
				})
				if err != nil {
					t.Fatalf("NewClient: %v", err)
				}
				t.Cleanup(func() { _ = c.Close() })
				trs = append(trs, tr)

				if !mode.batch {
					tr.Inject(s3fault.Rule{Op: "DeleteObjects"}, s3fault.Answer(http.StatusNotImplemented, "NotImplemented", nil))
				}

				prefix := "ns-" + hex.EncodeToString(randomBytes(4)) + "/"
				reopen := func(ctx context.Context) (remote.Store, error) {
					opens.Add(1)
					s, _, err := Open(ctx, c, prefix, remote.Mutable)

					return s, err
				}

				s, err := reopen(t.Context())
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				t.Cleanup(func() { _ = s.Close() })

				if got := s.(*store).choice.batch; got != mode.batch {
					t.Fatalf("the check chose batch delete %v, want %v", got, mode.batch)
				}

				return remotetest.Subject{
					Store: s,
					PutRaw: func(ctx context.Context, key string, body []byte) error {
						_, err := b.Admin.PutObject(ctx, &s3.PutObjectInput{Bucket: &b.Name, Key: new(prefix + key), Body: strings.NewReader(string(body))})
						return err
					},
					Reopen: reopen,
					Raw:    func(ctx context.Context) ([]string, error) { return rawUnder(ctx, b, prefix) },
				}
			})

			checkSigned(t, trs, int(opens.Load()))
		})
	}
}

// rawUnder lists every key of b relative to prefix, and fails on a key outside
// it: the bucket is the row's own, so the store wrote it.
func rawUnder(ctx context.Context, b *s3fault.Bucket, prefix string) ([]string, error) {
	var keys []string

	pages := s3.NewListObjectsV2Paginator(b.Admin, &s3.ListObjectsV2Input{Bucket: &b.Name})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, o := range page.Contents {
			k, ok := strings.CutPrefix(*o.Key, prefix)
			if !ok {
				return nil, fmt.Errorf("the bucket holds %q, outside the store's prefix %q", *o.Key, prefix)
			}

			keys = append(keys, k)
		}
	}

	return keys, nil
}

// checkSigned fails t on the first request the transports saw that was not
// signed with SigV4 over x-amz-content-sha256 UNSIGNED-PAYLOAD, or that
// carried a conditional header, but for one PutObject into a check namespace
// per open, and when they saw none.
func checkSigned(t *testing.T, trs []*s3fault.Transport, opens int) {
	t.Helper()

	seen, bad, checks := 0, 0, 0

	for _, tr := range trs {
		for _, req := range tr.Requests() {
			seen++

			if req.Op == "PutObject" && strings.Contains(req.Key, "/check/") && req.Header.Get("If-None-Match") == "*" {
				checks++
				req.Header.Del("If-None-Match")
			}

			if why := unsigned(req.Header); why != "" {
				if bad++; bad == 1 {
					t.Errorf("%s of %q %s, want it signed over x-amz-content-sha256 UNSIGNED-PAYLOAD and no conditional header", req.Op, req.Key, why)
				}
			}
		}
	}

	if bad > 1 {
		t.Errorf("%d of %d requests were wrongly signed or conditional, the first listed", bad, seen)
	}

	if seen == 0 {
		t.Error("the transports saw no request, want every request of the run")
	}

	if checks != opens {
		t.Errorf("%d conditional puts into a check namespace over %d opens, want one per open", checks, opens)
	}
}

// unsigned says what is wrong with h's signing, "" when nothing is.
func unsigned(h http.Header) string {
	if v := h.Values("X-Amz-Content-Sha256"); len(v) != 1 || v[0] != "UNSIGNED-PAYLOAD" {
		return "carried x-amz-content-sha256 " + strings.Join(v, ",")
	}

	auth := h.Get("Authorization")
	_, signed, _ := strings.Cut(auth, "SignedHeaders=")
	signed, _, _ = strings.Cut(signed, ",")

	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || !strings.Contains(";"+signed+";", ";x-amz-content-sha256;") {
		return "carried Authorization " + auth
	}

	for k := range h {
		if strings.HasPrefix(k, "If-") || strings.HasPrefix(k, "X-Amz-Copy-Source-If-") {
			return "carried the conditional header " + k
		}
	}

	return ""
}
