//go:build integration

package s3fault

import (
	"context"
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// A client that trusts only the pool LocalStack returns completes a request
// over the HTTPS endpoint: a wrong host, or a pool without the served
// certificate, fails the handshake.
func TestLocalStackHTTPS(t *testing.T) {
	endpoint, roots := LocalStack(t)
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		Retryer:      aws.NopRetryer{},
		HTTPClient:   &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}},
	})
	if _, err := client.ListBuckets(context.Background(), &s3.ListBucketsInput{}); err != nil {
		t.Fatalf("ListBuckets at %s trusting only the served certificate: %v", endpoint, err)
	}
}
