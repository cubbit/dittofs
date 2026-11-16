package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// The health probe is one attempt. The monitor that calls it already needs
// several failed probes in a row, so client retries inside one probe only
// stretch it: against an endpoint answering 503 a single probe made ten
// attempts and took minutes. The endpoint here answers 503 once and then 200,
// so a probe that retries reports healthy.
func TestHealthCheck_ReportsTheFirstAnswerWithoutRetrying(t *testing.T) {
	var answered atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if answered.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	store, err := NewFromConfig(context.Background(), Config{
		Bucket:         "probe",
		Region:         "us-east-1",
		Endpoint:       srv.URL,
		AccessKey:      "test",
		SecretKey:      "test",
		ForcePathStyle: true,
		AllowPrivate:   true,
	})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	err = store.HealthCheck(context.Background())

	if err == nil {
		t.Fatal("HealthCheck() = nil, want the first answer's 503")
	}
	var respErr *smithyhttp.ResponseError
	if !errors.As(err, &respErr) || respErr.HTTPStatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("HealthCheck() = %v, want an HTTP 503 response error", err)
	}
}
