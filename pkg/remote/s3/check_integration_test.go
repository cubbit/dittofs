//go:build integration

package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
	"github.com/cubbit/dittofs/pkg/sim"
)

// openBlock is the largest block of a client whose open the tests check: above
// the conformance suite's block, small enough that the two puts of it each
// open makes stay cheap.
const openBlock = 256 << 10

// openNS is the prefix every open test opens.
const openNS = "ns/"

// checkSeed is the random source the tests seed the check namespace from, and
// checkNS the namespace it draws.
var (
	checkSeed = []byte{0, 1, 2, 3, 4, 5, 6, 7}
	checkNS   = openNS + "check/0001020304050607/"
)

// openRig is a client on a fresh LocalStack bucket, every request through a
// recording fault transport, its recorder and its log kept.
type openRig struct {
	*lsRig
	c   *Client
	tr  *s3fault.Transport
	rec *calls
	log *logs
}

// newOpenRig builds the rig over a fresh bucket; mod changes the client's
// configuration before it is built.
func newOpenRig(t *testing.T, mod func(*ClientConfig)) *openRig {
	t.Helper()

	r := &openRig{lsRig: newGetRig(t), rec: &calls{}, log: &logs{}}

	cfg := ClientConfig{
		Endpoint:        r.endpoint,
		Bucket:          r.bucket,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		TrustRoots:      r.roots,
		Limit:           4,
		MaxBlockSize:    openBlock,
	}
	if mod != nil {
		mod(&cfg)
	}

	c, err := NewClient(cfg, Deps{
		Logger:    slog.New(slog.NewJSONHandler(r.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Recorder:  r.rec,
		Transport: func(rt http.RoundTripper) http.RoundTripper { r.tr = s3fault.New(rt, nil); return r.tr },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	c.setCheckSources(nil, bytes.NewReader(checkSeed))
	r.c = c

	return r
}

// capabilities is every feature verdict the sink was told of.
func (c *calls) capabilities() []capability {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]capability(nil), c.caps...)
}

// open opens openNS through Open, the call the node makes, closing the store
// at cleanup.
func (r *openRig) open(t *testing.T) (remote.Store, Claim, error) {
	t.Helper()

	s, claim, err := Open(t.Context(), r.c, openNS, remote.Mutable)
	if s != nil {
		t.Cleanup(func() { _ = s.Close() })
	}

	return s, claim, err
}

// leftUnderCheck fails t on any key the service holds under the prefix's
// check/.
func (r *openRig) leftUnderCheck(t *testing.T) {
	t.Helper()

	if keys := r.rawKeys(t, openNS+"check/"); len(keys) != 0 {
		t.Errorf("the service holds %v under the check namespace, want nothing", keys)
	}
}

// capabilities checks the recorder holds exactly one verdict per feature, all
// for openNS, each present unless it is named in missing.
func (r *openRig) capabilities(t *testing.T, missing ...feature) {
	t.Helper()

	got := r.rec.capabilities()
	if len(got) != int(featureCount) {
		t.Errorf("%d capability records, want one per feature, %d: %+v", len(got), featureCount, got)
	}

	for f := range featureCount {
		want := capability{openNS, f.String(), !slices.Contains(missing, f)}
		if n := slices.Index(got, want); n < 0 {
			t.Errorf("no capability record %+v among %+v", want, got)
		}
	}
}

// lines are the log records at level, as JSON maps.
func (r *openRig) lines(t *testing.T, level string) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, rec := range r.log.records(t) {
		if rec["level"] == level {
			out = append(out, rec)
		}
	}

	return out
}

// refused checks err is the refusal naming exactly named, in check order,
// logged once at Error, and that every feature was recorded, present but for
// named and absent, features missing that refuse nothing.
func (r *openRig) refused(t *testing.T, s remote.Store, err error, named []feature, absent ...feature) {
	t.Helper()

	names := make([]string, len(named))
	for i, f := range named {
		names[i] = f.String()
	}

	want := "service lacks " + strings.Join(names, ", ")
	if !errors.Is(err, remote.ErrInvalid) || s != nil || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("Open = %v, %v; want nil and ErrInvalid ending %q", s, err, want)
	}

	if l := r.lines(t, "ERROR"); len(l) != 1 || l[0]["missing"] != strings.Join(names, ", ") {
		t.Errorf("Error lines %v, want one naming %q", l, strings.Join(names, ", "))
	}

	r.capabilities(t, append(named, absent...)...)
}

