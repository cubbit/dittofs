package api

import (
	"bytes"
	"context"
	"iter"
	"maps"
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
	Netgroup(ctx context.Context, name string) (entity.Netgroup, bool, error)
	// Users and Groups list the named users and groups in name order,
	// after the name after; User and Group read one, false for one marked
	// deleted.
	Users(ctx context.Context, after string) iter.Seq2[entity.User, error]
	User(ctx context.Context, p entity.Principal) (entity.User, bool, error)
	Groups(ctx context.Context, after string) iter.Seq2[entity.Group, error]
	Group(ctx context.Context, p entity.Principal) (entity.Group, bool, error)
	// ShareGrant is the grant naming p's ID on a share; Quotas lists a
	// share's quotas.
	ShareGrant(ctx context.Context, share entity.ShareID, p entity.Principal) (entity.ShareGrant, bool, error)
	Quotas(ctx context.Context, share entity.ShareID) iter.Seq2[entity.Quota, error]
	Settings(ctx context.Context, scope entity.SettingScope) iter.Seq2[entity.Setting, error]
	// Share is one share with its state and its export policy, a share
	// with none stored having the default one; a share never created, or
	// whose removal has ended, is not found.
	Share(ctx context.Context, id entity.ShareID) (entity.Share, entity.ExportPolicy, error)
	// SharesAfter lists the shares in folded-name order, after the one
	// named after; ShareNamed is the one a name names; NamespaceShares lists
	// those counted in a namespace, in ID order, after the ID after.
	SharesAfter(ctx context.Context, after string) iter.Seq2[entity.Share, error]
	ShareNamed(ctx context.Context, name string) (entity.Share, bool, error)
	NamespaceShares(ctx context.Context, ns entity.NamespaceID, after entity.ShareID) iter.Seq2[entity.Share, error]
	Operation(ctx context.Context, id entity.OperationID) (entity.Operation, bool, error)
	Operations(ctx context.Context, after entity.OperationID) iter.Seq2[entity.Operation, error]
	RunningOperations(ctx context.Context, after entity.Operation) iter.Seq2[entity.Operation, error]
	AuditRecords(ctx context.Context, f entity.AuditFilter, after []byte) iter.Seq2[entity.AuditRecord, error]
	// AuditPosition is a record's position, which AuditRecords resumes after.
	AuditPosition(a entity.AuditRecord) []byte
}

// errNoStore refuses a call on a node that holds no control-plane store.
var errNoStore = &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no control-plane store"}

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
// wire, from the record and what the page read once for every item. A local
// listing's records are this node's own, read from the node and never from
// the store (RFC 23 §4.2): its page reads nothing from the store.
type listing[E, T any] struct {
	lo, hi  []byte
	local   bool
	filters []string
	records func(ctx context.Context, d Deps, after []byte, filter map[string]string) iter.Seq2[E, error]
	key     func(E) []byte
	item    func(E, perPage) T
}

// nodeReads is the store's reads of the nodes: every registered node's
// status, read with the node records in one view, a node that has reported
// none having the zero status and a status whose node has no record left
// out; every node's record in node ID order, resuming after a node, the zero
// ID starting from the first; and one node's record.
type nodeReads interface {
	RegisteredStatuses(ctx context.Context) ([]entity.NodeStatus, error)
	NodeRecords(ctx context.Context, after entity.NodeID) iter.Seq2[entity.Node, error]
	Node(ctx context.Context, id entity.NodeID) (entity.Node, bool, error)
}

// perPage is what a listing reads once per page and hands every item: every
// registered node's status.
type perPage struct{ statuses []entity.NodeStatus }

// applied is on how many nodes the record ref names is applied, of every
// registered node: those whose position of the record's kind, through
// returns, has reached at, and that hold the record neither refused nor
// restart-pending. A kind that carries no position passes a nil through,
// so its records are applied on every node from their commit. A node
// removed from the installation counts in neither number, and a lapsed one
// counts toward of until it is removed, so its record reads as not applied.
func (p perPage) applied(ref entity.ResourceRef, at uint64, through func(entity.NodeStatus) uint64) apiv1.Applied {
	a := apiv1.Applied{Of: len(p.statuses)}
	is := func(r entity.ResourceRef) bool { return r.Kind == ref.Kind && bytes.Equal(r.ID, ref.ID) }
	for _, st := range p.statuses {
		if through != nil && through(st) < at ||
			slices.ContainsFunc(st.Refused, func(r entity.RefusedRecord) bool { return is(r.Record) }) ||
			slices.ContainsFunc(st.RestartPending, is) {
			continue
		}
		a.Nodes++
	}
	return a
}

// list adapts a listing: its answer is 200 with one page of at most ?limit=
// items, 100 without it and never more than 1,000, resuming after the
// cursor ?after= names, and with next, the cursor of its last item, when one
// more follows. A cursor names a key, never an offset, so records inserted
// or deleted behind it make the next page neither skip nor repeat one that
// was there when the listing started. A query parameter the listing does not
// filter on is refused, naming it: a filter the store would answer by a scan
// is not offered. With several at fault, the 400 names the first in name
// order. Every registered node's status is read once for the page, whatever
// its size, and handed to each item, but for a local listing, which reads
// none and needs no store.
func list[E, T any](l listing[E, T]) adapter {
	return func(ctx context.Context, s *Server, r *http.Request, _ *route, _ [16]byte) (answer, error) {
		limit, after, filter := defaultLimit, []byte(nil), map[string]string{}
		q := r.URL.Query()
		for _, k := range slices.Sorted(maps.Keys(q)) {
			v := q[k]
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
		var pp perPage
		if !l.local {
			if s.d.Store == nil || s.d.Nodes == nil {
				return answer{}, errNoStore
			}
			var err error
			if pp.statuses, err = s.d.Nodes.RegisteredStatuses(ctx); err != nil {
				return answer{}, err
			}
		}
		page := apiv1.Page[T]{Items: []T{}}
		var last []byte
		for e, err := range l.records(ctx, s.d, after, filter) {
			if err != nil {
				return answer{}, err
			}
			if len(page.Items) == limit {
				page.Next = encodeCursor(last)
				break
			}
			page.Items = append(page.Items, l.item(e, pp))
			last = l.key(e)
		}
		return jsonAnswer(http.StatusOK, page)
	}
}
