package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/pkg/remote"
)

// probeObject is the object steps 2 to 4 put: ten bytes, so that bytes 2 to 5
// lie inside it and offset 20 past its end.
var probeObject = []byte("0123456789")

// The re-list bound of step 4: up to relists more listings, relistWait apart,
// 30 s in all.
const (
	relists    = 5
	relistWait = 6 * time.Second
)

// clock is the one method of a clock the capability check calls: real time in
// production; pkg/sim's TestClock satisfies it in tests.
type clock interface {
	After(d time.Duration) <-chan struct{}
}

// realClock is clock over real time.
type realClock struct{}

func (realClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })

	return ch
}

// probe is the capability check's steps 2 to 4 (RFC 4 Appendix C.4), in the
// check namespace ns, run once step 1 proved ch: the reads by key, the batch
// delete and the listing, each judged into the verdict, and ch with its batch
// set to whether step 3 found a batch delete it can trust. A call that fails
// rather than answers, a refused credential, a 5xx or a throttle among them,
// fails the check with the call's error as the table maps it; only a 403 to
// step 2's get of a key never written names a feature instead.
func probe(ctx context.Context, c *Client, ns string, ch choice) (choice, verdict, error) {
	s, err := open(c, ns, remote.Mutable, ch)
	if err != nil {
		return choice{}, verdict{}, err
	}

	var v verdict

	if err := probeReads(ctx, s, &v); err != nil {
		return choice{}, verdict{}, err
	}

	ch.batch, err = probeBatch(ctx, s, &v)
	if err != nil {
		return choice{}, verdict{}, err
	}

	if err := probeList(ctx, s, &v); err != nil {
		return choice{}, verdict{}, err
	}

	return ch, v, nil
}

// probeReads is step 2: probeObject put in the client's class, got at once
// and its class read back, bytes 2 to 5 got exactly, a get from offset 20
// answered 416, and a key never written answered 404 NoSuchKey, not 403.
func probeReads(ctx context.Context, s *store, v *verdict) error {
	key := s.prefix + "read/object"
	if err := putProbe(ctx, s, key); err != nil {
		return err
	}

	body, class, err := probeGet(ctx, s.c, key, "")
	if f := failure(ctx, opGet, err); f != nil {
		return f
	}

	// A get that does not return the object leaves its class unread.
	v.judge(featReadAfterWrite, err == nil && bytes.Equal(body, probeObject))
	if err == nil {
		v.judge(featStorageClass, judgeClass(class, s.c.cfg.StorageClass))
	}

	body, _, err = probeGet(ctx, s.c, key, "bytes=2-5")
	if f := failure(ctx, opGet, err); f != nil {
		return f
	}

	exact := err == nil && bytes.Equal(body, probeObject[2:6])

	_, _, err = probeGet(ctx, s.c, key, "bytes=20-")
	if f := failure(ctx, opGet, err); f != nil {
		return f
	}

	status, _ := reply(err)
	v.judge(featRangeGet, exact && status == http.StatusRequestedRangeNotSatisfiable)

	_, _, err = probeGet(ctx, s.c, s.prefix+"read/absent", "")
	status, code := reply(err)

	if status != http.StatusForbidden {
		if f := failure(ctx, opGet, err); f != nil {
			return f
		}
	}

	v.judge(featListBucket, status == http.StatusNotFound && code == "NoSuchKey")

	return nil
}

// probeBatch is step 3: two keys put and one never written, deleted in one
// DeleteObjects with Content-MD5 and Quiet false, every key reported deleted
// and the two gone from a listing. It reports whether the store may delete in
// batches. A batch rejected as unsupported, 501, 405 or 400 NotImplemented, is
// recorded as batch_delete missing, and the store deletes one key per
// request. A reply that misreports, a key refused, left out or still listed,
// or any other refusal of the batch, makes batch_delete missing too.
func probeBatch(ctx context.Context, s *store, v *verdict) (bool, error) {
	keys := []string{s.prefix + "delete/a", s.prefix + "delete/b", s.prefix + "delete/absent"}
	for _, k := range keys[:2] {
		if err := putProbe(ctx, s, k); err != nil {
			return false, err
		}
	}

	objs := make([]types.ObjectIdentifier, len(keys))
	for i := range keys {
		objs[i] = types.ObjectIdentifier{Key: &keys[i]}
	}

	out, err := s.c.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: &s.c.cfg.Bucket,
		Delete: &types.Delete{Objects: objs, Quiet: aws.Bool(false)},
	}, contentMD5)
	if err != nil {
		if f := failure(ctx, opDelete, err); f != nil {
			return false, f
		}

		v.judge(featBatchDelete, false)

		return false, nil
	}

	ok := !slices.ContainsFunc(deleteResults(keys, out.Deleted, out.Errors), func(e error) bool { return e != nil })

	if ok {
		prefix := s.prefix + "delete/"
		left, err := s.c.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &s.c.cfg.Bucket, Prefix: &prefix})
		if f := failure(ctx, opList, err); f != nil {
			return false, f
		}

		ok = err == nil && len(left.Contents) == 0
	}

	v.judge(featBatchDelete, ok)

	return ok, nil
}