// On LocalStack with no setting, Open returns a store whose puts succeed, the
// claim absent, the health object stored and nothing left under check/; the
// check ran in the namespace its source drew, sent its one conditional request
// there, recorded every feature present and logged one Info line.
func TestOpenOnLocalStack(t *testing.T) {
	r := newOpenRig(t, nil)

	s, claim, err := r.open(t)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if claim.Body != nil || !errors.Is(claim.Err, remote.ErrNotFound) {
		t.Errorf("claim %+v, want no body and ErrNotFound", claim)
	}

	if b, ok := r.stored(t, openNS+"control/health"); !ok || !bytes.Equal(b, healthBody) {
		t.Errorf("health object %q, held %v; want %q", b, ok, healthBody)
	}

	r.leftUnderCheck(t)

	body := randomBytes(openBlock)
	if err := s.Put(t.Context(), remote.Name{9}, bytes.NewReader(body), openBlock, crc32cOf(body)); err != nil {
		t.Errorf("Put of the largest block: %v", err)
	}

	for _, q := range r.tr.Requests() {
		if q.Op != "PutObject" || strings.HasPrefix(q.Key, openNS+"control/") || q.Key == (&store{prefix: openNS}).blockKey(remote.Name{9}) {
			continue
		}

		if !strings.HasPrefix(q.Key, checkNS) {
			t.Errorf("the check put %s, outside the namespace %s its source drew", q.Key, checkNS)
		}
	}

	if c := conditional(r.tr.Requests()); len(c) != 1 || !strings.HasPrefix(c[0].Key, checkNS) {
		t.Errorf("conditional requests %+v, want one, under %s", c, checkNS)
	}

	r.capabilities(t)

	// Steps 1 to 7 in order: the first request is step 1's, the bucket's
	// settings are read once step 4's conditional put is sent, the claim then,
	// the health object after it, and the cleanup's batch last of the check.
	reqs := r.tr.Requests()
	at := func(op, key string) int {
		return slices.IndexFunc(reqs, func(q s3fault.Request) bool { return q.Op == op && strings.HasPrefix(q.Key, key) })
	}
	cleanupAt := -1
	for i, q := range reqs {
		if q.Op == "DeleteObjects" {
			cleanupAt = i
		}
	}

	order := []int{
		at("PutObject", checkNS+"integrity/"), slices.IndexFunc(reqs, func(q s3fault.Request) bool { return q.Header.Get("If-None-Match") != "" }),
		at("GetBucketVersioning", ""), at("GetBucketEncryption", ""),
		at("GetObject", openNS+"control/claim"), at("PutObject", openNS+"control/health"), cleanupAt,
	}
	if order[0] != 0 || !slices.IsSorted(order) || slices.Contains(order, -1) {
		t.Errorf("the check's steps reached the service at %v, want 0 first and every step after the one before", order)
	}

	info := r.lines(t, "INFO")
	if len(info) != 1 || info[0]["mechanism"] != "crc32c" || info[0]["rejection_code"] != "BadDigest" ||
		info[0]["storage_class"] != "STANDARD" || info[0]["batch_delete"] != true {
		t.Errorf("Info lines %v, want one with crc32c, BadDigest, STANDARD and batch_delete", info)
	}

	if l := r.lines(t, "ERROR"); len(l) != 0 {
		t.Errorf("Error lines %v, want none", l)
	}
}

// A one-zone class is reported with its failure domain.
func TestOpenReportsAOneZoneClass(t *testing.T) {
	r := newOpenRig(t, func(c *ClientConfig) { c.StorageClass = "ONEZONE_IA" })

	if _, _, err := r.open(t); err != nil {
		t.Fatalf("Open: %v", err)
	}

	info := r.lines(t, "INFO")
	if len(info) != 1 || info[0]["storage_class"] != "ONEZONE_IA" || info[0]["failure_domain"] != "one zone" {
		t.Errorf("Info lines %v, want one naming ONEZONE_IA and one zone", info)
	}
}

// expiring is a lifecycle rule expiring every object under prefix after a day.
func expiring(prefix string) types.LifecycleRule {
	return types.LifecycleRule{
		ID: aws.String("expire"), Status: types.ExpirationStatusEnabled,
		Filter:     &types.LifecycleRuleFilter{Prefix: aws.String(prefix)},
		Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)},
	}
}

// kms sets the bucket's default encryption to a KMS key, under which an ETag
// is not the MD5 of the stored bytes.
func kms(t *testing.T, b *s3fault.Bucket) {
	if _, err := b.Admin.PutBucketEncryption(t.Context(), &s3.PutBucketEncryptionInput{
		Bucket: &b.Name,
		ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAwsKms},
		}}},
	}); err != nil {
		t.Fatalf("PutBucketEncryption: %v", err)
	}
}

