package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// maxBody bounds a request's body.
const maxBody = 1 << 20

// errNotFound is a path no row names, an ID that does not parse, or a resource
// the caller may not know exists.
var errNotFound = fmt.Errorf("api: no such resource: %w", vfserr.ErrNotFound)

// request is a handler's decoded request: the ID its path names, the
// segment its {sub} names below it, empty for none, its body,
// decoded strictly, and, for a write, the tag its record must have, which the
// handler passes to the store's write as its expect: the one If-Match names,
// or zero under If-None-Match: *.
type request[B any] struct {
	ID     [16]byte
	Sub    string
	Body   B
	Expect uint64
	Query  url.Values // a write's query, holding only parameters its row takes
	d      Deps       // the serving server's, which every adapter sets, for a handler and a pre-write step
	// own commits fn in a Write of its own, before the write's, audited as
	// the request arrived with target, change and an applied outcome, for a
	// pre-write step whose change must commit first; set by the write adapter.
	own func(ctx context.Context, target entity.ResourceRef, change []byte, fn func(entity.ControlPlaneTx) error) error
	// start writes, in the write's transaction, an operation of kind on
	// target, which the write then answers 202 with, for a handler whose
	// write starts one only sometimes; set by the write adapter inside it.
	start func(kind apiv1.OpKind, target entity.ResourceRef) error
}

// none is the body of a request that carries none.
type none struct{}

// answer is what a call sends: its status, its headers and its body; or,
// for a long poll that must wait, park, which handle runs for the answer to
// send, its write deadline moved past wait.
type answer struct {
	status int
	header http.Header
	body   []byte
	park   func() (answer, error)
	wait   time.Duration
}

// adapter runs a row's handler for a caller that holds the row's right, and
// returns what to send. No handler is handed the http.ResponseWriter, so none
// answers before its adapter has the whole answer: a write's is made inside
// its transaction and sent only once that has committed. The rest is what
// the row carries on the wire, which the API's OpenAPI document is built
// from: its form; the request body's type and the success answer's, nil for
// none; a listing's filters; the words a watched read's ?wait= takes beside
// a duration, and the errors its wait refuses with; for a write, whether a
// POST action changes the spec, whether its answer is recorded under its idempotency
// key, and whether it waits for the node to apply it; and whether the right
// the call needs is the one that starts the kind of operation it names.
type adapter struct {
	run         func(ctx context.Context, s *Server, r *http.Request, rt *route, id [16]byte) (answer, error)
	form        form
	body, out   reflect.Type
	filters     []string
	words       []param
	refuses     []error
	changesSpec bool
	replays     bool
	raises      bool
	roleByKind  bool
}

// form is an adapter's kind of call, which decides the parameters and
// headers it takes and the answers it gives.
type form uint8

const (
	formRead  form = iota // a plain read
	formWatch             // a read with an ETag and ?wait=
	formList              // a listing, paged by ?limit= and ?after=
	formWrite             // a write, its preconditions and Idempotency-Key
	formCall              // a call that makes its whole answer
)

// read adapts a read whose answer carries no ETag: its answer is 200 with
// what h returns from the server's Deps. A record with a tag is read through
// watch, which takes the record's tag, so none of those answers without its
// ETag.
func read[R any](h func(context.Context, Deps, request[none]) (R, error)) adapter {
	return adapter{form: formRead, out: reflect.TypeFor[R](), run: func(ctx context.Context, s *Server, r *http.Request, _ *route, id [16]byte) (answer, error) {
		return jsonOr(h(ctx, s.d, request[none]{ID: id, Sub: r.PathValue("sub"), d: s.d}))
	}}
}

// call adapts a call that is no write of the adapter's: h makes the whole
// answer. If it writes, it does so through a function of package auth, the
// one place besides the write adapter that calls the store's Write.
func call[B any](h func(context.Context, request[B]) (answer, error)) adapter {
	return adapter{form: formCall, body: reflect.TypeFor[B](), run: func(ctx context.Context, s *Server, r *http.Request, _ *route, id [16]byte) (answer, error) {
		rq, err := decodeRequest[B](r, id)
		if err != nil {
			return answer{}, err
		}
		rq.d = s.d
		return h(ctx, rq)
	}}
}

