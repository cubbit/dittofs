package s3

import (
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/pkg/remote"
)

// feature is one service feature the capability check verifies, one per
// mutable row of RFC 4 Appendix C.1 that a check step provokes. Single-part
// atomic replace has no feature: it cannot be provoked, so it is taken from the
// service's documentation. The constants run in check order, the order a
// refusal names them in.
type feature uint8

const (
	featPutIntegrity   feature = iota // step 1: an enforced checksum or a proven ETag, at the largest block
	featReadAfterWrite                // step 2: a get at once after the put returns the object
	featStorageClass                  // step 2: the class read back is the pinned one and needs no restore
	featRangeGet                      // step 2: an exact range, and 416 past the end
	featListBucket                    // step 2: a key never written answers 404, not 403
	featBatchDelete                   // step 3: DeleteObjects reports every key; absent is recorded, not refused
	featList                          // step 4: every key once, in order, with LastModified, within the re-list bound
	featConditionalPut                // step 4: If-None-Match refused; recorded, never refused
	featVersioning                    // step 5: versioning never enabled
	featLifecycle                     // step 5: no expiring or transitioning rule that could match the prefix
	featTiering                       // step 5: no archive tier that could match the prefix
	featEncryption                    // step 5: no default encryption that breaks the ETag the check chose
	featureCount
)

// featureNames are the features' names: the feature label of
// remote_capability and the word a refusal uses.
var featureNames = [featureCount]string{
	featPutIntegrity:   "put_integrity",
	featReadAfterWrite: "read_after_write",
	featStorageClass:   "storage_class",
	featRangeGet:       "range_get",
	featListBucket:     "list_bucket",
	featBatchDelete:    "batch_delete",
	featList:           "list",
	featConditionalPut: "conditional_put",
	featVersioning:     "versioning",
	featLifecycle:      "lifecycle",
	featTiering:        "tiering",
	featEncryption:     "encryption",
}

func (f feature) String() string { return featureNames[f] }

// refuses reports whether the feature's absence refuses the open. A service
// without batch delete is recorded and deleted from one key at a time, and
// conditional put is measured and used by nothing (RFC 4 §4.3).
func (f feature) refuses() bool {
	return f != featBatchDelete && f != featConditionalPut
}

// featureState is what one check found of a feature.
type featureState uint8

const (
	stateUnverified featureState = iota // the step that verifies it could not run
	statePresent
	stateMissing
)

// verdict is the state of every feature after one check, indexed by feature.
// The steps judge into one verdict.
type verdict [featureCount]featureState

// judge sets f present or missing.
func (v *verdict) judge(f feature, ok bool) {
	v[f] = stateMissing
	if ok {
		v[f] = statePresent
	}
}

// refusals names, in check order, the features found missing whose absence
// refuses the open, "" when none is. An unverified feature is not named: its
// step could not run because an earlier one failed, and that one is named.
func (v verdict) refusals() string {
	var names []string
	for f := range featureCount {
		if v[f] == stateMissing && f.refuses() {
			names = append(names, f.String())
		}
	}

	return strings.Join(names, ", ")
}

// judgeVersioning reports whether a bucket's versioning status admits a
// mutable store: only no status does.
//
// decision: versioning, and with it object lock, which requires it, is refused
// on every namespace store, Suspended included, since a suspended bucket keeps
// the versions written while it was on and sweep deletes no version. This also
// rules out the service's own bucket replication wherever that replication
// requires versioning, so a namespace's bucket is protected against its own
// loss, and against a holder of its credential, only by a copying backup to
// another location, immutable where that holder is the threat. Admit
// versioning, and teach sweep to delete versions, if a deployment needs a
// replica kept in step with the bucket, which a copying backup taken per
// snapshot is not.
func judgeVersioning(status types.BucketVersioningStatus) bool {
	return status == ""
}

// ruleFilter is what a lifecycle rule's or a tiering configuration's filter
// says about the objects it acts on: a key prefix, whether it names a tag, and
// the object sizes it is bounded by, nil where unbounded.
type ruleFilter struct {
	prefix string
	tagged bool
	over   *int64 // ObjectSizeGreaterThan
	under  *int64 // ObjectSizeLessThan
}