// Each bucket setting set for real refuses the open naming its feature, two at
// once both; settings that cannot reach the prefix, and a KMS default under a
// checksum the service checks, open.
func TestOpenJudgesEachBucketSetting(t *testing.T) {
	versioning := func(st types.BucketVersioningStatus) func(*testing.T, *s3fault.Bucket) {
		return func(t *testing.T, b *s3fault.Bucket) {
			if _, err := b.Admin.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
				Bucket: &b.Name, VersioningConfiguration: &types.VersioningConfiguration{Status: st},
			}); err != nil {
				t.Fatalf("PutBucketVersioning %s: %v", st, err)
			}
		}
	}

	lifecycle := func(rules ...types.LifecycleRule) func(*testing.T, *s3fault.Bucket) {
		return func(t *testing.T, b *s3fault.Bucket) {
			if _, err := b.Admin.PutBucketLifecycleConfiguration(t.Context(), &s3.PutBucketLifecycleConfigurationInput{
				Bucket: &b.Name, LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: rules},
			}); err != nil {
				t.Fatalf("PutBucketLifecycleConfiguration: %v", err)
			}
		}
	}

	tiering := func(t *testing.T, b *s3fault.Bucket) {
		if _, err := b.Admin.PutBucketIntelligentTieringConfiguration(t.Context(), &s3.PutBucketIntelligentTieringConfigurationInput{
			Bucket: &b.Name, Id: aws.String("archive"),
			IntelligentTieringConfiguration: &types.IntelligentTieringConfiguration{
				Id: aws.String("archive"), Status: types.IntelligentTieringStatusEnabled,
				Tierings: []types.Tiering{{AccessTier: types.IntelligentTieringAccessTierArchiveAccess, Days: aws.Int32(90)}},
			},
		}); err != nil {
			t.Fatalf("PutBucketIntelligentTieringConfiguration: %v", err)
		}
	}

	transition := types.LifecycleRule{
		ID: aws.String("archive"), Status: types.ExpirationStatusEnabled,
		Filter:      &types.LifecycleRuleFilter{Prefix: aws.String("")},
		Transitions: []types.Transition{{Days: aws.Int32(30), StorageClass: types.TransitionStorageClassGlacier}},
	}

	for _, tc := range []struct {
		name    string
		set     []func(*testing.T, *s3fault.Bucket)
		missing []feature
	}{
		{"versioning_enabled", []func(*testing.T, *s3fault.Bucket){versioning(types.BucketVersioningStatusEnabled)}, []feature{featVersioning}},
		{"versioning_suspended", []func(*testing.T, *s3fault.Bucket){
			versioning(types.BucketVersioningStatusEnabled), versioning(types.BucketVersioningStatusSuspended),
		}, []feature{featVersioning}},
		{"expiring_on_the_prefix", []func(*testing.T, *s3fault.Bucket){lifecycle(expiring(openNS))}, []feature{featLifecycle}},
		{"transition_on_every_key", []func(*testing.T, *s3fault.Bucket){lifecycle(transition)}, []feature{featLifecycle}},
		{"archive_tiering", []func(*testing.T, *s3fault.Bucket){tiering}, []feature{featTiering}},
		{"versioning_and_expiring", []func(*testing.T, *s3fault.Bucket){
			versioning(types.BucketVersioningStatusEnabled), lifecycle(expiring(openNS)),
		}, []feature{featVersioning, featLifecycle}},
		{"expiring_on_another_prefix", []func(*testing.T, *s3fault.Bucket){lifecycle(expiring("other/"))}, nil},
		{"kms_under_crc32c", []func(*testing.T, *s3fault.Bucket){kms}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOpenRig(t, nil)
			b := &s3fault.Bucket{Name: r.bucket, Admin: r.admin}
			for _, set := range tc.set {
				set(t, b)
			}

			s, _, err := r.open(t)
			r.leftUnderCheck(t)

			if tc.missing == nil {
				if err != nil {
					t.Fatalf("Open: %v, want it to open", err)
				}

				r.capabilities(t)

				return
			}

			r.refused(t, s, err, tc.missing)

			if _, ok := r.stored(t, openNS+"control/health"); ok {
				t.Error("a refused open put the health object")
			}
		})
	}
}

