package api

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// maxKey bounds an Idempotency-Key, in bytes.
const maxKey = 255

// appliedWait bounds how long a write raising the principals or settings
// generation waits for the node to apply it.
const appliedWait = time.Second

// writing is what a write row's adapter knows of the resource it writes:
// its kind; read, which returns the record id and the path's {sub} name,
// tracked in the write's transaction, as the body a PUT of it sends, with
// its tag, ok false when there is none; whether its commit raises the principals or settings
// generation; whether the row is a POST action that changes the spec, and so
// carries If-Match as a PUT does; secret, which names the fields of the
// body whose values its audit record holds only as "set", nil for none; and
// auditAs, the name its audit record's change gives the body's spec, empty
// keeping spec, for a record written as part of another resource's.
// The operation a write starts, if any, is its row's.
type writing[B any] struct {
	kind entity.ResourceKind
	read func(tx entity.ControlPlaneTx, id [16]byte, sub string) (doc B, tag uint64, ok bool, err error)
	// hidden, when set, reports a record the caller ctx carries may not
	// know of, read tracked in tx: a write naming it is answered 404, ahead
	// of its precondition, as one naming no record would be.
	hidden      func(ctx context.Context, tx entity.ControlPlaneTx, id [16]byte) (bool, error)
	raises      bool
	changesSpec bool
	secret      func(path string) bool
	auditAs     string
	// self, when set, is the ID of the one record a path naming no ID
	// reaches, such as the installation's, which its audit records and its
	// handler's request name.
	self func(context.Context, Deps) ([16]byte, error)
}

