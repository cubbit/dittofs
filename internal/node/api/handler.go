package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
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

// request is a handler's decoded request: the ID its path names, its body,
// decoded strictly, and, for a write, the tag its record must have, which the
// handler passes to the store's write as its expect: the one If-Match names,
// or zero under If-None-Match: *.
type request[B any] struct {
	ID     [16]byte
	Body   B
	Expect uint64
}

// none is the body of a request that carries none.
type none struct{}

// answer is what a call sends: its status, its headers and its body.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// adapter runs a row's handler for a caller that holds the row's right, and
// returns what to send. No handler is handed the http.ResponseWriter, so none
// answers before its adapter has the whole answer: a write's is made inside
// its transaction and sent only once that has committed.
type adapter func(ctx context.Context, s *Server, r *http.Request, id [16]byte) (answer, error)

// read adapts a read: its answer is 200 with what h returns.
func read[R any](h func(context.Context, request[none]) (R, error)) adapter {
	return func(ctx context.Context, _ *Server, _ *http.Request, id [16]byte) (answer, error) {
		resp, err := h(ctx, request[none]{ID: id})
		if err != nil {
			return answer{}, err
		}
		return jsonAnswer(http.StatusOK, resp)
	}
}

// call adapts a call that is no write of the adapter's: h makes the whole
// answer. If it writes, it does so through a function of package auth, the
// one place besides the write adapter that calls the store's Write.
func call[B any](h func(context.Context, request[B]) (answer, error)) adapter {
	return func(ctx context.Context, _ *Server, r *http.Request, id [16]byte) (answer, error) {
		rq, err := decodeRequest[B](r, id)
		if err != nil {
			return answer{}, err
		}
		return h(ctx, rq)
	}
}

// decodeRequest is the request naming id, its body read from r, at most
// maxBody bytes of it.
func decodeRequest[B any](r *http.Request, id [16]byte) (request[B], error) {
	rq := request[B]{ID: id}
	return rq, decode(http.MaxBytesReader(nil, r.Body, maxBody), &rq.Body)
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
// nil, and records the call: its series and its span, named by the row's op,
// and, for an error the problem table does not name, a log line carrying
// the request's ID. The span carries the resource's pattern, the path's ID
// once it parses and the outcome, never a body field.
func (s *Server) handle(w http.ResponseWriter, r *http.Request, rows []route) {
	start := time.Now()
	ctx := context.WithValue(r.Context(), requestIDKey{}, rand.Text())
	rt, id := pick(rows, r)
	op := "unmatched"
	if rt != nil {
		op = rt.op()
	}
	ctx, span := s.tracer.Start(ctx, op)
	a, err := s.run(ctx, r, rt, id)
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

// pick returns the row of rows r names, its verb cut from the path's {id} at
// the first colon, and the ID left; nil when none is named.
func pick(rows []route, r *http.Request) (*route, string) {
	id, verb, _ := strings.Cut(r.PathValue("id"), ":")
	for i := range rows {
		if rows[i].verb == verb {
			return &rows[i], id
		}
	}
	return nil, id
}

// run authenticates r unless its row is public, refuses a caller the row
// does not admit, parses the path's ID and runs the row's adapter. With no
// Authenticate, every caller is refused 401. A refusal of an authenticated
// caller here, before the adapter, is audited against the row's kind unless
// r is a GET or a HEAD; the write adapter audits its own.
//
// decision: a path no row names is refused unaudited. It names no resource,
// and the store refuses an audit record whose target has no kind, so its
// record would fail to write; the refusal is still counted by op, as
// unmatched. Overturned if the store admits an audit record targeting no
// resource.
//
// decision: a GET or a HEAD refused after authentication is not audited,
// narrower than auditing every request refused after authentication. A read
// changes nothing, and auditing each refused read would let any
// authenticated caller fill the audit with records at the rate it can send;
// the refusal is still counted by op and outcome. Overturned if a refused
// read must be attributable to its caller, such as a probe of resources the
// role cannot see, which then needs a rate-limited or sampled audit.
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
	id, err := admit(ctx, rt, rawID)
	if err != nil {
		if _, authed := auth.CallerFrom(ctx); authed && rt != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			rec := s.auditOf(ctx, r)
			rec.Target.Kind = rt.kind
			s.refuse(ctx, rec, err)
		}
		return answer{}, err
	}
	return rt.serve(ctx, s, r, id)
}

// admit refuses a row the caller ctx carries may not call and an ID that
// does not parse, and returns the ID the path names. A caller's rights are
// checked before whether it must change its password, so that refusal
// reveals no row the caller may not read.
func admit(ctx context.Context, rt *route, rawID string) (id [16]byte, err error) {
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
	if strings.Contains(rt.pattern, "{id}") {
		// Every ID is a UUID in its canonical form, which ParseShareID
		// parses for any of them.
		sid, err := entity.ParseShareID(rawID)
		if err != nil {
			return id, errNotFound
		}
		id = sid
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("api.id", sid.String()))
	}
	return id, nil
}