// For each bucket-setting read, 403 refuses naming its feature, 501 and a 404
// with the setting's not-found code pass, and a 503 fails the open
// ErrTransient after exactly one request of it.
func TestOpenReadsEachSettingsReply(t *testing.T) {
	for _, read := range []struct {
		op string
		f  feature
	}{
		{"GetBucketVersioning", featVersioning},
		{"GetBucketLifecycleConfiguration", featLifecycle},
		{"ListBucketIntelligentTieringConfigurations", featTiering},
		{"GetBucketEncryption", featEncryption},
	} {
		for _, tc := range []struct {
			status int
			code   string
		}{{403, "AccessDenied"}, {501, "NotImplemented"}, {404, settingNotFound[read.f]}, {503, "ServiceUnavailable"}} {
			if tc.status == 404 && tc.code == "" {
				continue // versioning has no not-found code: S3 answers no status
			}

			t.Run(fmt.Sprintf("%s_%d", read.op, tc.status), func(t *testing.T) {
				r := newOpenRig(t, nil)
				r.tr.Inject(s3fault.Rule{Op: read.op}, s3fault.Answer(tc.status, tc.code, nil))

				s, _, err := r.open(t)
				r.leftUnderCheck(t)

				switch tc.status {
				case 403:
					r.refused(t, s, err, []feature{read.f})
				case 501, 404:
					if err != nil {
						t.Fatalf("Open: %v, want a %d %s to pass", err, tc.status, tc.code)
					}

					r.capabilities(t)
				case 503:
					if !errors.Is(err, remote.ErrTransient) || s != nil {
						t.Errorf("Open = %v, %v; want nil and ErrTransient", s, err)
					}

					if n := len(requestsOf(r.tr, read.op)); n != 1 {
						t.Errorf("%d %s requests, want exactly one", n, read.op)
					}

					if got := r.rec.capabilities(); len(got) != 0 {
						t.Errorf("a failed check recorded %+v, want nothing", got)
					}
				}
			})
		}
	}
}

// A stored claim comes back whole; one that cannot be read opens with its
// error.
func TestOpenHandsBackTheClaim(t *testing.T) {
	t.Run("stored", func(t *testing.T) {
		r := newOpenRig(t, nil)
		r.rawPut(t, openNS+"control/claim", []byte("the claim"))

		_, claim, err := r.open(t)
		if err != nil || claim.Err != nil || string(claim.Body) != "the claim" {
			t.Errorf("Open = %+v, %v; want the claim's body and no error", claim, err)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		r := newOpenRig(t, nil)
		r.tr.Inject(s3fault.Rule{Op: "GetObject", KeyPrefix: openNS + "control/claim"}, s3fault.Answer(503, "ServiceUnavailable", nil))

		s, claim, err := r.open(t)
		if err != nil || s == nil || claim.Body != nil || !errors.Is(claim.Err, remote.ErrTransient) {
			t.Errorf("Open = %v, %+v, %v; want a store and Claim.Err ErrTransient", s, claim, err)
		}
	})
}

// A cleanup that fails, with batch delete unsupported and every one-key delete
// under the check namespace failing, opens anyway and logs one Warn line.
func TestOpenSurvivesAFailedCleanup(t *testing.T) {
	r := newOpenRig(t, nil)
	r.tr.Inject(s3fault.Rule{Op: "DeleteObjects"}, s3fault.Answer(501, "NotImplemented", nil))
	r.tr.Inject(s3fault.Rule{Op: "DeleteObject", KeyPrefix: checkNS}, s3fault.Answer(500, "InternalError", nil))

	if _, _, err := r.open(t); err != nil {
		t.Fatalf("Open: %v, want a failed cleanup not to fail it", err)
	}

	if w := r.lines(t, "WARN"); len(w) != 1 || w[0]["left"] != float64(len(checkKeys(checkNS))) {
		t.Errorf("Warn lines %v, want one with left %d", w, len(checkKeys(checkNS)))
	}

	if n := len(requestsOf(r.tr, "DeleteObject")); n != 1 {
		t.Errorf("%d DeleteObject requests, want the cleanup to stop at the first failure", n)
	}

	r.capabilities(t, featBatchDelete)

	// The keys left show the size step 1 proved its mechanism at: the
	// client's largest block, the bound Put enforces.
	if b, ok := r.stored(t, checkNS+"integrity/large"); !ok || len(b) != openBlock {
		t.Errorf("the largest-block proof stored %d bytes, held %v; want %d, the client's largest block", len(b), ok, openBlock)
	}
}

// A cleanup whose batch fails logs every key left, and the open stands.
func TestCleanupOfAFailedBatch(t *testing.T) {
	r := newOpenRig(t, nil)
	r.tr.Inject(s3fault.Rule{Op: "DeleteObjects"}, s3fault.Answer(500, "InternalError", nil))

	cleanup(t.Context(), r.c, openNS, checkNS, true)

	if w := r.lines(t, "WARN"); len(w) != 1 || w[0]["left"] != float64(len(checkKeys(checkNS))) {
		t.Errorf("Warn lines %v, want one with left %d", w, len(checkKeys(checkNS)))
	}
}

// An open cancelled while step 4 waits on a lagging listing still deletes
// everything the check wrote.
func TestOpenCancelledCleansUp(t *testing.T) {
	r := newOpenRig(t, nil)
	r.tr.Inject(s3fault.Rule{}, s3fault.HoldNewKeys(relists+1))

	clk := sim.NewTestClock(time.Unix(0, 0))
	r.c.setCheckSources(clk, nil)
	armed := clk.Armed(relistWait)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := Open(ctx, r.c, openNS, remote.Mutable)
		done <- err
	}()

	select {
	case <-armed:
		cancel()
	case err := <-done:
		t.Fatalf("Open returned %v before step 4 waited", err)
	case <-time.After(2 * time.Minute): // a backstop only; the wait is seen on the clock
		t.Fatal("step 4 never waited on the clock")
	}

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Open = %v, want context.Canceled", err)
	}

	r.leftUnderCheck(t)
}