// write adapts a write. Its checks run in this order: a query parameter its
// row does not take is refused, 400 naming it; the body is decoded;
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
// A pre-write step may hold a credential outside the store, under the ID
// of the operation the write starts, which its opStart's release drops when
// the write is refused or panics; and it may commit a write of its own
// before this one, through the request's own, audited as the request.
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
//
// An action's step, whose result is an opStart (see action), names the
// operation the row starts: its ID, its params and the release of what the
// step holds under it.
func write[B, I, R any](w writing[B], prepare func(context.Context, request[B]) (I, error), h func(context.Context, request[B], I, entity.ControlPlaneTx) (R, [16]byte, error)) adapter {
	return adapter{form: formWrite, body: reflect.TypeFor[B](), out: reflect.TypeFor[R](), changesSpec: w.changesSpec, replays: true, raises: w.raises, run: func(ctx context.Context, s *Server, r *http.Request, rt *route, id [16]byte) (a answer, err error) {
		if s.d.Store == nil {
			return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no control-plane store"}
		}
		created := rt.creates()
		sub := r.PathValue("sub")
		rec := s.auditOf(ctx, r)
		rec.Target.Kind = w.kind
		refused := rec // as the request arrived, holding nothing a failed attempt set
		defer func() {
			if err != nil {
				s.refuse(ctx, refused, err)
			}
		}()
		if w.self != nil {
			if id, err = w.self(ctx, s.d); err != nil {
				return answer{}, err
			}
		}
		if !created {
			rec.Target.ID = rt.ref(id, sub)
		}
		refused = rec
		for k := range r.URL.Query() {
			if !slices.Contains(rt.query, k) {
				return answer{}, &fieldError{field: k, detail: "not a parameter of this write"}
			}
		}
		body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
		if s.seen != nil {
			s.seen(body)
		}
		if rt.keyless || w.secret != nil {
			defer clear(body) // it holds a credential or a secret's value, on every path out
		}
		if err != nil {
			return answer{}, &fieldError{detail: err.Error()}
		}
		key := r.Header.Get("Idempotency-Key")
		switch {
		case len(key) > maxKey:
			return answer{}, &fieldError{field: "Idempotency-Key", detail: "longer than " + strconv.Itoa(maxKey) + " bytes"}
		case key != "" && rt.keyless:
			return answer{}, &fieldError{field: "Idempotency-Key", detail: "this call carries a credential, so it keeps no idempotency record and takes no key"}
		}
		rq := request[B]{ID: id, Sub: sub, Query: r.URL.Query()}
		prepared := false
		if rt.keyless || w.secret != nil {
			// The credential or secret the body decodes is prepare's from the
			// call on, which clears it; a refusal before it clears it here.
			defer func() {
				if !prepared {
					wipe(reflect.ValueOf(&rq.Body).Elem())
				}
			}()
		}
		rq.d = s.d
		rq.own = func(ctx context.Context, target entity.ResourceRef, change []byte, fn func(entity.ControlPlaneTx) error) error {
			a := refused
			a.Target, a.Change, a.Outcome = target, change, []byte("applied")
			if err := s.d.Store.Write(ctx, entity.WriteMeta{Audit: &a}, fn); err != nil {
				return err
			}
			s.audits.WithLabelValues("applied").Inc()
			return nil
		}
		err = decode(bytes.NewReader(body), &rq.Body)
		if s.seen != nil {
			s.seen(&rq.Body)
		}
		if rt.keyless {
			clear(body) // it holds a credential, which no digest reads on a keyless row
		}
		if err != nil {
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
		cond, err := conditionOf(r, key, rt.precondition())
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
		prepared = true
		if prepare != nil {
			if in, err = prepare(ctx, rq); err != nil {
				return answer{}, err
			}
		}
		st, _ := any(in).(opStart) // an action's step names the operation its row starts
		committed := false
		defer func() {
			if !committed && st.release != nil {
				st.release()
			}
		}()
		err = s.d.Store.Write(ctx, entity.WriteMeta{Audit: &rec, Idempotency: idem}, func(tx entity.ControlPlaneTx) error {
			rq := rq
			var started *apiv1.Operation
			rq.start = func(kind apiv1.OpKind, target entity.ResourceRef) error {
				if kind != rt.mayStart {
					return fmt.Errorf("api: %s starts no %s operation", rt.op(), kind)
				}
				op, err := start(ctx, tx, kind, target, opStart{})
				started = &op
				return err
			}
			if w.hidden != nil && !created {
				if hide, err := w.hidden(ctx, tx, id); err != nil || hide {
					return cmp.Or(err, errNotFound)
				}
			}
			doc, tag, ok, err := w.read(tx, id, sub)
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
			after, afterTag, exists, err := w.read(tx, wrote, sub)
			if err != nil {
				return err
			}
			if rec.Change, err = change(present(doc, ok), present(after, exists), w.auditAs, w.secret); err != nil {
				return err
			}
			rec.Target.ID = rt.ref(wrote, sub)
			rec.Before, rec.After, rec.Outcome = tag, afterTag, []byte("applied")
			status, body, location := http.StatusOK, any(resp), ""
			if created {
				status, location = http.StatusCreated, r.URL.Path+"/"+idString(wrote)
			}
			if rt.starts != "" {
				op, err := start(ctx, tx, rt.starts, entity.ResourceRef{Kind: w.kind, ID: wrote[:]}, st)
				if err != nil {
					return err
				}
				started = &op
			}
			if started != nil {
				status, body, location = http.StatusAccepted, *started, "/api/v1/operations/"+started.ID
				rec.Outcome = []byte(started.ID)
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
		committed = true
		s.audits.WithLabelValues("applied").Inc()
		if idem != nil {
			s.idempotencyWritten.Inc()
		}
		if w.raises {
			s.awaitApplied(ctx)
		}
		return a, nil
	}}
}

// awaitApplied waits for the node to apply what a write raising the
// principals or settings generation committed, for at most a second on the
// API's clock, the wall clock's with none. The second is armed before
// Applied is called, so a caller that sees Applied reached knows it runs.
//
// decision: the answer waits at most appliedWait, one second, the bound a reconcile pass is held
// to, and the write answers the commit's own result whatever the wait
// returns: the write is durable, and status.applied says where it is
// applied. Overturned if a pass on a healthy node regularly outlasts the
// second.
func (s *Server) awaitApplied(ctx context.Context) {
	if s.d.Applied == nil {
		return
	}
	var cancel context.CancelFunc
	if s.d.Clock == nil {
		ctx, cancel = context.WithTimeout(ctx, appliedWait)
	} else {
		bound := s.d.Clock.After(appliedWait)
		ctx, cancel = context.WithCancel(ctx)
		go func() {
			select {
			case <-bound:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	defer cancel()
	if err := s.d.Applied(ctx); err != nil {
		s.d.Logger.Debug("node api: a committed write is not yet applied on this node", "request_id", requestID(ctx), "err", err)
	}
}

// start writes, in tx, an operation of kind on target, started by the
// caller ctx carries and driven by no one yet, under the ID and with the
// params st names, a fresh ID when it names none, and returns it as its read
// answers it, its progress unknown, as it is before any step records one. A
// second one of kind on target while one runs is refused 409
// operation_running naming it.
func start(ctx context.Context, tx entity.ControlPlaneTx, kind apiv1.OpKind, target entity.ResourceRef, st opStart) (apiv1.Operation, error) {
	c, _ := auth.CallerFrom(ctx)
	id := st.id
	if id == (entity.OperationID{}) {
		id = entity.OperationID(entity.NewShareID())
	}
	op := entity.Operation{ID: id, Kind: string(kind), Target: target, Account: c.Principal, State: entity.OpPending, Params: st.params}
	gen, err := tx.PutOperation(op, 0)
	if err != nil {
		return apiv1.Operation{}, err
	}
	op.Generation, op.Started, op.Updated = gen, tx.Now(), tx.Now()
	return opOf(op).Operation, nil
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
// a header its row's precondition p requires, 428: its Idempotency-Key, and
// its If-Match, which a PUT that may create its record may replace by
// If-None-Match: *. A request carrying both, or an If-Match of * or of a
// list, is refused 400.
//
// decision: If-Match is required, not optional. Optional, every script
// omits it, and a lost update is the default behaviour; the CLI reads before
// it writes, so the cost falls on nobody who uses it. Overturned if a client
// class appears that cannot read first, which would get a named
// unconditional action, not a missing header. If-Match names one tag: *
// would make the write unconditional again, and a list would let a client
// that has read no single record write over any of several, so both are
// refused rather than honoured.
func conditionOf(r *http.Request, key string, p precondition) (condition, error) {
	m := r.Method
	if key == "" && p.key {
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
	case !p.compares():
		return condition{}, nil
	case p.create && none == "*":
		return condition{create: true}, nil
	case match == "" && p.create:
		return condition{}, &refusal{code: apiv1.CodePreconditionRequired, detail: "a " + m + " carries If-Match, or a PUT creating its record If-None-Match: *"}
	case match == "":
		return condition{}, &refusal{code: apiv1.CodePreconditionRequired, detail: "a " + m + " carries If-Match"}
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
// that does not exist, has every field null. A spec is named as, when as is
// not empty.
func change(old, new any, as string, secret func(path string) bool) ([]byte, error) {
	o, err := asJSON(old)
	if err != nil {
		return nil, err
	}
	n, err := asJSON(new)
	if err != nil {
		return nil, err
	}
	for _, side := range []any{o, n} {
		if m, ok := side.(map[string]any); ok && as != "" {
			m[as] = m["spec"]
			delete(m, "spec")
		}
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

// wipe zeroes every byte slice v holds, through its structs and pointers.
func wipe(v reflect.Value) {
	switch v.Kind() {
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			clear(v.Bytes())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			wipe(v.Field(i))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			wipe(v.Elem())
		}
	}
}
