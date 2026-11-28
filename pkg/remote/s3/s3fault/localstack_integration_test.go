//go:build integration

package s3fault

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// A client that trusts only the pool LocalStack returns completes a request
// over the HTTPS endpoint, and one trusting an empty pool fails its
// handshake, so the pass is the pool's and not the system's roots'.
func TestLocalStackHTTPS(t *testing.T) {
	endpoint, roots := LocalStack(t)
	if roots == nil {
		t.Fatal("LocalStack returned no pool, so a client would trust the system's roots")
	}
	list := func(roots *x509.CertPool) error {
		client := s3.New(s3.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(endpoint),
			UsePathStyle: true,
			Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
			Retryer:      aws.NopRetryer{},
			HTTPClient:   &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}},
		})
		_, err := client.ListBuckets(t.Context(), &s3.ListBucketsInput{})
		return err
	}
	if err := list(roots); err != nil {
		t.Fatalf("ListBuckets at %s trusting only the served certificate: %v", endpoint, err)
	}
	var unknown x509.UnknownAuthorityError
	if err := list(x509.NewCertPool()); !errors.As(err, &unknown) {
		t.Fatalf("ListBuckets at %s trusting an empty pool: %v, want x509.UnknownAuthorityError", endpoint, err)
	}
}