// decodeRequest is the request naming id, its body read from r, at most
// maxBody bytes of it.
func decodeRequest[B any](r *http.Request, id [16]byte) (request[B], error) {
	rq := request[B]{ID: id}
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return rq, &fieldError{detail: err.Error()}
	}
	return rq, decode(b, &rq.Body)
}

// jsonOr is 200 with v, or err.
func jsonOr(v any, err error) (answer, error) {
	if err != nil {
		return answer{}, err
	}
	return jsonAnswer(http.StatusOK, v)
}

func jsonAnswer(status int, v any) (answer, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return answer{}, err
	}
	return answer{status: status, header: http.Header{"Content-Type": {"application/json"}}, body: b}, nil
}

type requestIDKey struct{}

// requestID is the ID of the request ctx serves, as its problem body, its
// log lines and its audit record carry it.
func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// handle answers r by the row of rows it names, none naming it when rows is
// nil, once enter admits it, and records the call: its series and its span,
// named by the row's op, and, for an error the problem table does not name,
// a log line carrying the request's ID. The span carries the resource's
// pattern, the path's ID once it parses and the outcome, never a body field.
func (s *Server) handle(w http.ResponseWriter, r *http.Request, rows []route) {
	start := time.Now()
	ctx := context.WithValue(r.Context(), requestIDKey{}, rand.Text())
	rt, id := pick(rows, r)
	op := "unmatched"
	if rt != nil {
		op = rt.op()
	}
	ctx, span := s.tracer.Start(ctx, op)
	var a answer
	release, err := s.enter(rt)
	if err == nil {
		defer release()
		a, err = s.run(ctx, r, rt, id)
	}
	if err == nil && a.park != nil {
		// A parked poll holds no call slot: it gives its slot back here,
		// before it parks. Its write deadline, the wall clock's whatever
		// clock the wait is on, moves past its wait, so the write timeout
		// that bounds every other call does not cut it off. The read timeout
		// needs no such move: the server clears the read deadline once the
		// body is read.
		release()
		if err = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(a.wait + s.api.WriteTimeout)); err == nil {
			a, err = a.park()
		}
	}
	outcome := "ok"
	if err != nil {
		p, named := problemOf(err)
		outcome = string(p.Code)
		p.RequestID = requestID(ctx)
		if !named {
			s.unmapped.Inc()
			outcome = "unmapped"
			s.d.Logger.Error("node api: an error the problem table does not name", "op", op, "request_id", p.RequestID, "err", err)
		}
		a, _ = jsonAnswer(p.Status, p) // a Problem always encodes
		a.header.Set("Content-Type", apiv1.MediaTypeProblem)
		if wait := retryAfter(err, p.Status, p.Retryable); wait > 0 {
			a.header.Set("Retry-After", strconv.FormatInt(int64((wait+time.Second-1)/time.Second), 10))
		}
	}
	span.SetAttributes(attribute.String("api.pattern", r.Pattern), attribute.String("api.outcome", outcome))
	span.End()
	s.requests.WithLabelValues(op, outcome).Inc()
	s.seconds.WithLabelValues(op).Observe(time.Since(start).Seconds())
	for k, v := range a.header {
		w.Header()[k] = v
	}
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body)
}

// retryAfter is how long a caller refused with err, answered status, should
// wait before trying again: the wait err carries, none when it carries a
// negative one, or else one second for a retryable 503 and none for any
// other answer.
func retryAfter(err error, status int, retryable bool) time.Duration {
	var w interface{ RetryAfter() time.Duration }
	if errors.As(err, &w) && w.RetryAfter() != 0 {
		return max(w.RetryAfter(), 0)
	}
	if status == http.StatusServiceUnavailable && retryable {
		return time.Second
	}
	return 0
}

