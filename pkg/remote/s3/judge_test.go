package s3

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/pkg/remote"
)

// Every feature has a name, the names are distinct, and only batch delete and
// conditional put are recorded without refusing (RFC 4 C.1).
func TestFeatures(t *testing.T) {
	recorded := map[string]bool{"batch_delete": true, "conditional_put": true}
	seen := map[string]bool{}
	for f := range featureCount {
		name := f.String()
		if name == "" || seen[name] {
			t.Errorf("feature %d: name %q empty or repeated", f, name)
		}

		seen[name] = true
		if f.refuses() == recorded[name] {
			t.Errorf("%s.refuses() = %v, want %v", name, f.refuses(), !recorded[name])
		}
	}

	if len(seen) != 12 {
		t.Errorf("features: got %d, want 12", len(seen))
	}
}

var stateNames = [...]string{stateUnverified: "unverified", statePresent: "present", stateMissing: "missing"}

func TestVerdictRefusals(t *testing.T) {
	var v verdict
	if got := v.refusals(); got != "" {
		t.Errorf("all unverified: refusals() = %q, want empty", got)
	}

	// Judged out of check order, so the order of the text is the check's.
	v.judge(featVersioning, false)
	v.judge(featPutIntegrity, false)
	v.judge(featBatchDelete, false)
	v.judge(featConditionalPut, false)
	v.judge(featLifecycle, true)
	v.judge(featList, false)
	if got, want := v.refusals(), "put_integrity, list, versioning"; got != want {
		t.Errorf("refusals() = %q, want %q", got, want)
	}

	if v[featLifecycle] != statePresent || v[featReadAfterWrite] != stateUnverified {
		t.Errorf("lifecycle %s, read_after_write %s: want present, unverified", stateNames[v[featLifecycle]], stateNames[v[featReadAfterWrite]])
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

func expire(f *types.LifecycleRuleFilter) types.LifecycleRule {
	return types.LifecycleRule{
		Status:     types.ExpirationStatusEnabled,
		Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)},
		Filter:     f,
	}
}

