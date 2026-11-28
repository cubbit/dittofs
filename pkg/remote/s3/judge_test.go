package s3

import (
	"context"
	"math"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/pkg/remote"
)

// Every feature has a name, the names are distinct, and only batch delete and
// conditional put are recorded without refusing.
func TestFeatures(t *testing.T) {
	seen := map[string]bool{}
	for f := range featureCount {
		name := f.String()
		if name == "" || seen[name] {
			t.Errorf("feature %d: name %q empty or repeated", f, name)
		}

		seen[name] = true
		if want := f != featBatchDelete && f != featConditionalPut; f.refuses() != want {
			t.Errorf("%s.refuses() = %v, want %v", f, f.refuses(), want)
		}
	}

	if len(seen) != 12 {
		t.Errorf("features: got %d, want 12", len(seen))
	}
}

func TestVerdictMissing(t *testing.T) {
	var v verdict
	if got := v.missing(); got != "" {
		t.Errorf("all unverified: missing() = %q, want empty", got)
	}

	// Set out of check order, so the order of the text is the check's.
	v.judge(featVersioning, false)
	v.judge(featPutIntegrity, false)
	v.judge(featBatchDelete, false)
	v.judge(featConditionalPut, false)
	v.judge(featLifecycle, true)
	v.judge(featList, false)
	if got, want := v.missing(), "put_integrity, list, versioning"; got != want {
		t.Errorf("missing() = %q, want %q", got, want)
	}

	if v[featLifecycle] != present || v[featReadAfterWrite] != unverified {
		t.Errorf("lifecycle %d, read_after_write %d: want present, unverified", v[featLifecycle], v[featReadAfterWrite])
	}

	v.judge(featPutIntegrity, true)
	if v[featPutIntegrity] != present {
		t.Errorf("put_integrity judged again: got %d, want present", v[featPutIntegrity])
	}
}

