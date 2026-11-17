package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"iter"
	"net/http"
	"slices"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/prometheus/client_golang/prometheus"
)

// opRead is an operation as a read answers it, with its ID and its
// generation, which the answer carries as its ETag.
type opRead struct {
	apiv1.Operation
	id  entity.OperationID
	gen uint64
}

// viewOp is op on the wire, its progress read by d.Progress, unknown when
// the node has none.
func viewOp(ctx context.Context, d Deps, op entity.Operation) (opRead, error) {
	v := apiv1.Operation{ID: idString(op.ID), Kind: apiv1.OpKind(op.Kind), Target: refOf(op.Target),
		State: apiv1.OpState(op.State.String()), Progress: apiv1.Progress{Total: -1},
		Started: op.Started, Updated: op.Updated, Result: json.RawMessage(op.Result)}
	v.Account = principalOf(op.Account)
	if d.Progress != nil {
		v.Progress = d.Progress(ctx, op)
	}
	if op.Error != nil {
		v.Error = new(apiv1.Problem)
		if err := json.Unmarshal(op.Error, v.Error); err != nil {
			return opRead{}, err
		}
	}
	return opRead{v, op.ID, op.Generation}, nil
}

// principalOf is p on the wire.
//
// decision: the zero principal is the schedule's, and reads as the zero
// Principal, with no kind and no ID, never as a kind and the zero ID, which
// would read as an account having acted. Overturned if the wire gains a
// field naming the schedule apart from the account.
func principalOf(p entity.Principal) apiv1.Principal {
	if p == (entity.Principal{}) {
		return apiv1.Principal{}
	}
	return apiv1.Principal{Kind: p.Kind.String(), ID: p.ID.String()}
}

// idString is an ID as the API spells it: every ID is a UUID in its
// canonical form.
func idString(id [16]byte) string { return entity.ShareID(id).String() }

// refOf is ref on the wire: its kind's name, and its ID, a UUID when it is
// one and its bytes in base64url otherwise.
//
// ponytail: an ID that is no UUID, such as a setting's scope and key, reads
// as opaque base64url. Spell each kind's own form once the resources whose
// IDs are not UUIDs have wire forms of their own.
func refOf(ref entity.ResourceRef) apiv1.Ref {
	r := apiv1.Ref{Kind: ref.Kind.String()}
	if len(ref.ID) == 16 {
		r.ID = idString([16]byte(ref.ID))
	} else {
		r.ID = base64.RawURLEncoding.EncodeToString(ref.ID)
	}
	return r
}

// readOp reads the operation the request names, 404 when there is none.
func readOp(ctx context.Context, d Deps, rq request[none]) (opRead, error) {
	op, found, err := d.Store.Operation(ctx, entity.OperationID(rq.ID))
	if err != nil {
		return opRead{}, err
	}
	if !found {
		return opRead{}, errNotFound
	}
	return viewOp(ctx, d, op)
}

// operation is GET /operations/{id}: ?wait= returns once the operation's
// state or phase changes.
var operation = watching[opRead]{
	read: readOp,
	tag:  func(o opRead) uint64 { return o.gen },
	until: changes(func(first, now opRead) bool {
		return first.State != now.State || first.Progress.Phase != now.Progress.Phase
	}),
}

// runningStates are the states an operation that has not ended holds, which
// the running index covers, by name.
var runningStates = func() map[string]entity.OpState {
	m := map[string]entity.OpState{}
	for _, s := range []entity.OpState{entity.OpPending, entity.OpRunning, entity.OpCancelling} {
		m[s.String()] = s
	}
	return m
}()

// operations is GET /operations, in ID order, filtered by state.
//
// decision: ?state= takes only a state an operation that has not ended
// holds, answered from the running index; an ended state is refused, since
// no index holds the ended operations and the filter would scan every kept
// one. Overturned if the store indexes operations by state.
//
// ponytail: a state filter reads the whole running index for each page and
// sorts it by ID, O(running). Index running operations by state and ID when
// the running count makes a page's read show in a profile.
var operations = listing[opRead, apiv1.Operation]{
	filters: []string{"state"},
	records: func(ctx context.Context, d Deps, after []byte, f map[string]string) iter.Seq2[opRead, error] {
		var from entity.OperationID
		copy(from[:], after)
		ops := d.Store.Operations(ctx, from)
		if name, ok := f["state"]; ok {
			state, ok := runningStates[name]
			if !ok {
				return func(yield func(opRead, error) bool) {
					yield(opRead{}, &fieldError{field: "state", detail: "not the state of an operation that has not ended: pending, running or cancelling"})
				}
			}
			ops = running(ctx, d, state, after)
		}
		return func(yield func(opRead, error) bool) {
			for op, err := range ops {
				var v opRead
				if err == nil {
					v, err = viewOp(ctx, d, op)
				}
				if !yield(v, err) || err != nil {
					return
				}
			}
		}
	},
	key:  func(o opRead) []byte { return o.id[:] },
	item: func(o opRead, _ perPage) apiv1.Operation { return o.Operation },
}

