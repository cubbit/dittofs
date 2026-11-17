package api

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// maxKey bounds an Idempotency-Key, in bytes.
const maxKey = 255

// writing is what a write row's adapter knows of the resource it writes:
// its kind; read, which returns the record id names, tracked in the write's
// transaction, as the body a PUT of it sends, with its tag, ok false when
// there is none; whether its commit raises the principals or settings
// generation; whether the row is a POST action that changes the spec, and so
// carries If-Match as a PUT does; secret, which names the fields of the
// body whose values its audit record holds only as "set", nil for none; and
// op, the kind of operation the write starts on the record it wrote, empty
// for a write that starts none.
type writing[B any] struct {
	kind        entity.ResourceKind
	op          apiv1.OpKind
	read        func(tx entity.ControlPlaneTx, id [16]byte) (doc B, tag uint64, ok bool, err error)
	raises      bool
	changesSpec bool
	secret      func(path string) bool
}

// write adapts a write. Its checks run in this order: the body is decoded;
// the request's idempotency key is looked up, a recorded one answering its
// replay or 422; the headers its method requires are required, 428, or
// refused when they contradict each other, 400; prepare runs; then, inside
// the store's Write, the key is looked up again, the record's tag is
// compared, read tracked, 412, and h runs. So a key recorded for another
// request answers 422 before a missing If-Match answers 428, a replay or a
// refused key never runs prepare, and two writes naming one tag cannot both
// commit. prepare, when not nil, runs once before Write, never on a conflict
// retry, and its result is h's in; for a PATCH it sees the patch, decoded as
// the body. A PATCH body is a merge patch of the document read returns, and
// the result is decoded as strictly as a PUT's body, so a patch can set
// nothing a PUT could not. h returns its answer's body and the ID of the
// record it wrote: the path's, or, for a POST on a collection, the one it
// created.
//
// decision: prepare runs before the tag is compared, so a write with a stale
// tag runs it and is then refused 412. The tag is read inside Write's
// transaction, tracked, because only there does a match hold until the
// commit; a second read before prepare would cost every write a read to
// spare a step that a stale write wastes. Overturned if a step is costly
// enough, or has an effect outside the write, that running it for a stale
// write matters.
//
// The answer, 200 with what h returns and the record's tag as its ETag, or
// 201 with a Location naming the record for a POST on a collection, or, for
// a row that starts an operation, 202 with the operation's body and a
// Location naming it, is made
// inside the transaction and recorded under the key, so a replay answers
// exactly what was first sent and changes nothing. The audit record commits
// with the change; a refused request records no key and is audited in a
// transaction of its own. After a commit that raises the principals or
// settings generation, the answer waits for the node to apply it.
func write[B, I, R any](w writing[B], prepare func(context.Context, request[B]) (I, error), h func(context.Context, request[B], I, entity.ControlPlaneTx) (R, [16]byte, error)) adapter {
	return func(ctx context.Context, s *Server, r *http.Request, id [16]byte) (a answer, err error) {
		if s.d.Store == nil {
			return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no control-plane store"}
		}
		created := r.PathValue("id") == ""
		rec := s.auditOf(ctx, r)
		rec.Target.Kind = w.kind
		if !created {
			rec.Target.ID = id[:]
		}
		refused := rec // as the request arrived, holding nothing a failed attempt set
		defer func() {
			if err != nil {
				s.refuse(ctx, refused, err)
			}
		}()
		body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
		if err != nil {
			return answer{}, &fieldError{detail: err.Error()}
		}
		key := r.Header.Get("Idempotency-Key")
		if len(key) > maxKey {
			return answer{}, &fieldError{field: "Idempotency-Key", detail: "longer than " + strconv.Itoa(maxKey) + " bytes"}
		}
		rq := request[B]{ID: id}
		if err := decode(bytes.NewReader(body), &rq.Body); err != nil {
			return answer{}, err
		}
		if r.Method == http.MethodPatch {
			// An empty body or a null decodes as a body setting nothing,
			// so the patch's own shape is checked here, before its key.
			if _, err := mergePatch([]byte("{}"), body); err != nil {
				return answer{}, err
			}
		}
		var idem *entity.IdempotencyRecord
		if key != "" {
			if s.d.SigningKey == nil {
				return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no signing key to key an idempotency digest"}
			}
			signing, err := s.d.SigningKey(ctx)
			if err != nil {
				return answer{}, err
			}
			sum, err := digest(signing, r, body)
			if err != nil {
				return answer{}, err
			}
			c, _ := auth.CallerFrom(ctx)
			idem = &entity.IdempotencyRecord{Account: c.Principal, Key: key, Digest: sum}
			if err := s.d.Store.Replay(ctx, *idem); err != nil {
				return s.replayed(err)
			}
		}
		cond, err := conditionOf(r, key, w.changesSpec)
		if err != nil {
			return answer{}, err
		}
		// decision: the pre-write step's answer is an input to the
		// transaction, not a call from inside it. Inside, a conflict retry
		// would repeat a directory query or a credential probe for every
		// attempt, and the transaction would hold its reads open across a
		// network round trip. The cost is that the answer can be stale by
		// the commit, so h re-reads in the transaction the record the step
		// depended on. Overturned if a step's answer must be fresher than
		// one request.
		var in I
		if prepare != nil {
			if in, err = prepare(ctx, rq); err != nil {
				return answer{}, err
			}
		}
		err = s.d.Store.Write(ctx, entity.WriteMeta{Audit: &rec, Idempotency: idem}, func(tx entity.ControlPlaneTx) error {
			rq := rq
			doc, tag, ok, err := w.read(tx, id)
			if err != nil {
				return err
			}
			if err := cond.check(tag, ok); err != nil {
				return err
			}
			rq.Expect = tag
			if r.Method == http.MethodPatch {
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
			resp, wrote, err := h(ctx, rq, in, tx)
			if err != nil {
				return err
			}
			after, afterTag, exists, err := w.read(tx, wrote)
			if err != nil {
				return err
			}
			if rec.Change, err = change(present(doc, ok), present(after, exists), w.secret); err != nil {
				return err
			}
			rec.Target.ID = wrote[:]
			rec.Before, rec.After, rec.Outcome = tag, afterTag, []byte("applied")
			status, body, location := http.StatusOK, any(resp), ""
			if created {
				status, location = http.StatusCreated, r.URL.Path+"/"+idString(wrote)
			}
			if w.op != "" {
				op, err := s.start(ctx, tx, w.op, entity.ResourceRef{Kind: w.kind, ID: wrote[:]})
				if err != nil {
					return err
				}
				status, body, location = http.StatusAccepted, op, "/api/v1/operations/"+op.ID
				rec.Outcome = []byte(op.ID)
			}
			if a, err = jsonAnswer(status, body); err != nil {
				return err
			}
			if exists {
				a.header.Set("ETag", etag(afterTag))
			}
			if location != "" {
				a.header.Set("Location", location)
			}
			if idem != nil {
				idem.Answer, err = json.Marshal(recorded{a.status, a.header, a.body})
			}
			return err
		})
		if err != nil {
			return s.replayed(err)
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
			// node regularly outlasts the second. The second is on the
			// API's clock, and on the wall clock's with none.
			wctx, cancel := context.WithTimeout(ctx, time.Second)
			if s.d.Clock != nil {
				cancel()
				wctx, cancel = context.WithCancel(ctx)
				go func() {
					select {
					case <-s.d.Clock.After(time.Second):
						cancel()
					case <-wctx.Done():
					}
				}()
			}
			if werr := s.d.Applied(wctx); werr != nil {
				s.d.Logger.Debug("node api: a committed write is not yet applied on this node", "request_id", requestID(ctx), "err", werr)
			}
			cancel()
		}
		return a, nil
	}
}

// start writes, in tx, an operation of kind on target, started by the
// caller ctx carries and driven by no one yet, and returns it as its read
// answers it. A second one of kind on target while one runs is refused 409
// operation_running naming it.
func (s *Server) start(ctx context.Context, tx entity.ControlPlaneTx, kind apiv1.OpKind, target entity.ResourceRef) (apiv1.Operation, error) {
	c, _ := auth.CallerFrom(ctx)
	op := entity.Operation{ID: entity.OperationID(entity.NewShareID()), Kind: string(kind), Target: target, Account: c.Principal, State: entity.OpPending}
	gen, err := tx.PutOperation(op, 0)
	if err != nil {
		return apiv1.Operation{}, err
	}
	op.Generation, op.Started, op.Updated = gen, tx.Now(), tx.Now()
	v, err := viewOp(ctx, s.d, op)
	return v.Operation, err
}

// replayed is the answer err recorded when it is a replay of a request
// answered before, and err itself otherwise.
func (s *Server) replayed(err error) (answer, error) {
	var replay *entity.IdempotentReplay
	if !errors.As(err, &replay) {
		return answer{}, err
	}
	s.replays.Inc()
	var rc recorded
	if err := json.Unmarshal(replay.Answer, &rc); err != nil {
		return answer{}, err
	}
	return answer{status: rc.Status, header: rc.Header, body: rc.Body}, nil
}

// condition is what a write requires of its record's tag: none, that no
// record exists (a PUT with If-None-Match: *), or that the record's tag is
// match.
type condition struct {
	create bool
	match  string
}

// conditionOf is the condition r's headers name, refusing a request lacking
// a header its method requires, 428: a POST or a DELETE its Idempotency-Key,
// and a PUT, a PATCH, a DELETE or a POST action that changes the spec its
// If-Match, which a PUT may replace by If-None-Match: *. A request carrying
// both, or an If-Match of * or of a list, is refused 400.
//
// decision: If-Match is required, not optional. Optional, every script
// omits it, and a lost update is the default behaviour; the CLI reads before
// it writes, so the cost falls on nobody who uses it. Overturned if a client
// class appears that cannot read first, which would get a named
// unconditional action, not a missing header. If-Match names one tag: *
// would make the write unconditional again, and a list would let a client
// that has read no single record write over any of several, so both are
// refused rather than honoured.
func conditionOf(r *http.Request, key string, changesSpec bool) (condition, error) {
	m := r.Method
	if key == "" && (m == http.MethodPost || m == http.MethodDelete) {
		return condition{}, &refusal{code: apiv1.CodePreconditionRequired, detail: "a " + m + " carries an Idempotency-Key"}
	}
	// A header sent on several lines is one list, as HTTP joins them.
	match := strings.Join(r.Header.Values("If-Match"), ", ")
	none := strings.Join(r.Header.Values("If-None-Match"), ", ")
	switch {
	case match != "" && none != "":
		return condition{}, &fieldError{field: "If-Match", detail: "a request carries If-Match or If-None-Match, not both"}
	case match == "*" || strings.Contains(match, ","):
		return condition{}, &fieldError{field: "If-Match", detail: "If-Match names exactly one tag, never * or a list"}
	case m == http.MethodPost && !changesSpec:
		return condition{}, nil
	case m == http.MethodPut && none == "*":
		return condition{create: true}, nil
	case match == "":
		return condition{}, &refusal{code: apiv1.CodePreconditionRequired, detail: "a " + m + " carries If-Match, or a PUT creating its record If-None-Match: *"}
	}
	return condition{match: match}, nil
}

// check refuses, 412 naming the record's tag when there is one, a write
// whose condition the record does not meet: one creating its record finds
// one, or one naming a tag finds none or another. A tag is compared only for
// equality.
func (c condition) check(tag uint64, ok bool) error {
	switch {
	case c.create && ok:
		return mismatch(tag)
	case c.match == "" || c.create:
		return nil
	case !ok:
		return &refusal{code: apiv1.CodeGenerationMismatch, detail: "no record holds the tag If-Match names"}
	case c.match != etag(tag):
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

// digestInfo is the HKDF info string the digest's key is derived under.
const digestInfo = "dittofs idempotency digest v1"

// digest is a request's identity under its idempotency key: an HMAC-SHA256
// of its method, its path with its query, and its body, keyed by the
// HKDF-SHA256 derivation of signing under digestInfo.
//
// decision: the digest is keyed, never a plain hash of the body, because the
// body can carry a secret before the pre-write step seals it, and a plain
// hash in the store lets whoever holds a dump test guesses at a low-entropy
// one offline. The key comes from the installation's signing key, sealed
// outside the store and the same on every node, so every node computes the
// same digest. It is derived per request from the version that signs now,
// since the signing key rotates: a retry of a request first answered before
// a rotation computes another digest and is answered 422, never applied
// twice. Overturned if a client must replay across a rotation; then each
// signing key version is kept for an idempotency record's life, and a
// mismatch is checked again under the previous one.
func digest(signing []byte, r *http.Request, body []byte) ([32]byte, error) {
	k, err := hkdf.Key(sha256.New, signing, nil, digestInfo, sha256.Size)
	if err != nil {
		return [32]byte{}, err
	}
	h := hmac.New(sha256.New, k)
	h.Write([]byte(r.Method + " " + r.URL.RequestURI() + "\n"))
	h.Write(body)
	return [32]byte(h.Sum(nil)), nil
}

// recorded is an answer as an idempotency record holds it.
type recorded struct {
	Status int         `json:"status"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body"`
}

// present is doc when its record exists, and nil when it does not.
func present(doc any, ok bool) any {
	if !ok {
		return nil
	}
	return doc
}

// change is what differs between the JSON of old and new, each field that
// does named by its dotted path with its old and new values, or with "set"
// when secret names the path, nil when nothing does; a nil side, a record
// that does not exist, has every field null.
func change(old, new any, secret func(path string) bool) ([]byte, error) {
	o, err := asJSON(old)
	if err != nil {
		return nil, err
	}
	n, err := asJSON(new)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	diff("", o, n, secret, out)
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

func diff(path string, o, n any, secret func(string) bool, out map[string]any) {
	if path != "" && secret != nil && secret(path) {
		if !reflect.DeepEqual(o, n) {
			out[path] = "set"
		}
		return
	}
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
		diff(at(k), v, nm[k], secret, out)
	}
	for k, v := range nm {
		if _, ok := om[k]; !ok {
			diff(at(k), nil, v, secret, out)
		}
	}
}