// lifecycleFilter is r's filter, its prefix taken from Filter.Prefix,
// Filter.And.Prefix or the deprecated rule Prefix, whichever is set. S3 sets
// at most one of them, and at most one of a filter's own fields.
func lifecycleFilter(r types.LifecycleRule) ruleFilter {
	var f ruleFilter
	p := r.Prefix //nolint:staticcheck // a bucket configured before Filter still answers with it
	if lf := r.Filter; lf != nil {
		f.tagged = keyed(lf.Tag)
		f.over, f.under = lf.ObjectSizeGreaterThan, lf.ObjectSizeLessThan
		if lf.Prefix != nil {
			p = lf.Prefix
		}

		if a := lf.And; a != nil {
			f.tagged = f.tagged || slices.ContainsFunc(a.Tags, func(t types.Tag) bool { return keyed(&t) })
			if a.ObjectSizeGreaterThan != nil {
				f.over = a.ObjectSizeGreaterThan
			}

			if a.ObjectSizeLessThan != nil {
				f.under = a.ObjectSizeLessThan
			}

			if a.Prefix != nil {
				p = a.Prefix
			}
		}
	}

	if p != nil {
		f.prefix = *p
	}

	return f
}

// keyed reports whether t is a tag a filter can match on: S3 requires a key,
// and the SDK reads an empty <Tag/> as a tag with none, which filters nothing.
func keyed(t *types.Tag) bool {
	return t != nil && aws.ToString(t.Key) != ""
}

// tieringFilter is c's filter; a tiering filter bounds no size.
func tieringFilter(c types.IntelligentTieringConfiguration) ruleFilter {
	var f ruleFilter
	if tf := c.Filter; tf != nil {
		f.tagged = keyed(tf.Tag)
		p := tf.Prefix
		if a := tf.And; a != nil {
			f.tagged = f.tagged || slices.ContainsFunc(a.Tags, func(t types.Tag) bool { return keyed(&t) })
			if a.Prefix != nil {
				p = a.Prefix
			}
		}

		if p != nil {
			f.prefix = *p
		}
	}

	return f
}

// couldMatch reports whether f could match an object the store writes under
// prefix: its prefix is empty, a prefix of the store's, or under it; it names
// no tag, since the store puts none; and its size bounds meet 1 byte to
// largest, the size of the largest object the store writes: the largest block
// or a control object's bound, whichever is greater. A largest below 1 bounds
// nothing.
//
// decision: the size bounds are read inclusively, though S3 documents them as
// strict, so a service that implements them inclusively cannot expire the
// largest object while the check passes. The cost is refusing a rule bounded
// exactly at 1 byte or at the largest object's size.
func couldMatch(f ruleFilter, prefix string, largest int64) bool {
	if f.tagged || !strings.HasPrefix(prefix, f.prefix) && !strings.HasPrefix(f.prefix, prefix) {
		return false
	}

	lo, hi := int64(1), largest
	if hi < 1 {
		hi = math.MaxInt64
	}

	if f.over != nil {
		lo = max(lo, *f.over)
	}

	if f.under != nil {
		hi = min(hi, *f.under)
	}

	return lo <= hi
}

// judgeLifecycle reports whether rules admit a mutable store under prefix: no
// enabled rule that expires or transitions objects could match it. A rule
// acting only on incomplete multipart uploads or noncurrent versions acts on
// nothing a mutable store holds, and an expiry that removes only delete
// markers, with no date and no age, acts on nothing an unversioned bucket
// holds. largest is couldMatch's.
func judgeLifecycle(rules []types.LifecycleRule, prefix string, largest int64) bool {
	for _, r := range rules {
		// decision: a Disabled rule passes, so a rule enabled after the open
		// goes unseen until the next one, as any drift does until a periodic
		// re-read of the bucket's settings lands. Refuse a Disabled rule that
		// could match if drift between opens is ever found deleting blocks. A
		// status other than Disabled, one a service spells its own way among
		// them, counts as enabled.
		if r.Status == types.ExpirationStatusDisabled {
			continue
		}

		e := r.Expiration
		expires := e != nil && (e.Days != nil || e.Date != nil)
		if (expires || len(r.Transitions) > 0) && couldMatch(lifecycleFilter(r), prefix, largest) {
			return false
		}
	}

	return true
}

