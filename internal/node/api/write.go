package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// maxKey bounds an Idempotency-Key, in bytes.
const maxKey = 255

// writing is what a write row's adapter knows of the resource its path
// names: its kind; read, which returns its record, tracked in the write's
// transaction, as the body a PUT of it sends, with its tag, ok false when
// there is none; and whether its commit raises the principals or settings
// generation.
type writing[B any] struct {
	kind   entity.ResourceKind
	read   func(tx entity.ControlPlaneTx, id [16]byte) (doc B, tag uint64, ok bool, err error)
	raises bool
}

// write adapts a write. Its checks run in this order: the body is decoded;
// then, inside the store's Write, the request's idempotency key is looked
// up; then the headers its method requires are required and checked against
// the record's tag, read tracked; then h runs. So a key recorded for another
// request answers 422 before a missing If-Match answers 428, and two writes
// naming one tag cannot both commit. prepare, when not nil, runs once before
// Write, never on a conflict retry, and its result is h's in; for a PATCH it
// sees the patch, decoded as the body. A PATCH body is
// a merge patch of the document read returns, and the result is decoded as
// strictly as a PUT's body, so a patch can set nothing a PUT could not.
//
// The answer, 200 with what h returns and the record's tag as its ETag, is
// made inside the transaction and recorded under the key, so a replay
// answers exactly what was first sent and changes nothing. The audit record
// commits with the change; a refused request records no key and is audited
// in a transaction of its own. After a commit that raises the principals or
// settings generation, the answer waits for the node to apply it.
func write[B, I, R any](w writing[B], prepare func(context.Context, request[B]) (I, error), h func(context.Context, request[B], I, entity.ControlPlaneTx) (R, error)) adapter {
	return func(ctx context.Context, s *Server, r *http.Request, id [16]byte) (a answer, err error) {
		c, _ := auth.CallerFrom(ctx)
		key := r.Header.Get("Idempotency-Key")
		rec := entity.AuditRecord{Actor: c.Principal, Node: s.d.Node, Source: r.RemoteAddr,
			Request: requestOf(ctx, r, key), Target: entity.ResourceRef{Kind: w.kind, ID: id[:]}}
		refused := rec // as the request arrived, holding nothing a failed attempt set
		defer func() {
			if err != nil {
				s.refuse(ctx, refused, err)
			}
		}()
		if s.d.Store == nil {
			return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no control-plane store"}
		}
		body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
		if err != nil {
			return answer{}, &fieldError{detail: err.Error()}
		}
		if len(key) > maxKey {
			return answer{}, &fieldError{field: "Idempotency-Key", detail: "longer than " + strconv.Itoa(maxKey) + " bytes"}
		}
		rq := request[B]{ID: id}
		if err := decode(bytes.NewReader(body), &rq.Body); err != nil {
			return answer{}, err
		}
		patch := r.Method == http.MethodPatch
		if patch {
			if _, err := mergePatch([]byte("{}"), body); err != nil {
				return answer{}, err
			}
		}
		var in I
		if prepare != nil {
			if in, err = prepare(ctx, rq); err != nil {
				return answer{}, err
			}
		}
		var idem *entity.IdempotencyRecord
		if key != "" {
			idem = &entity.IdempotencyRecord{Account: c.Principal, Key: key, Digest: digest(r, body)}
		}
		err = s.d.Store.Write(ctx, entity.WriteMeta{Audit: &rec, Idempotency: idem}, func(tx entity.ControlPlaneTx) error {
			rq := rq
			doc, tag, ok, err := w.read(tx, id)
			if err != nil {
				return err
			}
			if err := precondition(r, key, tag, ok); err != nil {
				return err
			}
			rq.Expect = tag
			if patch {
				b, err := json.Marshal(doc)
				if err != nil {
					return err
				}
				if b, err = mergePatch(b, body); err != nil {
					return err
				}
				rq.Body = *new(B)
				if err := decode(bytes.NewReader(b), &rq.Body); err != nil {
					return err
				}
			}
			resp, err := h(ctx, rq, in, tx)
			if err != nil {
				return err
			}
			after, afterTag, exists, err := w.read(tx, id)
			if err != nil {
				return err
			}
			if rec.Change, err = change(present(doc, ok), present(after, exists)); err != nil {
				return err
			}
			rec.Before, rec.After, rec.Outcome = tag, afterTag, []byte("applied")
			if a, err = jsonAnswer(http.StatusOK, resp); err != nil {
				return err
			}
			if exists {
				a.header.Set("ETag", etag(afterTag))
			}
			if idem != nil {
				idem.Answer, err = json.Marshal(recorded{a.status, a.header, a.body})
			}
			return err
		})
		var replay *entity.IdempotentReplay
		if errors.As(err, &replay) {
			s.replays.Inc()
			var rc recorded
			if err := json.Unmarshal(replay.Answer, &rc); err != nil {
				return answer{}, err
			}
			return answer{rc.Status, rc.Header, rc.Body}, nil
		}
		if err != nil {
			return answer{}, err
		}
		s.audits.WithLabelValues("applied").Inc()
		if idem != nil {
			s.idempotencyWritten.Inc()
		}
		if w.raises && s.d.Applied != nil {
			// decision: the answer waits at most 1 s for the node to
			// apply a write raising the principals or settings
			// generation, the bound a reconcile pass is held to, and
			// answers the commit's own result whatever the wait
			// returns: the write is durable, and status.applied says
			// where it is applied. Overturned if a pass on a healthy
			// node regularly outlasts the second.
			//
			// ponytail: the second is the wall clock's. Move it to the
			// API's injected clock when the API is handed one.
			wctx, cancel := context.WithTimeout(ctx, time.Second)
			if werr := s.d.Applied(wctx); werr != nil {
				s.d.Logger.Debug("node api: a committed write is not yet applied on this node", "request_id", requestID(ctx), "err", werr)
			}
			cancel()
		}
		return a, nil
	}
}

