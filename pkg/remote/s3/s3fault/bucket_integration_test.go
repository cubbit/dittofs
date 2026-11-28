//go:build integration

package s3fault

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// These tests pin which bucket settings LocalStack holds, each applied for
// real to a fresh bucket and asserted where it changes what a request
// returns; a setting the service only keeps as configuration is read back.
// An upgrade of LocalStack that changes one fails here.

var bucketBody = []byte("0123456789")

func putK(t *testing.T, b *Bucket, in *s3.PutObjectInput) *s3.PutObjectOutput {
	t.Helper()
	in.Bucket, in.Key, in.Body = &b.Name, aws.String("k"), bytes.NewReader(bucketBody)
	out, err := b.Admin.PutObject(t.Context(), in)
	if err != nil {
		t.Fatalf("PutObject %s/k: %v", b.Name, err)
	}
	return out
}

func getK(t *testing.T, b *Bucket, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	t.Helper()
	in.Bucket, in.Key = &b.Name, aws.String("k")
	out, err := b.Admin.GetObject(t.Context(), in)
	if err == nil {
		t.Cleanup(func() { _ = out.Body.Close() })
	}
	return out, err
}

// Versioning held: two puts of one key leave two versions.
func TestVersioningHeld(t *testing.T) {
	b := FreshBucket(t)
	if _, err := b.Admin.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
		Bucket:                  &b.Name,
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("PutBucketVersioning: %v", err)
	}
	first, second := putK(t, b, &s3.PutObjectInput{}), putK(t, b, &s3.PutObjectInput{})
	out, err := b.Admin.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{Bucket: &b.Name})
	if err != nil {
		t.Fatalf("ListObjectVersions: %v", err)
	}
	if len(out.Versions) != 2 || aws.ToString(first.VersionId) == aws.ToString(second.VersionId) {
		t.Fatalf("after two puts: %d versions, version IDs %q and %q, want 2 versions with distinct IDs",
			len(out.Versions), aws.ToString(first.VersionId), aws.ToString(second.VersionId))
	}
}

// An expiring lifecycle rule held: it reads back as written.
func TestExpiringLifecycleRuleHeld(t *testing.T) {
	b := FreshBucket(t)
	if _, err := b.Admin.PutBucketLifecycleConfiguration(t.Context(), &s3.PutBucketLifecycleConfigurationInput{
		Bucket: &b.Name,
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{
			ID:         aws.String("expire"),
			Status:     types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)},
		}}},
	}); err != nil {
		t.Fatalf("PutBucketLifecycleConfiguration: %v", err)
	}
	out, err := b.Admin.GetBucketLifecycleConfiguration(t.Context(), &s3.GetBucketLifecycleConfigurationInput{Bucket: &b.Name})
	if err != nil {
		t.Fatalf("GetBucketLifecycleConfiguration: %v", err)
	}
	if r := out.Rules; len(r) != 1 || aws.ToString(r[0].ID) != "expire" || r[0].Status != types.ExpirationStatusEnabled ||
		r[0].Filter == nil || r[0].Filter.Prefix == nil || *r[0].Filter.Prefix != "" ||
		r[0].Expiration == nil || aws.ToInt32(r[0].Expiration.Days) != 1 {
		t.Fatalf("lifecycle read back: %+v, want rule expire, enabled, prefix \"\", expiring after 1 day", r)
	}
}

// An intelligent-tiering rule with an archive tier held: it reads back.
func TestArchiveTieringRuleHeld(t *testing.T) {
	b := FreshBucket(t)
	if _, err := b.Admin.PutBucketIntelligentTieringConfiguration(t.Context(), &s3.PutBucketIntelligentTieringConfigurationInput{
		Bucket: &b.Name,
		Id:     aws.String("archive"),
		IntelligentTieringConfiguration: &types.IntelligentTieringConfiguration{
			Id:       aws.String("archive"),
			Status:   types.IntelligentTieringStatusEnabled,
			Tierings: []types.Tiering{{AccessTier: types.IntelligentTieringAccessTierArchiveAccess, Days: aws.Int32(90)}},
		},
	}); err != nil {
		t.Fatalf("PutBucketIntelligentTieringConfiguration: %v", err)
	}
	out, err := b.Admin.ListBucketIntelligentTieringConfigurations(t.Context(), &s3.ListBucketIntelligentTieringConfigurationsInput{Bucket: &b.Name})
	if err != nil {
		t.Fatalf("ListBucketIntelligentTieringConfigurations: %v", err)
	}
	if l := out.IntelligentTieringConfigurationList; len(l) != 1 || aws.ToString(l[0].Id) != "archive" ||
		l[0].Status != types.IntelligentTieringStatusEnabled || len(l[0].Tierings) != 1 ||
		l[0].Tierings[0].AccessTier != types.IntelligentTieringAccessTierArchiveAccess || aws.ToInt32(l[0].Tierings[0].Days) != 90 {
		t.Fatalf("tiering read back: %+v, want configuration archive, enabled, ARCHIVE_ACCESS after 90 days", l)
	}
}

