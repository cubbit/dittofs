package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
	"go.opentelemetry.io/otel/attribute"
)

// maxBody bounds a request's body.
const maxBody = 1 << 20

// errNotFound is a path no row names, an ID that does not parse, or a resource
// the caller may not know exists.
var errNotFound = fmt.Errorf("api: no such resource: %w", vfserr.ErrNotFound)

// request is a handler's decoded request: the ID its path names, its query,
// and its body, decoded strictly.
type request[B any] struct {
	ID    [16]byte
	Query url.Values
	Body  B
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
type adapter func(ctx context.Context, d *Deps, r *http.Request, id [16]byte) (answer, error)

// read adapts a read: its answer is 200 with what h returns.
func read[R any](h func(context.Context, request[none]) (R, error)) adapter {
	return func(ctx context.Context, _ *Deps, r *http.Request, id [16]byte) (answer, error) {
		resp, err := h(ctx, request[none]{ID: id, Query: r.URL.Query()})
		if err != nil {
			return answer{}, err
		}
		return jsonAnswer(http.StatusOK, resp)
	}
}

// write adapts a write: h runs inside one transaction of the store's Write,
// and the answer, 200 with what h returns, is made inside it, so a write
// answers only what committed.
func write[B, R any](h func(context.Context, request[B], entity.ControlPlaneTx) (R, error)) adapter {
	return func(ctx context.Context, d *Deps, r *http.Request, id [16]byte) (answer, error) {
		rq := request[B]{ID: id, Query: r.URL.Query()}
		if err := decode(http.MaxBytesReader(nil, r.Body, maxBody), &rq.Body); err != nil {
			return answer{}, err
		}
		if d.Store == nil {
			return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no control-plane store"}
		}
		var a answer
		err := d.Store.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			resp, err := h(ctx, rq, tx)
			if err != nil {
				return err
			}
			a, err = jsonAnswer(http.StatusOK, resp)
			return err
		})
		return a, err
	}
}

// call adapts a call that is no write of the adapter's: h makes the whole
// answer, and writes through its own Write if it writes.
func call[B any](h func(context.Context, request[B]) (answer, error)) adapter {
	return func(ctx context.Context, _ *Deps, r *http.Request, id [16]byte) (answer, error) {
		rq := request[B]{ID: id, Query: r.URL.Query()}
		if err := decode(http.MaxBytesReader(nil, r.Body, maxBody), &rq.Body); err != nil {
			return answer{}, err
		}
		return h(ctx, rq)
	}
}

func jsonAnswer(status int, v any) (answer, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return answer{}, err
	}
	return answer{status: status, header: http.Header{"Content-Type": {"application/json"}}, body: b}, nil
}

// group returns the handler of the rows sharing one method and pattern.
func (s *Server) group(rows []route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handle(w, r, rows) })
}

// handle answers r by the row of rows it names, none naming it when rows is
// nil, and records the call: its series and its span, named by the row's op.
// The span carries the resource's pattern, the path's ID and the outcome,
// never a body field.
func (s *Server) handle(w http.ResponseWriter, r *http.Request, rows []route) {
	start := time.Now()
	rt, id := pick(rows, r)
	op := "unmatched"
	if rt != nil {
		op = rt.op()
	}
	ctx, span := s.tracer.Start(r.Context(), op)
	a, err := s.run(ctx, r, rt, id)
	outcome := "ok"
	if err != nil {
		p, named := problemOf(err)
		outcome = string(p.Code)
		if !named {
			s.unmapped.Inc()
			outcome = "unmapped"
		}
		p.RequestID = rand.Text()
		a, _ = jsonAnswer(p.Status, p) // a Problem always encodes
		a.header.Set("Content-Type", apiv1.MediaTypeProblem)
	}
	span.SetAttributes(attribute.String("api.pattern", r.Pattern), attribute.String("api.id", id), attribute.String("api.outcome", outcome))
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
// Authenticate, every caller is refused 401.
func (s *Server) run(ctx context.Context, r *http.Request, rt *route, rawID string) (answer, error) {
	if rt == nil || rt.right != public {
		if s.d.Authenticate == nil {
			return answer{}, &refusal{code: apiv1.CodeUnauthenticated, detail: "the node authenticates no caller"}
		}
		c, err := s.d.Authenticate(r)
		if err != nil {
			return answer{}, err
		}
		ctx = auth.With(ctx, c)
		switch {
		case rt == nil:
			return answer{}, errNotFound
		case c.MustChange && !rt.mustChange:
			return answer{}, &refusal{code: apiv1.CodePasswordChangeRequired, detail: "change the account's password first"}
		}
		if err := allow(c, rt.right); err != nil {
			return answer{}, err
		}
	}
	var id [16]byte
	if strings.Contains(rt.pattern, "{id}") {
		// Every ID is a UUID in its canonical form, which ParseShareID
		// parses for any of them.
		sid, err := entity.ParseShareID(rawID)
		if err != nil {
			return answer{}, errNotFound
		}
		id = sid
	}
	return rt.serve(ctx, &s.d, r, id)
}
