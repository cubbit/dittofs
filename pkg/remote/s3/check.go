package s3

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/pkg/remote"
)

// healthBody is the health object's body; nothing reads its content on a
// namespace's store.
var healthBody = []byte("dittofs health\n")

// refusedError is an open the capability check refused: it names, in check
// order, every missing feature whose absence refuses the open.
type refusedError struct{ missing string }

func (e *refusedError) Error() string {
	return "remote/s3: open refused: service lacks " + e.missing
}

func (e *refusedError) Unwrap() error { return remote.ErrInvalid }

// cleanupTimeout bounds the check's cleanup, which runs on after the open's
// own context has ended so a cancelled open still deletes what it wrote.
const cleanupTimeout = 30 * time.Second

// check is the capability check of s, the namespace's store (RFC 4 Appendix
// C.4), steps 1 to 7 in order, in a fresh check namespace drawn from the
// client's random source. It sets s.choice to what step 1 and step 3 proved
// and returns the claim step 6 read and the verdict. Steps 2 to 4 run only
// once step 1 proved a mechanism, and their features stay unverified
// otherwise; steps 5 and 6 run whatever step 1 found, so a refusal names every
// missing feature it can. Step 7 puts the health object only when nothing
// refuses the open. A call that fails rather than answers fails the check with
// its error as the table maps it. Whatever the outcome, every key the check
// namespace may hold is deleted after, a failure logged and not returned.
//
// decision: a store's open calls the service directly, outside the syncer.
// Its calls are a fixed handful per open, not per backup or per block, and a
// failure fails the open rather than any transfer, so no retry budget,
// throttle limit or health count is at stake. Route them through a flow if
// opens become frequent enough, a periodic re-read reopening per pass say,
// that their requests show in a service's throttling.
func check(ctx context.Context, s *store) (Claim, verdict, error) {
	c := s.c

	var id [8]byte
	if _, err := io.ReadFull(c.rand, id[:]); err != nil {
		return Claim{}, verdict{}, fmt.Errorf("remote/s3: drawing the check namespace: %w", err)
	}

	ns := s.prefix + "check/" + hex.EncodeToString(id[:]) + "/"

	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()

		cleanup(cctx, c, s.prefix, ns, s.choice.batch)
	}()

	ch, v, err := proveIntegrity(ctx, c, ns, c.cfg.MaxBlockSize)
	if err != nil {
		return Claim{}, verdict{}, err
	}

	if ch.proved {
		var w verdict
		ch, w, err = probe(ctx, c, ns, ch)
		s.choice = ch // a failed probe proves nothing: the cleanup deletes one key at a time

		if err != nil {
			return Claim{}, verdict{}, err
		}

		v.merge(w)
	}

	if err := judgeSettings(ctx, c, s.prefix, ch, &v); err != nil {
		return Claim{}, verdict{}, err
	}

	var claim Claim
	claim.Body, _, claim.Err = s.GetControl(ctx, remote.RoleClaim)

	if v.refusals() == "" {
		if err := s.PutControl(ctx, remote.RoleHealth, healthBody); err != nil {
			return Claim{}, verdict{}, err
		}
	}

	return claim, v, nil
}

// merge takes every feature w verified.
func (v *verdict) merge(w verdict) {
	for f, st := range w {
		if st != stateUnverified {
			v[f] = st
		}
	}
}

// judgeSettings is step 5: the bucket's versioning status, lifecycle rules,
// tiering configurations, every page of them, and default encryption, each
// read as settingRead says and judged for the store under prefix. The
// encryption is judged only once step 1 proved a mechanism, since the ETag
// is what it can break.
func judgeSettings(ctx context.Context, c *Client, prefix string, ch choice, v *verdict) error {
	b := &c.cfg.Bucket

	ver, err := c.api.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: b})
	if err := setting(ctx, v, featVersioning, err, func() bool { return judgeVersioning(ver.Status) }); err != nil {
		return err
	}

	lc, err := c.api.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: b})
	if err := setting(ctx, v, featLifecycle, err, func() bool {
		return judgeLifecycle(lc.Rules, prefix, c.cfg.MaxBlockSize)
	}); err != nil {
		return err
	}

	configs, err := tieringConfigs(ctx, c)
	if err := setting(ctx, v, featTiering, err, func() bool { return judgeTiering(configs, prefix) }); err != nil {
		return err
	}

	enc, err := c.api.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: b})

	judge := func() bool { return judgeEncryption(enc.ServerSideEncryptionConfiguration, ch.sum) }
	if !ch.proved {
		judge = nil // the ETag is what a default can break, and none is proved
	}

	return setting(ctx, v, featEncryption, err, judge)
}

