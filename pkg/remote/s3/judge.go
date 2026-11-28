package s3

import (
	"net/http"
	"strings"

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

// state is what one check found of a feature.
type state uint8

const (
	unverified state = iota // the step that verifies it could not run
	present
	missing
)

// verdict is the state of every feature after one check, indexed by feature.
type verdict [featureCount]state

// judge sets f present or missing.
func (v *verdict) judge(f feature, ok bool) {
	v[f] = missing
	if ok {
		v[f] = present
	}
}

// missing names, in check order, the features found missing whose absence
// refuses the open, "" when none is. An unverified feature is not named: its
// step could not run because an earlier one failed, and that one is named.
func (v verdict) missing() string {
	var names []string
	for f := range featureCount {
		if v[f] == missing && f.refuses() {
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
// loss only by a copying backup to another location. Admit versioning, and
// teach sweep to delete versions, if a deployment needs a replica kept in step
// with the bucket.
func judgeVersioning(status types.BucketVersioningStatus) bool {
	return status == ""
}

// filter is what a lifecycle rule's or a tiering configuration's filter says
// about the objects it acts on: a key prefix, whether it names a tag, and the
// object sizes it is bounded by, nil where unbounded.
type filter struct {
	prefix string
	tagged bool
	over   *int64 // ObjectSizeGreaterThan
	under  *int64 // ObjectSizeLessThan
}

// lifecycleFilter is r's filter, its prefix taken from Filter.Prefix,
// Filter.And.Prefix or the deprecated rule Prefix, whichever is set.
func lifecycleFilter(r types.LifecycleRule) filter {
	var f filter
	p := r.Prefix //nolint:staticcheck // a bucket configured before Filter still answers with it
	if lf := r.Filter; lf != nil {
		f.tagged = lf.Tag != nil
		f.over, f.under = lf.ObjectSizeGreaterThan, lf.ObjectSizeLessThan
		if lf.Prefix != nil {
			p = lf.Prefix
		}

		if a := lf.And; a != nil {
			f.tagged = f.tagged || len(a.Tags) > 0
			if a.ObjectSizeGreaterThan != nil {
				f.over = a.ObjectSizeGreaterThan
			}

			if a.ObjectSizeLessThan != nil {
				f.under = a.ObjectSizeLessThan
			}

			if lf.Prefix == nil && a.Prefix != nil {
				p = a.Prefix
			}
		}
	}

	if p != nil {
		f.prefix = *p
	}

	return f
}

// tieringFilter is c's filter; a tiering filter bounds no size.
func tieringFilter(c types.IntelligentTieringConfiguration) filter {
	var f filter
	if tf := c.Filter; tf != nil {
		f.tagged = tf.Tag != nil
		p := tf.Prefix
		if a := tf.And; a != nil {
			f.tagged = f.tagged || len(a.Tags) > 0
			if p == nil {
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
// maxBlock, the sizes the store's objects take.
func couldMatch(f filter, prefix string, maxBlock int64) bool {
	if f.tagged || !strings.HasPrefix(prefix, f.prefix) && !strings.HasPrefix(f.prefix, prefix) {
		return false
	}

	// The smallest and largest sizes both the filter and the store admit.
	lo, hi := int64(1), maxBlock
	if f.over != nil && *f.over >= lo {
		if *f.over >= hi {
			return false
		}

		lo = *f.over + 1
	}

	if f.under != nil && *f.under <= hi {
		if *f.under <= lo {
			return false
		}

		hi = *f.under - 1
	}

	return lo <= hi
}

// judgeLifecycle reports whether rules admit a mutable store under prefix: no
// enabled rule that expires or transitions objects could match it. A rule
// acting only on incomplete multipart uploads or noncurrent versions acts on
// nothing a mutable store holds.
func judgeLifecycle(rules []types.LifecycleRule, prefix string, maxBlock int64) bool {
	for _, r := range rules {
		// decision: a Disabled rule passes, so a rule enabled after the open
		// goes unseen until the next one, as any drift does until a periodic
		// re-read of the bucket's settings lands. Refuse a Disabled rule that
		// could match if drift between opens is ever found deleting blocks.
		if r.Status != types.ExpirationStatusEnabled {
			continue
		}

		if (r.Expiration != nil || len(r.Transitions) > 0) && couldMatch(lifecycleFilter(r), prefix, maxBlock) {
			return false
		}
	}

	return true
}

// judgeTiering reports whether configs admit a store under prefix: no enabled
// configuration holds an archive tier, whose gets need a restore, with a filter
// that could match it. A disabled one passes, as a disabled lifecycle rule
// does.
func judgeTiering(configs []types.IntelligentTieringConfiguration, prefix string) bool {
	for _, c := range configs {
		if c.Status != types.IntelligentTieringStatusEnabled {
			continue
		}

		for _, t := range c.Tierings {
			archive := t.AccessTier == types.IntelligentTieringAccessTierArchiveAccess ||
				t.AccessTier == types.IntelligentTieringAccessTierDeepArchiveAccess
			// A tiering filter bounds no size, so any largest block will do.
			if archive && couldMatch(tieringFilter(c), prefix, 1) {
				return false
			}
		}
	}

	return true
}

// judgeEncryption reports whether a bucket's default encryption, nil for none,
// admits sum, the integrity mechanism step 1 chose. A checksum the service
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

// judgeClass reports whether the class a get reported for the check's object
// is pinned, the class the store writes in, and one whose gets need no
// restore. A get reporting no class reads as STANDARD, since S3 sends no
// x-amz-storage-class header for it.
func judgeClass(reported types.StorageClass, pinned string) bool {
	class := string(reported)
	if class == "" {
		class = string(types.StorageClassStandard)
	}

	return class == pinned && restoreFree(class)
}

// failureDomain is what class keeps an object's data across, for the open's
// log line (RFC 4 §8 item 7): one zone for the one-zone classes, and what the
// service documents, as RFC 4 Appendix B records it, for any other.
func failureDomain(class string) string {
	switch types.StorageClass(class) {
	case types.StorageClassOnezoneIa, types.StorageClassExpressOnezone:
		return "one zone"
	}

	return "as the service documents"
}

// read is what a bucket-setting read's error says (RFC 4 Appendix C.1).
type read uint8

const (
	readConfig  read = iota // the call answered with a configuration, to be judged
	readNone                // no configuration, or the service has no such feature: passes
	readRefused             // 403: the credential may not read the setting, so the feature is missing
	readFailed              // anything else: the open fails with the call's error as the table maps it
)

// settingNotFound is the code a 404 carries when the bucket holds no such
// configuration, per setting. Versioning and tiering answer an empty reply
// instead, so a 404 to either is never "no configuration".
var settingNotFound = map[feature]string{
	featLifecycle:  "NoSuchLifecycleConfiguration",
	featEncryption: "ServerSideEncryptionConfigurationNotFoundError",
}

// settingRead reads err, what the SDK returned for the read of setting f.
// 404 with f's not-found code and 501 are readNone; any 403 is readRefused;
// any other error, 404 NoSuchBucket, 401, a 3xx, a 5xx and a failure to send
// among them, is readFailed.
func settingRead(f feature, err error) read {
	if err == nil {
		return readConfig
	}

	status, code := reply(err)
	switch {
	case status == http.StatusNotImplemented,
		status == http.StatusNotFound && code != "" && code == settingNotFound[f]:
		return readNone
	case status == http.StatusForbidden:
		return readRefused
	}

	return readFailed
}
