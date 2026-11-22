package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// maxAuditPath bounds the path an audit record copies, in bytes, well past
// any path and query a row takes.
const maxAuditPath = 512

// auditOf is the audit record of r as it arrived, before its target and
// outcome are known: the caller ctx carries, by principal, name and
// credential, the node and the client's address, and the request's method,
// path with its query, request ID and idempotency key, never its body. The
// query is kept because a row's parameters can change what its write does,
// as a DELETE's disposition does. The path and query are cut to
// maxAuditPath bytes and a key over maxKey, refused for its length, is left
// out, so a caller cannot grow the audit by what it sends.
func (s *Server) auditOf(ctx context.Context, r *http.Request) entity.AuditRecord {
	c, _ := auth.CallerFrom(ctx)
	path, key := r.URL.RequestURI(), r.Header.Get("Idempotency-Key")
	if len(path) > maxAuditPath {
		path = path[:maxAuditPath]
	}
	if len(key) > maxKey {
		key = ""
	}
	req, _ := json.Marshal(struct { // strings always encode
		Method         string `json:"method"`
		Path           string `json:"path"`
		RequestID      string `json:"request_id"`
		IdempotencyKey string `json:"idempotency_key,omitempty"`
	}{r.Method, path, requestID(ctx), key})
	return entity.AuditRecord{Actor: c.Principal, ActorName: c.Name, Credential: c.Credential,
		Node: s.d.Node, Source: r.RemoteAddr, Request: req}
}

// refuse counts a refused request by its precondition, when one refused it,
// or as a refused authentication when its caller's role or a password it
// must change did, and audits it in a Write of its own holding only its
// audit record, so no idempotency record is kept and a corrected retry under
// the same key is a new request. A failure to write the record is logged and
// does not change the answer; with no store there is nowhere to write it.
func (s *Server) refuse(ctx context.Context, rec entity.AuditRecord, err error) {
	p, _ := problemOf(err)
	reason := ""
	switch {
	case p.Code == apiv1.CodeGenerationMismatch:
		s.preconditions.WithLabelValues("generation").Inc()
	case p.Code == apiv1.CodeIdempotencyMismatch:
		s.preconditions.WithLabelValues("idempotency").Inc()
	case p.Code == apiv1.CodePreconditionRequired:
		s.preconditions.WithLabelValues("missing").Inc()
	case p.Code == apiv1.CodeForbidden || errors.Is(err, errHidden):
		reason = "role"
	case p.Code == apiv1.CodePasswordChangeRequired:
		reason = "must_change"
	}
	if reason != "" && s.d.Credentials != nil {
		_ = s.d.Credentials.Refused(reason) // its answer is the refusal err already is
	}
	if s.d.Store == nil {
		return
	}
	rec.Outcome = []byte("refused")
	if p.Code != "" {
		rec.Outcome = []byte("refused: " + string(p.Code))
	}
	if werr := s.d.Store.Write(ctx, entity.WriteMeta{Audit: &rec}, func(entity.ControlPlaneTx) error { return nil }); werr != nil {
		s.d.Logger.Error("node api: a refused request's audit record was not written", "request_id", requestID(ctx), "err", werr)
		return
	}
	s.audits.WithLabelValues("refused").Inc()
}