func TestJudgeVersioning(t *testing.T) {
	for _, tc := range []struct {
		status types.BucketVersioningStatus
		want   bool
	}{
		{"", true},
		{types.BucketVersioningStatusEnabled, false},
		{types.BucketVersioningStatusSuspended, false},
	} {
		if got := judgeVersioning(tc.status); got != tc.want {
			t.Errorf("judgeVersioning(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

const (
	nsPrefix = "ns/"
	maxBlock = 4 << 20
)

func expire(f *types.LifecycleRuleFilter) types.LifecycleRule {
	return types.LifecycleRule{
		Status:     types.ExpirationStatusEnabled,
		Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)},
		Filter:     f,
	}
}

func TestJudgeLifecycle(t *testing.T) {
	tag := &types.Tag{Key: aws.String("k"), Value: aws.String("v")}
	disabled := expire(nil)
	disabled.Status = types.ExpirationStatusDisabled
	abortOnly := types.LifecycleRule{
		Status:                         types.ExpirationStatusEnabled,
		AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(1)},
		NoncurrentVersionExpiration:    &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(1)},
	}
	transition := types.LifecycleRule{
		Status:      types.ExpirationStatusEnabled,
		Transitions: []types.Transition{{Days: aws.Int32(30), StorageClass: types.TransitionStorageClassGlacier}},
		Filter:      &types.LifecycleRuleFilter{Prefix: aws.String("")},
	}
	legacy := expire(nil)
	legacy.Prefix = aws.String("ns/blocks/") //nolint:staticcheck // a deprecated field a service still answers with
	legacyOther := expire(nil)
	legacyOther.Prefix = aws.String("other/") //nolint:staticcheck // a deprecated field a service still answers with

	for _, tc := range []struct {
		name  string
		rules []types.LifecycleRule
		want  bool
	}{
		{"no rules", nil, true},
		{"no filter", []types.LifecycleRule{expire(nil)}, false},
		{"empty filter", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{})}, false},
		{"prefix empty", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{Prefix: aws.String("")})}, false},
		{"prefix the store's", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{Prefix: aws.String("ns/")})}, false},
		{"prefix under the store's", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{Prefix: aws.String("ns/blocks/")})}, false},
		{"prefix shorter than the store's", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{Prefix: aws.String("n")})}, false},
		{"prefix another", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{Prefix: aws.String("other/")})}, true},
		{"prefix a sibling", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{Prefix: aws.String("ns2/")})}, true},
		{"deprecated prefix under the store's", []types.LifecycleRule{legacy}, false},
		{"deprecated prefix another", []types.LifecycleRule{legacyOther}, true},
		{"tag only", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{Tag: tag})}, true},
		{"and with a tag", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{Prefix: aws.String("ns/"), Tags: []types.Tag{*tag}}})}, true},
		{"and prefix under the store's", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{Prefix: aws.String("ns/blocks/"), ObjectSizeGreaterThan: aws.Int64(0)}})}, false},
		{"and prefix another", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{Prefix: aws.String("other/"), ObjectSizeGreaterThan: aws.Int64(0)}})}, true},
		{"size over 1 KiB", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeGreaterThan: aws.Int64(1024)})}, false},
		{"size under 1 KiB", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeLessThan: aws.Int64(1024)})}, false},
		{"size over the largest block", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeGreaterThan: aws.Int64(maxBlock)})}, true},
		{"size over one less than the largest block", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeGreaterThan: aws.Int64(maxBlock - 1)})}, false},
		{"size under 1 byte", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeLessThan: aws.Int64(1)})}, true},
		{"size under 2 bytes", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeLessThan: aws.Int64(2)})}, false},
		{"size over the largest int64", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeGreaterThan: aws.Int64(math.MaxInt64)})}, true},
		{"size under the smallest int64", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeLessThan: aws.Int64(math.MinInt64)})}, true},
		{"and size range inside", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{ObjectSizeGreaterThan: aws.Int64(10), ObjectSizeLessThan: aws.Int64(100)}})}, false},
		{"and size range above", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{ObjectSizeGreaterThan: aws.Int64(maxBlock), ObjectSizeLessThan: aws.Int64(maxBlock * 2)}})}, true},
		{"and size range empty", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{ObjectSizeGreaterThan: aws.Int64(10), ObjectSizeLessThan: aws.Int64(11)}})}, true},
		{"disabled", []types.LifecycleRule{disabled}, true},
		{"abort and noncurrent only", []types.LifecycleRule{abortOnly}, true},
		{"transition only", []types.LifecycleRule{transition}, false},
		{"a passing rule then a matching one", []types.LifecycleRule{abortOnly, expire(&types.LifecycleRuleFilter{Prefix: aws.String("ns/blocks/")})}, false},
	} {
		if got := judgeLifecycle(tc.rules, nsPrefix, maxBlock); got != tc.want {
			t.Errorf("%s: judgeLifecycle = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func archive(status types.IntelligentTieringStatus, f *types.IntelligentTieringFilter, tier types.IntelligentTieringAccessTier) types.IntelligentTieringConfiguration {
	return types.IntelligentTieringConfiguration{
		Id:       aws.String("c"),
		Status:   status,
		Filter:   f,
		Tierings: []types.Tiering{{AccessTier: tier, Days: aws.Int32(90)}},
	}
}

func TestJudgeTiering(t *testing.T) {
	on, off := types.IntelligentTieringStatusEnabled, types.IntelligentTieringStatusDisabled
	deep := types.IntelligentTieringAccessTierDeepArchiveAccess
	arch := types.IntelligentTieringAccessTierArchiveAccess
	tag := &types.Tag{Key: aws.String("k"), Value: aws.String("v")}
	for _, tc := range []struct {
		name    string
		configs []types.IntelligentTieringConfiguration
		want    bool
	}{
		{"none", nil, true},
		{"archive, no filter", []types.IntelligentTieringConfiguration{archive(on, nil, arch)}, false},
		{"deep archive on the prefix", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{Prefix: aws.String("ns/")}, deep)}, false},
		{"archive under the prefix, by and", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{And: &types.IntelligentTieringAndOperator{Prefix: aws.String("ns/blocks/")}}, arch)}, false},
		{"archive off the prefix", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{Prefix: aws.String("other/")}, arch)}, true},
		{"archive on a tag", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{Tag: tag}, arch)}, true},
		{"archive, and with a tag", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{And: &types.IntelligentTieringAndOperator{Prefix: aws.String("ns/"), Tags: []types.Tag{*tag}}}, arch)}, true},
		{"disabled", []types.IntelligentTieringConfiguration{archive(off, nil, deep)}, true},
		{"no archive tier", []types.IntelligentTieringConfiguration{{Status: on}}, true},
	} {
		if got := judgeTiering(tc.configs, nsPrefix); got != tc.want {
			t.Errorf("%s: judgeTiering = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestJudgeEncryption(t *testing.T) {
	def := func(alg types.ServerSideEncryption) *types.ServerSideEncryptionConfiguration {
		return &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: alg},
		}}}
	}

	for _, tc := range []struct {
		name string
		def  *types.ServerSideEncryptionConfiguration
		etag bool // what the ETag admits; a checksum admits every default
	}{
		{"none", nil, true},
		{"no default in the rule", &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{BucketKeyEnabled: aws.Bool(true)}}}, true},
		{"AES256", def(types.ServerSideEncryptionAes256), true},
		{"aws:kms", def(types.ServerSideEncryptionAwsKms), false},
		{"aws:kms:dsse", def(types.ServerSideEncryptionAwsKmsDsse), false},
		{"an algorithm not known", def("aws:future"), false},
	} {
		for _, sum := range []remote.Checksum{remote.ChecksumNone, remote.ChecksumCRC32C, remote.ChecksumMD5} {
			want := tc.etag || sum != remote.ChecksumNone
			if got := judgeEncryption(tc.def, sum); got != want {
				t.Errorf("%s under checksum %d: judgeEncryption = %v, want %v", tc.name, sum, got, want)
			}
		}
	}
}

