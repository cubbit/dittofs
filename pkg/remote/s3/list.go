package s3

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/pkg/remote"
)

// List reads one page of at most n keys under the prefix's blocks/ in one
// ListObjectsV2, starting after the cursor's key (none for the zero cursor), in
// ascending key order, which is ascending name order (RFC 4 §4.6, Appendix
// C.3). n counts the keys listed, as the service's max-keys does, so a page may
// hold fewer blocks than keys: a key that is not a block name is skipped,
// counted and logged at Warn, never guessed at. next is the page's last key,
// block or not, or after when the page holds none, and done follows the reply's
// IsTruncated. A non-zero cursor outside the prefix's blocks/, or an n of zero
// or less, is remote.ErrInvalid with no request.
func (s *store) List(ctx context.Context, after remote.Cursor, n int) ([]remote.Info, remote.Cursor, bool, error) {
	start := time.Now()
	page, next, done, foreign, err := s.list(ctx, after, n)
	s.record(opList, err, time.Since(start), 0)

	for _, k := range foreign {
		s.c.rec.ForeignKey()
		s.c.log.Warn("remote list skipped a key that is not a block name", "key", k)
	}

	return page, next, done, err
}

// list is List's request and its reading, returning the foreign keys skipped.
func (s *store) list(ctx context.Context, after remote.Cursor, n int) ([]remote.Info, remote.Cursor, bool, []string, error) {
	if err := s.refuseClosed(opList); err != nil {
		return nil, after, false, nil, err
	}

	blocks := s.prefix + "blocks/"

	switch {
	case n <= 0:
		return nil, after, false, nil, fmt.Errorf("remote/s3: list of %d keys: %w", n, remote.ErrInvalid)
	case after != "" && !strings.HasPrefix(string(after), blocks):
		return nil, after, false, nil, fmt.Errorf("remote/s3: list after a cursor outside %s: %w", blocks, remote.ErrInvalid)
	}

	in := &s3.ListObjectsV2Input{
		Bucket:  &s.c.cfg.Bucket,
		Prefix:  &blocks,
		MaxKeys: new(int32(min(n, math.MaxInt32))),
	}
	if after != "" {
		in.StartAfter = new(string(after))
	}

	out, err := s.c.api.ListObjectsV2(ctx, in)
	if err := fromSDK(ctx, opList, recorded{}, nil, err); err != nil {
		return nil, after, false, nil, err
	}

	truncated := out.IsTruncated != nil && *out.IsTruncated

	return s.readPage(ctx, after, n, out.Contents, truncated)
}

// readPage reads one ListObjectsV2 reply: the blocks among its keys, the
// cursor of its last key, whether the walk is done, and the keys that are not
// block names. A reply of more than n keys is read as its first n, truncated.
// A page marked truncated that holds no key cannot advance, and a key outside
// the request's prefix or not sorting past the one before it is a reply the
// walk cannot trust: each is remote.ErrTransient. ctx is checked before each
// key, so a context ended mid-page ends the call with its error.
func (s *store) readPage(ctx context.Context, after remote.Cursor, n int, contents []types.Object, truncated bool) ([]remote.Info, remote.Cursor, bool, []string, error) {
	if len(contents) > n {
		contents, truncated = contents[:n], true
	}

	if len(contents) == 0 {
		if truncated {
			return nil, after, false, nil, fmt.Errorf("remote/s3: list: a truncated page holds no key: %w", remote.ErrTransient)
		}

		return nil, after, true, nil, nil
	}

	blocks := s.prefix + "blocks/"
	page := make([]remote.Info, 0, len(contents))
	prev := string(after)

	var foreign []string

	for _, o := range contents {
		if err := ctx.Err(); err != nil {
			return nil, after, false, nil, err
		}

		k := ""
		if o.Key != nil {
			k = *o.Key
		}

		if !strings.HasPrefix(k, blocks) || k <= prev {
			return nil, after, false, nil, fmt.Errorf("remote/s3: list: a key outside %s or out of order: %w", blocks, remote.ErrTransient)
		}

		prev = k

		name, ok := s.nameFromKey(k)
		if !ok {
			foreign = append(foreign, k)
			continue
		}

		info := remote.Info{Name: name}
		if o.Size != nil {
			info.Size = *o.Size
		}
		if o.LastModified != nil {
			info.ModTime = *o.LastModified
		}

		page = append(page, info)
	}

	return page, remote.Cursor(prev), !truncated, foreign, nil
}