// pick returns the row of rows r names, its verb cut from the path's {id} at
// the first colon, and the ID left; nil when none is named. A row with no
// {id} is served at a pattern of its own, so it is the one row its pattern
// names.
func pick(rows []route, r *http.Request) (*route, string) {
	id, verb, _ := strings.Cut(r.PathValue("id"), ":")
	for i := range rows {
		if rows[i].verb == verb || !rows[i].hasID() {
			return &rows[i], id
		}
	}
	return nil, id
}

// run authenticates r unless its row is public, refuses a caller the row
// does not admit, parses the path's ID and runs the row's adapter. With no
// Authenticate, every caller is refused 401. A refusal of an authenticated
// caller here, before the adapter, is audited against the row's kind and
// the path's ID when it parses, or, on an installation row, whose path
// names none, the installation's ID; the write adapter audits its own.
//
// decision: a path no row names is refused unaudited. It names no resource,
// and the store refuses an audit record whose target has no kind, so its
// record would fail to write; the refusal is still counted by op, as
// unmatched. Overturned if the store admits an audit record targeting no
// resource.
func (s *Server) run(ctx context.Context, r *http.Request, rt *route, rawID string) (answer, error) {
	if rt == nil || rt.right != auth.Public {
		if s.d.Authenticate == nil {
			return answer{}, &refusal{code: apiv1.CodeUnauthenticated, detail: "the node authenticates no caller"}
		}
		c, err := s.d.Authenticate(r)
		if err != nil {
			return answer{}, err
		}
		ctx = auth.With(ctx, c)
	}
	id, err := admit(ctx, rt, rawID, r.PathValue("sub"))
	if err != nil {
		if _, authed := auth.CallerFrom(ctx); authed && rt != nil {
			rec := s.auditOf(ctx, r)
			rec.Target.Kind = rt.kind
			// Named as the row's adapter names it, so an applied and a
			// refused write of one record share a target: an installation
			// row's path names no ID, so the installation's is read.
			sid, perr := entity.ParseShareID(rawID)
			switch hasID := rt.hasID(); {
			case (hasID && perr == nil) || (!hasID && rt.target != nil):
				rec.Target.ID = rt.ref(sid, r.PathValue("sub"))
			case !hasID && rt.kind == entity.ResourceInstallation && s.d.Store != nil:
				if in, err := s.d.Store.Installation(ctx); err == nil {
					rec.Target.ID = in.ID[:]
				}
			}
			s.refuse(ctx, rec, err)
		}
		return answer{}, err
	}
	return rt.serve.run(ctx, s, r, rt, id)
}

// admit refuses a row the caller ctx carries may not call, and an ID or a
// {sub} that does not parse, and returns the ID the path names. A caller's
// rights are checked before whether it must change its password, so that
// refusal reveals no row the caller may not read.
func admit(ctx context.Context, rt *route, rawID, sub string) (id [16]byte, err error) {
	if rt == nil {
		return id, errNotFound
	}
	if c, ok := auth.CallerFrom(ctx); ok {
		read, hold := auth.Allowed(c, rt.right)
		switch {
		case !read:
			return id, errNotFound
		case !hold:
			return id, &refusal{code: apiv1.CodeForbidden, detail: "the caller's role does not permit this call"}
		case c.MustChange && !rt.mustChange:
			return id, &refusal{code: apiv1.CodePasswordChangeRequired, detail: "change the account's password first"}
		}
	}
	if rt.hasID() {
		// Every ID is a UUID in its canonical form, which ParseShareID
		// parses for any of them.
		sid, err := entity.ParseShareID(rawID)
		if err != nil {
			return id, errNotFound
		}
		id = sid
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("api.id", sid.String()))
	}
	if rt.target != nil {
		if _, err := rt.target(id, sub); err != nil {
			return id, err
		}
	}
	return id, nil
}
