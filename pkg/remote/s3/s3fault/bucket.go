package s3fault

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Bucket is a fresh LocalStack bucket for one test; Admin reaches it without
// the transport, to apply the bucket's settings for real and to read back
// what the service stored.
type Bucket struct {
	Name  string
	Admin *s3.Client
}

// FreshBucket creates a bucket of its own on LocalStack's HTTPS endpoint,
// with only the service's defaults (SSE-S3 default encryption among them),
// and at cleanup deletes every version and delete marker, then the bucket.
// It aborts no multipart upload, since nothing that uses it starts one. It
// skips or fails as LocalStack does when the endpoint is unset.
func FreshBucket(tb testing.TB) *Bucket {
	tb.Helper()
	endpoint, roots := LocalStack(tb)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	tb.Cleanup(tr.CloseIdleConnections) // runs after the bucket's removal
	b := &Bucket{
		// rand.Text is base32, so lowered it is a valid bucket name.
		Name: "s3fault-" + strings.ToLower(rand.Text()),
		Admin: s3.New(s3.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(endpoint),
			UsePathStyle: true,
			Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
			Retryer:      aws.NopRetryer{},
			// The checksums a test sends are its own, as the store's are.
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
			HTTPClient:                 &http.Client{Timeout: 30 * time.Second, Transport: tr},
		}),
	}
	if _, err := b.Admin.CreateBucket(tb.Context(), &s3.CreateBucketInput{Bucket: &b.Name}); err != nil {
		tb.Fatalf("creating bucket %s at %s: %v", b.Name, endpoint, err)
	}
	tb.Cleanup(func() {
		if err := b.remove(context.Background()); err != nil {
			tb.Errorf("removing bucket %s: %v", b.Name, err)
		}
	})
	return b
}

// remove deletes every version and delete marker, so that a versioned bucket
// empties too, then the bucket.
func (b *Bucket) remove(ctx context.Context) error {
	pages := s3.NewListObjectVersionsPaginator(b.Admin, &s3.ListObjectVersionsInput{Bucket: &b.Name})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return err
		}
		var ids []types.ObjectIdentifier
		for _, v := range page.Versions {
			ids = append(ids, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range page.DeleteMarkers {
			ids = append(ids, types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if len(ids) == 0 {
			continue
		}
		// A page holds at most 1,000 entries, a batch delete's limit.
		out, err := b.Admin.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &b.Name, Delete: &types.Delete{Objects: ids}})
		if err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("%d keys not deleted, first %s: %s %s", len(out.Errors), aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
		}
	}
	_, err := b.Admin.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &b.Name})
	return err
}
