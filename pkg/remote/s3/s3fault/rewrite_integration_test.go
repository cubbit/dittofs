//go:build integration

package s3fault

import (
	"bytes"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Each key put through the transport is held out of its own first two
// listings: a, put before the first listing, shows in the third, and b, put
// after it, in the fourth. c, put around the transport, is in every listing,
// and both held keys are stored all along.
func TestHoldNewKeysHoldsEachKeyForItsOwnListings(t *testing.T) {
	r := newRig(t, nil)
	r.tr.Inject(Rule{}, HoldNewKeys(2))
	if _, err := r.admin.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: aws.String("c"), Body: bytes.NewReader([]byte("c"))}); err != nil {
		t.Fatal(err)
	}
	listing := func() []string {
		t.Helper()
		out, err := r.client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: &r.bucket})
		if err != nil {
			t.Fatalf("ListObjectsV2 under HoldNewKeys: %v", err)
		}
		var keys []string
		for _, o := range out.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		return keys
	}

	if err := r.put(t.Context(), "a", []byte("a")); err != nil {
		t.Fatal(err)
	}
	got := [][]string{listing()}
	if err := r.put(t.Context(), "b", []byte("b")); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		got = append(got, listing())
	}
	want := [][]string{{"c"}, {"c"}, {"a", "c"}, {"a", "b", "c"}}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("listing %d lists %q, want %q", i+1, got[i], want[i])
		}
	}
	for _, key := range []string{"a", "b"} {
		if _, ok := r.stored(t, key); !ok {
			t.Errorf("the service does not hold %s, want it stored while held out of listings", key)
		}
	}
}

// A batch of three with one key refused: the reply names that key with the
// code given and the other two as deleted, and the service still holds the
// refused key and neither of the others.
func TestFailBatchKeyRefusesOneKeyForReal(t *testing.T) {
	r := newRig(t, nil)
	keys := []string{"a", "b", "c"}
	for _, key := range keys {
		if _, err := r.admin.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: aws.String(key), Body: bytes.NewReader([]byte(key))}); err != nil {
			t.Fatal(err)
		}
	}
	r.tr.Inject(Rule{Op: "DeleteObjects"}, FailBatchKey("b", "AccessDenied"))
	var objects []types.ObjectIdentifier
	for _, key := range keys {
		objects = append(objects, types.ObjectIdentifier{Key: aws.String(key)})
	}
	out, err := r.client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{Bucket: &r.bucket, Delete: &types.Delete{Objects: objects}})
	if err != nil {
		t.Fatalf("DeleteObjects under FailBatchKey: %v", err)
	}
	if len(out.Errors) != 1 || aws.ToString(out.Errors[0].Key) != "b" || aws.ToString(out.Errors[0].Code) != "AccessDenied" {
		t.Errorf("the batch's errors: %+v, want b alone, refused AccessDenied", out.Errors)
	}
	var deleted []string
	for _, d := range out.Deleted {
		deleted = append(deleted, aws.ToString(d.Key))
	}
	slices.Sort(deleted)
	if want := []string{"a", "c"}; !slices.Equal(deleted, want) {
		t.Errorf("the batch deleted %q, want %q", deleted, want)
	}
	for _, key := range keys {
		if body, ok := r.stored(t, key); ok != (key == "b") || (ok && string(body) != key) {
			t.Errorf("after the batch the service holds %s: %v (%q), want only the refused b held", key, ok, body)
		}
	}
}
