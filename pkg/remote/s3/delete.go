package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/cubbit/dittofs/pkg/remote"
)

// batchMax is the most keys one DeleteObjects request names (RFC 4 Appendix
// C.1).
const batchMax = 1000

// Delete removes the named blocks and returns one error per name, in order;
// nil means the service's reply stated that name deleted, or the name was
// already absent (RFC 4 §4.5). The names go in DeleteObjects requests of at
// most batchMax keys, Quiet false, sent one after another; on a service
// without batch delete, in one DeleteObject per name. A name the reply leaves
// out or reports twice, and every name of a request whose outcome is unknown,
// is remote.ErrTransient; a request the service refused fails each of its
// names with that refusal.
func (s *store) Delete(ctx context.Context, names []remote.Name) []error {
	start := time.Now()
	errs := make([]error, len(names))

	if err := s.refuseClosed(opDelete); err != nil {
		for i := range errs {
			errs[i] = err
		}
	} else if s.choice.batch {
		for lo := 0; lo < len(names); lo += batchMax {
			hi := min(lo+batchMax, len(names))
			s.deleteBatch(ctx, names[lo:hi], errs[lo:hi])
		}
	} else {
		for i, n := range names {
			errs[i] = s.deleteOne(ctx, n)
		}
	}

	// One call, one record: a denial's, so its cause reaches the operator
	// whatever else failed; otherwise the first failure's; ok when every name
	// was deleted.
	var shown error
	for _, err := range errs {
		if shown == nil || errors.Is(err, remote.ErrDenied) && !errors.Is(shown, remote.ErrDenied) {
			shown = err
		}
	}
	s.record(opDelete, shown, time.Since(start), 0)

	return errs
}

// deleteOne deletes one name with DeleteObject; an absent key answered 404
// NoSuchKey is the name deleted.
func (s *store) deleteOne(ctx context.Context, n remote.Name) error {
	key := s.blockKey(n)
	_, err := s.c.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.c.cfg.Bucket, Key: &key})

	return fromSDK(ctx, opDelete, recorded{}, nil, err)
}

// deleteBatch deletes names, at most batchMax, in one DeleteObjects request
// and writes each name's result into errs. A name repeated in names is sent
// once, and every slot of it gets that key's result.
func (s *store) deleteBatch(ctx context.Context, names []remote.Name, errs []error) {
	keys := make([]string, 0, len(names))
	slot := make(map[string][]int, len(names))

	for i, n := range names {
		k := s.blockKey(n)
		if _, seen := slot[k]; !seen {
			keys = append(keys, k)
		}
		slot[k] = append(slot[k], i)
	}

	objs := make([]types.ObjectIdentifier, len(keys))
	for i := range keys {
		objs[i] = types.ObjectIdentifier{Key: &keys[i]}
	}

	out, err := s.c.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: &s.c.cfg.Bucket,
		Delete: &types.Delete{Objects: objs, Quiet: aws.Bool(false)},
	}, contentMD5)

	ferr := fromSDK(ctx, opDelete, recorded{}, nil, err)
	if err != nil && ferr == nil {
		// classify reads a 404 NoSuchKey to a delete as the name deleted, which
		// holds for one key; to a whole batch it states nothing of any key.
		ferr = wrap(opDelete, http.StatusNotFound, "NoSuchKey", remote.ErrTransient)
	}

	got := make([]error, len(keys))
	if ferr != nil {
		for i := range got {
			got[i] = ferr
		}
	} else {
		got = deleteResults(keys, out.Deleted, out.Errors)
	}

	for i, k := range keys {
		for _, j := range slot[k] {
			errs[j] = got[i]
		}
	}
}

// deleteResults reads a DeleteObjects reply into one result per requested
// key, in order: nil for a key reported deleted once, the key's code through
// classify for a key reported failed once, and remote.ErrTransient for a key
// the reply leaves out or reports more than once. A result is read from the
// reply, never inferred from its silence. A key the reply names that was
// never requested is ignored.
func deleteResults(keys []string, deleted []types.DeletedObject, failed []types.Error) []error {
	seen := make(map[string]int, len(keys))
	code := make(map[string]string, len(failed))

	for _, d := range deleted {
		seen[aws.ToString(d.Key)]++
	}

	for _, e := range failed {
		k := aws.ToString(e.Key)
		seen[k]++
		code[k] = aws.ToString(e.Code)
	}

	out := make([]error, len(keys))
	for i, k := range keys {
		switch c, failedKey := code[k]; {
		case seen[k] == 0:
			out[i] = fmt.Errorf("remote/s3: %s: the reply does not name the key: %w", opDelete, remote.ErrTransient)
		case seen[k] > 1:
			out[i] = fmt.Errorf("remote/s3: %s: the reply names the key %d times: %w", opDelete, seen[k], remote.ErrTransient)
		case failedKey:
			out[i] = classify(opDeleteKey, 0, c, recorded{})
		}
	}

	return out
}

// contentMD5 makes a DeleteObjects request carry Content-MD5 and no other
// checksum header (RFC 4 Appendix C.1, C.3). The SDK marks the operation as
// requiring a checksum and adds a CRC32 of its own even at WhenRequired, so
// its middleware is removed by ID, which fails the call before it is sent if
// the SDK ever renames it.
func contentMD5(o *s3.Options) {
	o.APIOptions = append(o.APIOptions, func(st *middleware.Stack) error {
		if _, err := st.Finalize.Remove("AWSChecksum:ComputeInputPayloadChecksum"); err != nil {
			return err
		}

		return smithyhttp.AddContentChecksumMiddleware(st)
	})
}