// probeList is step 4: three block keys put, then listed in one List call
// until all three appear, within the re-list bound; then walked one key per
// page with start-after, each expected once, in order, with its time; then
// one of them put again with If-None-Match: *, the service's answer recorded
// as conditional_put.
func probeList(ctx context.Context, s *store, v *verdict) error {
	names := []remote.Name{{1}, {2}, {3}} // ascending, as the walk must return them
	for _, n := range names {
		if err := putProbe(ctx, s, s.blockKey(n)); err != nil {
			return err
		}
	}

	// decision: step 4's five re-lists over at most 30 s are fixed, not a
	// setting. A service whose listing lags longer cannot back GC's listing
	// walk (RFC 4 §4.6), so refusing it is the intended outcome, not a value to
	// tune around. The bound was chosen, not measured. Raise it if a service
	// that passes the services suite (RFC 4 §7.4) is refused here by a lag seen
	// on its own listing.
	listed := false
	for i := 0; ; i++ {
		page, _, _, _, err := s.list(ctx, "", len(names))
		if !answered(err) && err != nil {
			return err
		}

		if listed = err == nil && slices.Equal(namesOf(page), names); listed || i == relists {
			break
		}

		select {
		case <-s.c.clock.After(relistWait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	ok := listed
	if ok {
		var err error
		if ok, err = walkOnePerPage(ctx, s, names); err != nil {
			return err
		}
	}

	v.judge(featList, ok)

	// decision: conditional put is measured and reported, never used (RFC 4
	// §4.3): this is the one request the check sends with a conditional
	// header, and no rule reads its outcome. The one object that would gain
	// from it is the namespace claim, which stays a check fenced by clocks.
	// Add a conditional control put, used only where the check proved it, if a
	// deployment runs two installations on one bucket whose clocks cannot be
	// held within the clock-offset bound.
	key := s.blockKey(names[0])
	_, err := s.c.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &s.c.cfg.Bucket,
		Key:           &key,
		Body:          bytes.NewReader(probeObject),
		ContentLength: aws.Int64(int64(len(probeObject))),
		StorageClass:  types.StorageClass(s.c.cfg.StorageClass),
		IfNoneMatch:   aws.String("*"),
	})
	if f := failure(ctx, opPut, err); f != nil {
		return f
	}

	status, _ := reply(err)
	v.judge(featConditionalPut, status == http.StatusPreconditionFailed)

	return nil
}

// walkOnePerPage lists blocks/ one key per page, each page starting after the
// last key of the one before, and reports whether the walk returned names
// exactly, in order, each with its time, and ended within one page of them.
//
// decision: a page out of order, repeating a key, or naming a block without
// its time fails the check as remote.ErrTransient rather than naming list,
// since List refuses such a page as a reply it cannot trust before the walk
// sees it. Read those pages here, from the SDK's reply, if a service is ever
// seen sending one on every listing, which would fail its every open as
// retryable.
func walkOnePerPage(ctx context.Context, s *store, names []remote.Name) (bool, error) {
	var got []remote.Name

	after := remote.Cursor("")
	for range len(names) + 1 {
		page, next, done, _, err := s.list(ctx, after, 1)
		if err != nil {
			if answered(err) {
				return false, nil
			}

			return false, err
		}

		for _, in := range page {
			if in.ModTime.IsZero() {
				return false, nil
			}
		}

		got = append(got, namesOf(page)...)
		if done {
			return slices.Equal(got, names), nil
		}

		after = next
	}

	return false, nil
}

// namesOf is the names of a listed page, in its order.
func namesOf(page []remote.Info) []remote.Name {
	out := make([]remote.Name, len(page))
	for i, in := range page {
		out[i] = in.Name
	}

	return out
}

// putProbe puts probeObject at key through the store's put, under the
// mechanism step 1 proved and in the client's class.
func putProbe(ctx context.Context, s *store, key string) error {
	n := int64(len(probeObject))
	_, err := s.put(ctx, opPut, key, bytes.NewReader(probeObject), n, sumOf(s.choice.sum, bytes.NewReader(probeObject)))

	return err
}

// probeGet gets key over the SDK client, of rng unless it is "", and returns
// at most one byte more than probeObject holds, with the class the reply
// reported. err is the SDK's error, or the body's read error, as it came.
func probeGet(ctx context.Context, c *Client, key, rng string) ([]byte, types.StorageClass, error) {
	in := &s3.GetObjectInput{Bucket: &c.cfg.Bucket, Key: &key}
	if rng != "" {
		in.Range = &rng
	}

	out, err := c.api.GetObject(ctx, in)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = out.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(out.Body, int64(len(probeObject))+1))
	if err != nil {
		return nil, "", err
	}

	return body, out.StorageClass, nil
}

// failure is err, a check call's, as the table maps it when the call failed
// rather than answered: a network failure, a 5xx, a throttle, a refused
// credential or ctx ended. It is nil when err is, and when the service
// answered with a verdict on the feature, an error answered reports.
func failure(ctx context.Context, op opKind, err error) error {
	if e := fromSDK(ctx, op, recorded{}, nil, err); !answered(e) {
		return e
	}

	return nil
}

// answered reports whether e, an error as the table maps it, is the service's
// answer on what the check provoked, remote.ErrNotFound, remote.ErrInvalid or
// remote.ErrCorrupt, rather than a failure that fails the check. A nil e is
// not one.
func answered(e error) bool {
	return errors.Is(e, remote.ErrNotFound) || errors.Is(e, remote.ErrInvalid) || errors.Is(e, remote.ErrCorrupt)
}