// Default encryption held. A fresh bucket already encrypts under SSE-S3, as
// AWS does since 2023, so the default case applies nothing and pins it;
// SSE-KMS is applied, and SSE-S3 applied over it. Each time an object put
// with no encryption header comes back under the default in force, and its
// ETag stays the MD5 of its bytes, which AWS documents as untrue under
// SSE-KMS: LocalStack cannot show the ETag proof broken, only the
// configuration a check reads.
func TestDefaultEncryptionHeld(t *testing.T) {
	sum := md5.Sum(bucketBody)
	md5hex := `"` + hex.EncodeToString(sum[:]) + `"`
	kms, sse := types.ServerSideEncryptionAwsKms, types.ServerSideEncryptionAes256
	for _, c := range []struct {
		name  string
		apply []types.ServerSideEncryption
		want  types.ServerSideEncryption
	}{
		{"default", nil, sse},
		{"kms", []types.ServerSideEncryption{kms}, kms},
		{"s3-over-kms", []types.ServerSideEncryption{kms, sse}, sse},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := FreshBucket(t)
			for _, alg := range c.apply {
				if _, err := b.Admin.PutBucketEncryption(t.Context(), &s3.PutBucketEncryptionInput{
					Bucket: &b.Name,
					ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
						ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: alg},
					}}},
				}); err != nil {
					t.Fatalf("PutBucketEncryption %s: %v", alg, err)
				}
			}
			putK(t, b, &s3.PutObjectInput{})
			out, err := getK(t, b, &s3.GetObjectInput{})
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}
			if out.ServerSideEncryption != c.want {
				t.Errorf("get reports encryption %q, want the default %q", out.ServerSideEncryption, c.want)
			}
			if hasKey := aws.ToString(out.SSEKMSKeyId) != ""; hasKey != (c.want == kms) {
				t.Errorf("get under %s reports KMS key %q", c.want, aws.ToString(out.SSEKMSKeyId))
			}
			if aws.ToString(out.ETag) != md5hex {
				t.Errorf("ETag %s under %s, want the MD5 %s, as LocalStack 4.13.1 gives", aws.ToString(out.ETag), c.want, md5hex)
			}
		})
	}
}

// An archive storage class held: a get reports the class by refusing the
// object until it is restored, and a head names the class.
func TestArchiveStorageClassHeld(t *testing.T) {
	for _, class := range []types.StorageClass{types.StorageClassGlacier, types.StorageClassDeepArchive} {
		t.Run(string(class), func(t *testing.T) {
			b := FreshBucket(t)
			putK(t, b, &s3.PutObjectInput{StorageClass: class})
			_, err := getK(t, b, &s3.GetObjectInput{})
			var api smithy.APIError
			var resp *smithyhttp.ResponseError
			if !errors.As(err, &api) || api.ErrorCode() != "InvalidObjectState" || !errors.As(err, &resp) || resp.HTTPStatusCode() != 403 {
				t.Errorf("GetObject of a %s object: %v, want 403 InvalidObjectState", class, err)
			}
			head, err := b.Admin.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: &b.Name, Key: aws.String("k")})
			if err != nil {
				t.Fatalf("HeadObject: %v", err)
			}
			if head.StorageClass != class {
				t.Errorf("head reports class %q, want %q", head.StorageClass, class)
			}
		})
	}
}

// A stored checksum held: a get sent with x-amz-checksum-mode: ENABLED
// returns the CRC32C of the stored bytes, the one the put sent, and a get
// without it returns none.
func TestStoredChecksumReturnedOnGet(t *testing.T) {
	b := FreshBucket(t)
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], crc32.Checksum(bucketBody, crc32.MakeTable(crc32.Castagnoli)))
	want := base64.StdEncoding.EncodeToString(raw[:])
	putK(t, b, &s3.PutObjectInput{ChecksumCRC32C: &want})
	out, err := getK(t, b, &s3.GetObjectInput{ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		t.Fatalf("GetObject with checksum mode: %v", err)
	}
	if got := aws.ToString(out.ChecksumCRC32C); got != want {
		t.Errorf("get with checksum mode returns CRC32C %q, want the stored %q", got, want)
	}
	out, err = getK(t, b, &s3.GetObjectInput{})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if got := aws.ToString(out.ChecksumCRC32C); got != "" {
		t.Errorf("get without checksum mode returns CRC32C %q, want none", got)
	}
}

// Two fresh buckets are two buckets, and neither outlives its test, even
// with versions and a delete marker left in it.
func TestFreshBucketsAreDistinctAndRemoved(t *testing.T) {
	var names []string
	t.Run("two", func(t *testing.T) {
		for range 2 {
			b := FreshBucket(t)
			names = append(names, b.Name)
			if _, err := b.Admin.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
				Bucket:                  &b.Name,
				VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
			}); err != nil {
				t.Fatalf("PutBucketVersioning: %v", err)
			}
			putK(t, b, &s3.PutObjectInput{})
			putK(t, b, &s3.PutObjectInput{})
			if _, err := b.Admin.DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: &b.Name, Key: aws.String("k")}); err != nil {
				t.Fatalf("DeleteObject: %v", err)
			}
		}
	})
	if len(names) != 2 || names[0] == names[1] {
		t.Fatalf("two FreshBucket calls named %q, want two distinct buckets", names)
	}
	// A third bucket only lends its client; the two above are gone by now.
	admin := FreshBucket(t).Admin
	for _, name := range names {
		_, err := admin.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String(name)})
		var missing *types.NotFound
		if !errors.As(err, &missing) {
			t.Errorf("HeadBucket %s after its test: %v, want NotFound", name, err)
		}
	}
}
