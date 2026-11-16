package api

import (
	"context"
	"iter"
	"net/http"
	"slices"
	"strconv"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// reads is the metadata store's reads the API answers from. A read goes to
// the store, never to a node's cache, so a write any node acknowledged reads
// back on every node.
type reads interface {
	Netgroups(ctx context.Context, after string) iter.Seq2[entity.Netgroup, error]
	Settings(ctx context.Context, scope entity.SettingScope) iter.Seq2[entity.Setting, error]
}

// Listing bounds: the page size a listing answers without ?limit=, and the
// most it answers with one.
const (
	defaultLimit = 100
	maxLimit     = 1000
)

// listing is what a list row knows of its records: the range [lo, hi) of
// the keys its cursor may name, a nil hi bounding nothing; the query
// parameters it filters on, each an indexed field of its records; records,
// which yields its records in key order, resuming after the key after, with
// the filters the request named; each record's key; and its item on the
// wire.
type listing[E, T any] struct {
	lo, hi  []byte
	filters []string
	records func(ctx context.Context, st reads, after []byte, filter map[string]string) iter.Seq2[E, error]
	key     func(E) []byte
	item    func(E) T
}

// list adapts a listing: its answer is 200 with one page of at most ?limit=
// items, 100 without it and never more than 1,000, resuming after the
// cursor ?after= names, and with next, the cursor of its last item, when one
// more follows. A cursor names a key, never an offset, so records inserted
// or deleted behind it make the next page neither skip nor repeat one that
// was there when the listing started. A query parameter the listing does not
// filter on is refused, naming it: a filter the store would answer by a scan
// is not offered.
func list[E, T any](l listing[E, T]) adapter {
	return func(ctx context.Context, s *Server, r *http.Request, _ [16]byte) (answer, error) {
		limit, after, filter := defaultLimit, []byte(nil), map[string]string{}
		for k, v := range r.URL.Query() {
			if len(v) != 1 {
				return answer{}, &fieldError{field: k, detail: "given more than once"}
			}
			var err error
			switch {
			case k == "limit":
				limit, err = strconv.Atoi(v[0])
				if err != nil || limit < 1 || limit > maxLimit {
					return answer{}, &fieldError{field: k, detail: "not a page size from 1 to " + strconv.Itoa(maxLimit)}
				}
			case k == "after":
				if after, err = decodeCursor(v[0], l.lo, l.hi); err != nil {
					return answer{}, err
				}
			case slices.Contains(l.filters, k):
				filter[k] = v[0]
			default:
				return answer{}, &fieldError{field: k, detail: "not a filter of this listing"}
			}
		}
		if s.d.Reads == nil {
			return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no control-plane store"}
		}
		page := apiv1.Page[T]{Items: []T{}}
		var last []byte
		for e, err := range l.records(ctx, s.d.Reads, after, filter) {
			if err != nil {
				return answer{}, err
			}
			if len(page.Items) == limit {
				page.Next = encodeCursor(last)
				break
			}
			page.Items = append(page.Items, l.item(e))
			last = l.key(e)
		}
		return jsonAnswer(http.StatusOK, page)
	}
}
