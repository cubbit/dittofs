package api

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// refuse counts a refused write by its precondition, when one refused it,
// and audits it in a Write of its own holding only its audit record, so no
// idempotency record is kept and a corrected retry under the same key is a
// new request. A failure to write the record is logged and does not change
// the answer.
func (s *Server) refuse(ctx context.Context, rec entity.AuditRecord, err error) {
	p, _ := problemOf(err)
	switch p.Code {
	case apiv1.CodeGenerationMismatch:
		s.preconditions.WithLabelValues("generation").Inc()
	case apiv1.CodeIdempotencyMismatch:
		s.preconditions.WithLabelValues("idempotency").Inc()
	case apiv1.CodePreconditionRequired:
		s.preconditions.WithLabelValues("missing").Inc()
	}
	if s.d.Store == nil {
		return
	}
	rec.Outcome = []byte("refused")
	if p.Code != "" {
		rec.Outcome = []byte("refused: " + string(p.Code))
	}
	if werr := s.d.Store.Write(ctx, entity.WriteMeta{Audit: &rec}, func(entity.ControlPlaneTx) error { return nil }); werr != nil {
		s.d.Logger.Error("node api: a refused write's audit record was not written", "request_id", requestID(ctx), "err", werr)
		return
	}
	s.audits.WithLabelValues("refused").Inc()
}