// running is every operation in state, from the running index, in ID
// order after the key after.
func running(ctx context.Context, d Deps, state entity.OpState, after []byte) iter.Seq2[entity.Operation, error] {
	return func(yield func(entity.Operation, error) bool) {
		var ops []entity.Operation
		for op, err := range d.Store.RunningOperations(ctx, entity.Operation{}) {
			if err != nil {
				yield(op, err)
				return
			}
			if op.State == state && bytes.Compare(op.ID[:], after) > 0 {
				ops = append(ops, op)
			}
		}
		slices.SortFunc(ops, func(a, b entity.Operation) int { return bytes.Compare(a.ID[:], b.ID[:]) })
		for _, op := range ops {
			if !yield(op, nil) {
				return
			}
		}
	}
}

// startRight is the right that may start each kind of operation, which a
// cancel of one asks of its caller; a kind not named asks for Installation.
var startRight = map[apiv1.OpKind]auth.Right{
	apiv1.OpRotateSigningKey:      auth.Installation,
	apiv1.OpResolvePrincipal:      auth.Principals,
	apiv1.OpTestDirectory:         auth.Installation,
	apiv1.OpJoinDomain:            auth.Installation,
	apiv1.OpRotateMachinePassword: auth.Installation,
	apiv1.OpShareDelete:           auth.Shares,
	apiv1.OpUserDelete:            auth.Principals,
	apiv1.OpGroupDelete:           auth.Principals,
	apiv1.OpNTHashDelete:          auth.Stores,
}

// cancel is POST /operations/{id}:cancel: 404 for an operation that does
// not exist, 403 for a caller that may not start its kind, and otherwise 409
// not_cancellable naming it, every refusal audited.
//
// decision: no kind in scope has a cancel, so every cancel is refused and
// changes nothing; it takes no Idempotency-Key and runs no write, and the
// right it asks is read from the operation's kind once the operation is
// read. Overturned when a kind gains a cancel, which then writes the
// operation through the write adapter.
//
// ponytail: a user's deletion asks for Principals, though deleting an
// account with a role asks for Accounts. Read the target's role when a
// cancel of a user's deletion exists.
func cancel(ctx context.Context, s *Server, r *http.Request, id [16]byte) (_ answer, err error) {
	rec := s.auditOf(ctx, r)
	rec.Target = entity.ResourceRef{Kind: entity.ResourceOperation, ID: id[:]}
	defer func() { s.refuse(ctx, rec, err) }()
	if s.d.Store == nil {
		return answer{}, errNoStore
	}
	op, found, err := s.d.Store.Operation(ctx, entity.OperationID(id))
	if err != nil {
		return answer{}, err
	}
	if !found {
		return answer{}, errNotFound
	}
	right, ok := startRight[apiv1.OpKind(op.Kind)]
	if !ok {
		right = auth.Installation
	}
	if c, _ := auth.CallerFrom(ctx); !holds(c, right) {
		return answer{}, &refusal{code: apiv1.CodeForbidden, detail: "the caller's role may not start an operation of this kind"}
	}
	return answer{}, &refusal{code: apiv1.CodeNotCancellable, detail: "an operation of kind " + op.Kind + " has no cancel", names: []string{idString(op.ID)}}
}

func holds(c auth.Caller, r auth.Right) bool {
	_, hold := auth.Allowed(c, r)
	return hold
}

// opsCollector exports api_operations and api_operation_oldest_seconds,
// read at scrape from the store's running index, so a scrape reads only the
// operations that have not ended.
type opsCollector struct {
	s             *Server
	count, oldest *prometheus.Desc
}

func newOpsCollector(s *Server) opsCollector {
	return opsCollector{s,
		prometheus.NewDesc("dittofs_api_operations", "Operations that have not ended, by kind and state.", []string{"kind", "state"}, nil),
		prometheus.NewDesc("dittofs_api_operation_oldest_seconds", "How long the oldest operation of each kind that has not ended has run.", []string{"kind"}, nil)}
}

func (c opsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.count; ch <- c.oldest }

// Collect reads the running index once, with the API's clock as now, the
// wall clock's when the API has none.
func (c opsCollector) Collect(ch chan<- prometheus.Metric) {
	if c.s.d.Store == nil {
		return
	}
	type kindState struct{ kind, state string }
	counts, first := map[kindState]int{}, map[string]time.Time{}
	for op, err := range c.s.d.Store.RunningOperations(context.Background(), entity.Operation{}) {
		if err != nil {
			ch <- prometheus.NewInvalidMetric(c.count, err)
			return
		}
		counts[kindState{op.Kind, op.State.String()}]++
		if t, ok := first[op.Kind]; !ok || op.Started.Before(t) {
			first[op.Kind] = op.Started
		}
	}
	now := time.Now()
	if c.s.d.Clock != nil {
		now = c.s.d.Clock.Now()
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.count, prometheus.GaugeValue, float64(n), k.kind, k.state)
	}
	for kind, t := range first {
		ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, now.Sub(t).Seconds(), kind)
	}
}