// setting judges f from its read's error, and from judge when the read
// answered a configuration; a nil judge leaves f unverified unless the read
// was refused. A read that failed is returned as the table maps it, a 404 the
// table reads as an absent object read as the invalid reply it is.
func setting(ctx context.Context, v *verdict, f feature, err error, judge func() bool) error {
	switch out := settingRead(f, err); {
	case out == settingFailed:
		if e := fromSDK(ctx, opSetting, recorded{}, nil, err); !errors.Is(e, remote.ErrNotFound) {
			return e
		}

		return wrap(opSetting, http.StatusNotFound, "NoSuchKey", remote.ErrInvalid)
	case out == settingRefused:
		v.judge(f, false)
	case judge == nil:
	case out == settingNone:
		v.judge(f, true)
	default:
		v.judge(f, judge())
	}

	return nil
}

// tieringPages bounds the pages of tiering configurations the check reads: a
// bucket holds at most 1,000, so a service still truncating past it is
// failing.
const tieringPages = 100

// tieringConfigs lists every tiering configuration of the bucket, page by
// page, stopping at a page that is not truncated or names no new token; past
// tieringPages the read fails as remote.ErrTransient.
func tieringConfigs(ctx context.Context, c *Client) ([]types.IntelligentTieringConfiguration, error) {
	var (
		all   []types.IntelligentTieringConfiguration
		token *string
	)

	for range tieringPages {
		out, err := c.api.ListBucketIntelligentTieringConfigurations(ctx, &s3.ListBucketIntelligentTieringConfigurationsInput{
			Bucket: &c.cfg.Bucket, ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}

		all = append(all, out.IntelligentTieringConfigurationList...)

		next := aws.ToString(out.NextContinuationToken)
		if !aws.ToBool(out.IsTruncated) || next == "" || next == aws.ToString(token) {
			return all, nil
		}

		token = &next
	}

	return nil, fmt.Errorf("remote/s3: %s: tiering configurations still truncated after %d pages: %w", opSetting, tieringPages, remote.ErrTransient)
}

// checkKeys are the keys steps 1 to 4 may write under ns, as integrity.go
// and probe.go name them, block keys and others, which the store's Delete of
// names cannot reach. The open's tests list the check namespace after it, so
// a key a step adds and this list misses shows there.
func checkKeys(ns string) []string {
	s := &store{prefix: ns}
	keys := []string{
		ns + "integrity/crc32c", ns + "integrity/crc32c-wrong",
		ns + "integrity/md5", ns + "integrity/md5-wrong",
		ns + "integrity/etag",
		ns + "integrity/large", ns + "integrity/large-wrong",
		ns + "read/object",
		ns + "delete/a", ns + "delete/b",
	}

	for _, n := range []remote.Name{{1}, {2}, {3}} {
		keys = append(keys, s.blockKey(n))
	}

	return keys
}

// cleanup deletes every key of checkKeys(ns), in one batch where step 3
// proved one, else one key per request, stopping at the first failure so a
// service that is down costs one more request, not one per key. The keys it
// could not delete are logged at Warn, with prefix, and not returned.
func cleanup(ctx context.Context, c *Client, prefix, ns string, batch bool) {
	keys := checkKeys(ns)
	left := len(keys)

	if batch {
		objs := make([]types.ObjectIdentifier, len(keys))
		for i := range keys {
			objs[i] = types.ObjectIdentifier{Key: &keys[i]}
		}

		out, err := c.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &c.cfg.Bucket,
			Delete: &types.Delete{Objects: objs, Quiet: aws.Bool(false)},
		}, contentMD5)
		if err == nil {
			left = 0
			for _, e := range deleteResults(keys, out.Deleted, out.Errors) {
				if e != nil {
					left++
				}
			}
		}
	} else {
		for _, k := range keys {
			_, err := c.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &c.cfg.Bucket, Key: &k})
			if fromSDK(ctx, opDelete, recorded{}, nil, err) != nil {
				break
			}

			left--
		}
	}

	if left > 0 {
		c.log.Warn("remote capability check left keys behind", "prefix", prefix, "left", left)
	}
}