func TestJudgeLifecycle(t *testing.T) {
	const largest = 4 << 20
	tag := &types.Tag{Key: aws.String("k"), Value: aws.String("v")}
	disabled := expire(nil)
	disabled.Status = types.ExpirationStatusDisabled
	lowercase := expire(nil)
	lowercase.Status = "enabled"
	abortOnly := types.LifecycleRule{
		Status:                         types.ExpirationStatusEnabled,
		AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(1)},
		NoncurrentVersionExpiration:    &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(1)},
	}
	markersOnly := types.LifecycleRule{
		Status:     types.ExpirationStatusEnabled,
		Expiration: &types.LifecycleExpiration{ExpiredObjectDeleteMarker: aws.Bool(true)},
	}
	byDate := expire(nil)
	byDate.Expiration = &types.LifecycleExpiration{Date: aws.Time(time.Unix(1, 0))}
	transition := types.LifecycleRule{
		Status:      types.ExpirationStatusEnabled,
		Transitions: []types.Transition{{Days: aws.Int32(30), StorageClass: types.TransitionStorageClassGlacier}},
		Filter:      &types.LifecycleRuleFilter{Prefix: aws.String("")},
	}
	legacy := expire(nil)
	legacy.Prefix = aws.String("ns/blocks/") //nolint:staticcheck // a deprecated field a service still answers with
	legacyOther := expire(nil)
	legacyOther.Prefix = aws.String("other/") //nolint:staticcheck // a deprecated field a service still answers with
	size := func(over, under *int64) []types.LifecycleRule {
		return []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{ObjectSizeGreaterThan: over, ObjectSizeLessThan: under}})}
	}

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
		{"empty tag", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{Tag: &types.Tag{}})}, false},
		{"and with a tag", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{Prefix: aws.String("ns/"), Tags: []types.Tag{*tag}}})}, true},
		{"and with an empty tag", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{Prefix: aws.String("ns/"), Tags: []types.Tag{{Key: aws.String("")}}}})}, false},
		{"and prefix under the store's", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{Prefix: aws.String("ns/blocks/"), ObjectSizeGreaterThan: aws.Int64(0)}})}, false},
		{"and prefix another", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{Prefix: aws.String("other/"), ObjectSizeGreaterThan: aws.Int64(0)}})}, true},
		{"size over 1 KiB", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeGreaterThan: aws.Int64(1024)})}, false},
		{"size over the largest object", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeGreaterThan: aws.Int64(largest + 1)})}, true},
		{"size over exactly the largest object", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeGreaterThan: aws.Int64(largest)})}, false},
		{"size over the largest int64", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeGreaterThan: aws.Int64(math.MaxInt64)})}, true},
		{"size under 1 byte", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeLessThan: aws.Int64(0)})}, true},
		{"size under exactly 1 byte", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeLessThan: aws.Int64(1)})}, false},
		{"size under the smallest int64", []types.LifecycleRule{expire(&types.LifecycleRuleFilter{ObjectSizeLessThan: aws.Int64(math.MinInt64)})}, true},
		{"and size range inside", size(aws.Int64(10), aws.Int64(100)), false},
		{"and size range at the top", size(aws.Int64(largest), aws.Int64(largest+1)), false},
		{"and size range above", size(aws.Int64(largest+1), aws.Int64(largest*2)), true},
		{"and size range empty", size(aws.Int64(11), aws.Int64(10)), true},
		{"and size range from below 0", size(aws.Int64(-5), aws.Int64(1)), false},
		{"disabled", []types.LifecycleRule{disabled}, true},
		{"disabled then matching", []types.LifecycleRule{disabled, expire(&types.LifecycleRuleFilter{Prefix: aws.String("ns/")})}, false},
		{"status spelled another way", []types.LifecycleRule{lowercase}, false},
		{"abort and noncurrent only", []types.LifecycleRule{abortOnly}, true},
		{"delete markers only", []types.LifecycleRule{markersOnly}, true},
		{"expiry by date", []types.LifecycleRule{byDate}, false},
		{"transition only", []types.LifecycleRule{transition}, false},
		{"a passing rule then a matching one", []types.LifecycleRule{abortOnly, expire(&types.LifecycleRuleFilter{Prefix: aws.String("ns/blocks/")})}, false},
	} {
		if got := judgeLifecycle(tc.rules, "ns/", largest); got != tc.want {
			t.Errorf("%s: judgeLifecycle = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A largest object below 1 bounds no size, so a rule that matches every
	// size is still seen.
	if judgeLifecycle([]types.LifecycleRule{expire(nil)}, "ns/", 0) {
		t.Error("largest 0: judgeLifecycle admitted a bucket-wide expiry, want refused")
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
		{"a tier the SDK does not know", []types.IntelligentTieringConfiguration{archive(on, nil, "FUTURE_ACCESS")}, false},
		{"archive under the prefix, by and", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{And: &types.IntelligentTieringAndOperator{Prefix: aws.String("ns/blocks/")}}, arch)}, false},
		{"archive off the prefix, by and", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{And: &types.IntelligentTieringAndOperator{Prefix: aws.String("other/")}}, arch)}, true},
		{"archive off the prefix", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{Prefix: aws.String("other/")}, arch)}, true},
		{"archive on a tag", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{Tag: tag}, arch)}, true},
		{"archive on an empty tag", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{Tag: &types.Tag{}}, arch)}, false},
		{"archive, and with a tag", []types.IntelligentTieringConfiguration{archive(on, &types.IntelligentTieringFilter{And: &types.IntelligentTieringAndOperator{Prefix: aws.String("ns/"), Tags: []types.Tag{*tag}}}, arch)}, true},
		{"disabled", []types.IntelligentTieringConfiguration{archive(off, nil, deep)}, true},
		{"disabled then matching", []types.IntelligentTieringConfiguration{archive(off, nil, deep), archive(on, nil, arch)}, false},
		{"status spelled another way", []types.IntelligentTieringConfiguration{archive("enabled", nil, arch)}, false},
		{"no tier", []types.IntelligentTieringConfiguration{{Status: on}}, true},
	} {
		if got := judgeTiering(tc.configs, "ns/"); got != tc.want {
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

// readSetting sends the SDK's read of setting f through c and returns its
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

var outcomeNames = [...]string{settingConfig: "config", settingNone: "none", settingRefused: "refused", settingFailed: "failed"}

// settingRead over the errors the SDK's own deserializer builds from each
// setting read's replies.
func TestSettingRead(t *testing.T) {
	ctx := t.Context()
	xml := "application/xml"
	notFound := func(code string) answer { return answer{http.StatusNotFound, xml, xmlError(code)} }
	check := func(f feature, name string, a answer, want settingOutcome) {
		t.Helper()
		c := sdkClient(serve(t, a), nil)
		if got := settingRead(f, readSetting(ctx, c, f)); got != want {
			t.Errorf("%s read answered %s: settingRead = %s, want %s", f, name, outcomeNames[got], outcomeNames[want])
		}
	}

	settings := []feature{featVersioning, featLifecycle, featTiering, featEncryption}
	for _, f := range settings {
		for _, r := range []struct {
			name   string
			answer answer
			want   settingOutcome
		}{
			{"404 NoSuchBucket", notFound("NoSuchBucket"), settingFailed},
			{"404 another setting's code", notFound("NoSuchTagSet"), settingFailed},
			{"404 no body", answer{http.StatusNotFound, "", ""}, settingFailed},
			{"501 NotImplemented", answer{http.StatusNotImplemented, xml, xmlError("NotImplemented")}, settingNone},
			{"501 no body", answer{http.StatusNotImplemented, "", ""}, settingNone},
			{"403 AccessDenied", answer{http.StatusForbidden, xml, xmlError("AccessDenied")}, settingRefused},
			{"403 proxy page", answer{http.StatusForbidden, "text/html", proxyHTML403}, settingRefused},
			{"401", answer{http.StatusUnauthorized, xml, xmlError("Unauthorized")}, settingFailed},
			{"301 PermanentRedirect", answer{http.StatusMovedPermanently, xml, xmlError("PermanentRedirect")}, settingFailed},
			{"503 ServiceUnavailable", answer{http.StatusServiceUnavailable, xml, xmlError("ServiceUnavailable")}, settingFailed},
			{"500 InternalError", answer{http.StatusInternalServerError, xml, xmlError("InternalError")}, settingFailed},
		} {
			check(f, r.name, r.answer, r.want)
		}
	}

	// Each setting's own not-found code is no configuration; versioning has
	// none, so its own 404 fails.
	for f, code := range map[feature]string{
		featLifecycle:  "NoSuchLifecycleConfiguration",
		featTiering:    "NoSuchConfiguration",
		featEncryption: "ServerSideEncryptionConfigurationNotFoundError",
	} {
		check(f, "404 "+code, notFound(code), settingNone)
	}

	check(featVersioning, "404 NoSuchConfiguration", notFound("NoSuchConfiguration"), settingFailed)

	if got := settingRead(featVersioning, nil); got != settingConfig {
		t.Errorf("settingRead(no error) = %s, want config", outcomeNames[got])
	}

	// No reply at all: the call is cancelled before it is sent.
	c := sdkClient(serve(t, answer{status: http.StatusOK}), nil)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := settingRead(featLifecycle, readSetting(cancelled, c, featLifecycle)); got != settingFailed {
		t.Errorf("settingRead(no reply) = %s, want failed", outcomeNames[got])
	}
}