// precondition refuses a request lacking a header its method requires, 428:
// a POST or a DELETE its Idempotency-Key, and a PUT, a PATCH or a DELETE its
// If-Match; and 412, naming the record's tag when there is one, a request
// whose If-Match is not the record's tag or whose PUT with If-None-Match: *
// finds a record. A tag is compared only for equality.
//
// decision: If-Match is required, not optional. Optional, every script
// omits it, and a lost update is the default behaviour; the CLI reads before
// it writes, so the cost falls on nobody who uses it. Overturned if a client
// class appears that cannot read first, which would get a named
// unconditional action, not a missing header.
func precondition(r *http.Request, key string, tag uint64, ok bool) error {
	m := r.Method
	if key == "" && (m == http.MethodPost || m == http.MethodDelete) {
		return &refusal{code: apiv1.CodePreconditionRequired, detail: "a " + m + " carries an Idempotency-Key"}
	}
	match := r.Header.Get("If-Match")
	switch {
	case m == http.MethodPost:
		return nil
	case m == http.MethodPut && r.Header.Get("If-None-Match") == "*":
		if ok {
			return mismatch(tag)
		}
		return nil
	case match == "":
		return &refusal{code: apiv1.CodePreconditionRequired, detail: "a " + m + " carries If-Match, or a PUT creating its record If-None-Match: *"}
	case !ok:
		return &refusal{code: apiv1.CodeGenerationMismatch, detail: "no record holds the tag If-Match names"}
	case match != etag(tag):
		return mismatch(tag)
	}
	return nil
}

// mismatch refuses a write whose record's tag is not the one it expects,
// naming that tag.
func mismatch(tag uint64) error {
	return &refusal{code: apiv1.CodeGenerationMismatch, detail: "the record's tag is not the one the request expects", names: []string{etag(tag)}}
}

// etag is a record's tag as the ETag header carries it.
func etag(tag uint64) string { return `"` + strconv.FormatUint(tag, 10) + `"` }

// digest is a request's identity under its idempotency key: its method, its
// path with its query, and its body.
func digest(r *http.Request, body []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(r.Method + " " + r.URL.RequestURI() + "\n"))
	h.Write(body)
	return [32]byte(h.Sum(nil))
}

// recorded is an answer as an idempotency record holds it.
type recorded struct {
	Status int         `json:"status"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body"`
}

// requestOf is a write's request as its audit record holds it: its method,
// its path, its request ID and its idempotency key, never its body.
func requestOf(ctx context.Context, r *http.Request, key string) []byte {
	b, _ := json.Marshal(struct { // strings always encode
		Method         string `json:"method"`
		Path           string `json:"path"`
		RequestID      string `json:"request_id"`
		IdempotencyKey string `json:"idempotency_key,omitempty"`
	}{r.Method, r.URL.Path, requestID(ctx), key})
	return b
}

// present is doc when its record exists, and nil when it does not.
func present(doc any, ok bool) any {
	if !ok {
		return nil
	}
	return doc
}

// change is what differs between the JSON of old and new, each field that
// does named by its dotted path with its old and new values, nil when
// nothing does; a nil side, a record that does not exist, has every field
// null.
func change(old, new any) ([]byte, error) {
	o, err := asJSON(old)
	if err != nil {
		return nil, err
	}
	n, err := asJSON(new)
	if err != nil {
		return nil, err
	}
	out := map[string][2]any{}
	diff("", o, n, out)
	if len(out) == 0 {
		return nil, nil
	}
	return json.Marshal(out)
}

func asJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return decodeAny(b)
}

func diff(path string, o, n any, out map[string][2]any) {
	om, oObj := o.(map[string]any)
	nm, nObj := n.(map[string]any)
	if o == nil && nObj {
		om, oObj = map[string]any{}, true
	}
	if n == nil && oObj {
		nm, nObj = map[string]any{}, true
	}
	if !oObj || !nObj {
		if !reflect.DeepEqual(o, n) {
			out[path] = [2]any{o, n}
		}
		return
	}
	at := func(k string) string {
		if path == "" {
			return k
		}
		return path + "." + k
	}
	for k, v := range om {
		diff(at(k), v, nm[k], out)
	}
	for k, v := range nm {
		if _, ok := om[k]; !ok {
			diff(at(k), nil, v, out)
		}
	}
}