func TestJudgeClass(t *testing.T) {
	for _, tc := range []struct {
		reported types.StorageClass
		pinned   string
		want     bool
	}{
		{"", "STANDARD", true},
		{types.StorageClassStandard, "STANDARD", true},
		{"", "STANDARD_IA", false},
		{types.StorageClassStandardIa, "STANDARD", false},
		{types.StorageClassGlacier, "GLACIER", false},
		{types.StorageClassDeepArchive, "DEEP_ARCHIVE", false},
		{"PROVIDER_COLD", "PROVIDER_COLD", false},
		{types.StorageClassOnezoneIa, "ONEZONE_IA", true},
		{types.StorageClassIntelligentTiering, "INTELLIGENT_TIERING", true},
	} {
		if got := judgeClass(tc.reported, tc.pinned); got != tc.want {
			t.Errorf("judgeClass(%q, %q) = %v, want %v", tc.reported, tc.pinned, got, tc.want)
		}
	}

	for class, want := range map[string]string{
		"ONEZONE_IA":      "one zone",
		"EXPRESS_ONEZONE": "one zone",
		"STANDARD":        "as the service documents",
		"STANDARD_IA":     "as the service documents",
	} {
		if got := failureDomain(class); got != want {
			t.Errorf("failureDomain(%q) = %q, want %q", class, got, want)
		}
	}
}

// readSetting sends the SDK's read of setting f to endpoint and returns its
// error.
func readSetting(ctx context.Context, c *s3.Client, f feature) error {
	b := aws.String("b")
	var err error
	switch f {
	case featVersioning:
		_, err = c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: b})
	case featLifecycle:
		_, err = c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: b})
	case featTiering:
		_, err = c.ListBucketIntelligentTieringConfigurations(ctx, &s3.ListBucketIntelligentTieringConfigurationsInput{Bucket: b})
	case featEncryption:
		_, err = c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: b})
	default:
		panic("no setting read for " + f.String())
	}

	return err
}

// settingRead over the errors the SDK's own deserializer builds from each
// setting read's replies.
func TestSettingRead(t *testing.T) {
	ctx := t.Context()
	settings := []feature{featVersioning, featLifecycle, featTiering, featEncryption}
	xml := "application/xml"
	notFound := func(code string) answer { return answer{http.StatusNotFound, xml, xmlError(code)} }

	type row struct {
		name   string
		answer answer
		want   func(f feature) read
	}

	always := func(r read) func(feature) read { return func(feature) read { return r } }
	ownCode := func(f feature) answer {
		if code, ok := settingNotFound[f]; ok {
			return notFound(code)
		}

		return notFound("NoSuchConfiguration")
	}

	rows := []row{
		{"404 NoSuchBucket", notFound("NoSuchBucket"), always(readFailed)},
		{"404 another setting's code", notFound("NoSuchTagSet"), always(readFailed)},
		{"404 no body", answer{http.StatusNotFound, "", ""}, always(readFailed)},
		{"501 NotImplemented", answer{http.StatusNotImplemented, xml, xmlError("NotImplemented")}, always(readNone)},
		{"501 no body", answer{http.StatusNotImplemented, "", ""}, always(readNone)},
		{"403 AccessDenied", answer{http.StatusForbidden, xml, xmlError("AccessDenied")}, always(readRefused)},
		{"403 proxy page", answer{http.StatusForbidden, "text/html", proxyHTML403}, always(readRefused)},
		{"401", answer{http.StatusUnauthorized, xml, xmlError("Unauthorized")}, always(readFailed)},
		{"301 PermanentRedirect", answer{http.StatusMovedPermanently, xml, xmlError("PermanentRedirect")}, always(readFailed)},
		{"503 ServiceUnavailable", answer{http.StatusServiceUnavailable, xml, xmlError("ServiceUnavailable")}, always(readFailed)},
		{"500 InternalError", answer{http.StatusInternalServerError, xml, xmlError("InternalError")}, always(readFailed)},
	}

	for _, f := range settings {
		own := ownCode(f)
		_, hasCode := settingNotFound[f]
		rows := append(rows, row{"404 its own not-found code", own, func(feature) read {
			if hasCode {
				return readNone
			}

			return readFailed
		}})

		for _, r := range rows {
			c := sdkClient(serve(t, r.answer), nil)
			if got, want := settingRead(f, readSetting(ctx, c, f)), r.want(f); got != want {
				t.Errorf("%s read answered %s: settingRead = %d, want %d", f, r.name, got, want)
			}
		}
	}

	if got := settingRead(featVersioning, nil); got != readConfig {
		t.Errorf("settingRead(no error) = %d, want readConfig", got)
	}

	// No reply at all: the call is cancelled before it is sent.
	c := sdkClient(serve(t, answer{status: http.StatusOK}), nil)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := settingRead(featLifecycle, readSetting(cancelled, c, featLifecycle)); got != readFailed {
		t.Errorf("settingRead(no reply) = %d, want readFailed", got)
	}
}