// judgeTiering reports whether configs admit a store under prefix: no enabled
// configuration holds a tier with a filter that could match it. Every tier a
// tiering configuration can hold is an archive tier, whose gets need a
// restore, so any tier counts, one the SDK does not know among them.
func judgeTiering(configs []types.IntelligentTieringConfiguration, prefix string) bool {
	for _, c := range configs {
		// decision: a Disabled configuration passes, so one enabled after the
		// open goes unseen until the next one, as a Disabled lifecycle rule
		// does. Refuse a Disabled configuration that could match if drift
		// between opens is ever found archiving blocks. Any other status counts
		// as enabled.
		if c.Status == types.IntelligentTieringStatusDisabled {
			continue
		}

		// A tiering filter bounds no size, so any largest block will do.
		if len(c.Tierings) > 0 && couldMatch(tieringFilter(c), prefix, 1) {
			return false
		}
	}

	return true
}

// judgeEncryption reports whether a bucket's default encryption, nil for none,
// admits sum, the integrity mechanism step 1 proved; it is judged only once
// one is proved, since ChecksumNone here is the ETag. A checksum the service
// checks holds whatever the encryption; the ETag holds only under no default
// or AES256, since under a KMS key, or any algorithm not known to keep it, the
// ETag is not the MD5 of the stored bytes.
func judgeEncryption(def *types.ServerSideEncryptionConfiguration, sum remote.Checksum) bool {
	if sum != remote.ChecksumNone || def == nil {
		return true
	}

	for _, r := range def.Rules {
		if d := r.ApplyServerSideEncryptionByDefault; d != nil && d.SSEAlgorithm != types.ServerSideEncryptionAes256 {
			return false
		}
	}

	return true
}

// classRead is the class a get reported, STANDARD when it reported none,
// since S3 sends no x-amz-storage-class header for it.
func classRead(reported types.StorageClass) string {
	if reported == "" {
		return string(types.StorageClassStandard)
	}

	return string(reported)
}

// judgeClass reports whether the class a get reported for the check's object
// is pinned, the class the store writes in, and one whose gets need no
// restore.
func judgeClass(reported types.StorageClass, pinned string) bool {
	class := classRead(reported)
	return class == pinned && restoreFree(class)
}

// failureDomain is what class keeps an object's data across, for the open's
// log line (RFC 4 §8 item 7): one zone for the one-zone classes, and what the
// service documents, as RFC 4 Appendix B records it, for any other.
func failureDomain(class string) string {
	switch class {
	case "ONEZONE_IA", "EXPRESS_ONEZONE":
		return "one zone"
	}

	return "as the service documents"
}

// settingOutcome is what a bucket-setting read's error says (RFC 4
// Appendix C.1).
type settingOutcome uint8

const (
	settingConfig  settingOutcome = iota // the call answered with a configuration, to be judged
	settingNone                          // no configuration, or the service has no such feature: passes
	settingRefused                       // 403: the credential may not read the setting, so the feature is missing
	settingFailed                        // anything else: the open fails with the call's error as the table maps it
)

// settingNotFound is the code a 404 carries when the bucket holds no such
// configuration, per setting. Versioning has none: S3 answers an empty reply,
// so a 404 to it is never "no configuration". Listing tiering configurations
// answers an empty list too, but a service may answer it as S3 answers the
// read of one configuration.
var settingNotFound = map[feature]string{
	featLifecycle:  "NoSuchLifecycleConfiguration",
	featTiering:    "NoSuchConfiguration",
	featEncryption: "ServerSideEncryptionConfigurationNotFoundError",
}

// settingRead reads err, what the SDK returned for the read of setting f.
// 404 with f's not-found code and 501 are settingNone; any 403 is
// settingRefused; any other error, 404 NoSuchBucket, 401, a 3xx, a 5xx and a
// failure to send among them, is settingFailed.
func settingRead(f feature, err error) settingOutcome {
	if err == nil {
		return settingConfig
	}

	status, code := reply(err)
	switch {
	case status == http.StatusNotImplemented,
		status == http.StatusNotFound && code != "" && code == settingNotFound[f]:
		return settingNone
	case status == http.StatusForbidden:
		return settingRefused
	}

	return settingFailed
}