// A step 1 that proves nothing leaves steps 2 to 4 unverified and unnamed,
// while step 5 still runs: a refused encryption read is named beside it.
func TestOpenWithNothingProved(t *testing.T) {
	r := newOpenRig(t, nil)
	r.tr.Inject(s3fault.Rule{Op: "PutObject"}, s3fault.Answer(400, "InvalidRequest", nil))
	r.tr.Inject(s3fault.Rule{Op: "GetBucketEncryption"}, s3fault.Answer(403, "AccessDenied", nil))

	s, _, err := r.open(t)
	r.refused(t, s, err, []feature{featPutIntegrity, featEncryption},
		featReadAfterWrite, featStorageClass, featRangeGet, featListBucket, featBatchDelete, featList, featConditionalPut)
	r.leftUnderCheck(t)
}

// Under Cubbit DS3's composition the check chooses the ETag and opens, with
// conditional_put 0; the same service under a KMS default encryption, set for
// real, refuses naming encryption.
func TestOpenUnderCubbitDS3(t *testing.T) {
	t.Run("opens_with_the_etag", func(t *testing.T) {
		r := newOpenRig(t, nil)
		cubbit(r.tr)

		s, _, err := r.open(t)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		if got := s.(*store).Checksum(); got != remote.ChecksumNone {
			t.Errorf("Checksum %v, want ChecksumNone, the ETag", got)
		}

		r.capabilities(t, featConditionalPut)
		r.leftUnderCheck(t)
	})

	t.Run("refused_under_kms", func(t *testing.T) {
		r := newOpenRig(t, nil)
		kms(t, &s3fault.Bucket{Name: r.bucket, Admin: r.admin})
		cubbit(r.tr)

		s, _, err := r.open(t)
		r.refused(t, s, err, []feature{featEncryption}, featConditionalPut)
	})
}

// A wrong secret, which LocalStack does not check, scripted as S3 answers it
// to every request: the open fails ErrDenied of cause access, and neither its
// error nor any log record, the failed cleanup's among them, holds the access
// key, the secret or the signature.
func TestOpenRefusesAWrongSecret(t *testing.T) {
	const key, secret, signature = "AKIDOPENCHECKKEY", "the-wrong-secret-value", "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"

	r := newOpenRig(t, func(c *ClientConfig) { c.AccessKeyID, c.SecretAccessKey = key, secret })

	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided.</Message>` +
		`<AWSAccessKeyId>` + key + `</AWSAccessKeyId><SignatureProvided>` + signature + `</SignatureProvided>` +
		`<RequestId>s3fault</RequestId></Error>`)
	r.tr.Inject(s3fault.Rule{}, s3fault.Answer(403, "SignatureDoesNotMatch", body))

	s, _, err := r.open(t)

	var d *remote.DeniedError
	if s != nil || !errors.As(err, &d) || d.Cause != remote.CauseAccess {
		t.Fatalf("Open = %v, %v; want nil and ErrDenied of cause access", s, err)
	}

	r.log.mu.Lock()
	logged := r.log.buf.String()
	r.log.mu.Unlock()

	if logged == "" {
		t.Fatal("the refused open logged nothing, want the cleanup's Warn line to search")
	}

	for _, leak := range []string{key, secret, signature} {
		if strings.Contains(err.Error(), leak) || strings.Contains(logged, leak) {
			t.Errorf("%q appears in the error %q or the log %q", leak, err, logged)
		}
	}
}
