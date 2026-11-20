package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// ResolvePrincipalProgress is resolve-principal's progress: it runs in one
// turn, so there is none to read.
func (s *Server) ResolvePrincipalProgress(context.Context, entity.Operation) apiv1.Progress {
	return apiv1.Progress{}
}

// ResolvePrincipalStep runs a resolve-principal operation in one turn: it
// finds the principal op.Params names, by its qualified name or its SID,
// minting a directory principal no one holds yet, and answers the
// principal's ID. A spelling no principal holds, or one that does not parse,
// fails the operation with a *apiv1.Problem naming it; every other error,
// the directory's ErrUnavailable among them, is retried.
func (s *Server) ResolvePrincipalStep(ctx context.Context, op entity.Operation) (step.StepResult, error) {
	var params apiv1.ResolvePrincipalParams
	if err := json.Unmarshal(op.Params, &params); err != nil {
		return step.StepResult{}, resolveProblem(http.StatusBadRequest, fmt.Sprintf("the parameters do not decode: %v", err))
	}
	var sp identity.Spelling
	switch {
	case (params.Name == "") == (params.SID == ""):
		return step.StepResult{}, resolveProblem(http.StatusBadRequest, "exactly one of name and sid is required")
	case params.Name != "":
		sp = identity.Spelling{Scheme: identity.SchemeQualified, Text: params.Name}
	default:
		v, err := sid.ParseSIDString(params.SID)
		if err != nil {
			return step.StepResult{}, resolveProblem(http.StatusBadRequest, fmt.Sprintf("the SID %q does not parse: %v", params.SID, err))
		}
		sp = identity.Spelling{Scheme: identity.SchemeSID, SID: *v}
	}
	spelled := params.Name + params.SID
	p, err := s.Find(ctx, sp, identity.FindOpts{Mint: true})
	switch {
	case errors.Is(err, vfserr.ErrUnknownPrincipal):
		return step.StepResult{}, resolveProblem(http.StatusNotFound, fmt.Sprintf("no principal holds %s", spelled))
	case errors.Is(err, vfserr.ErrInvalid):
		return step.StepResult{}, resolveProblem(http.StatusBadRequest, fmt.Sprintf("%s: %v", spelled, err))
	case err != nil:
		return step.StepResult{}, err
	}
	res, err := json.Marshal(apiv1.Principal{Kind: p.Kind.String(), ID: p.ID.String()})
	return step.StepResult{Done: true, Result: res}, err
}

func resolveProblem(status int, detail string) *apiv1.Problem {
	return &apiv1.Problem{Status: status, Title: "the principal was not resolved", Detail: detail}
}

// TestDirectoryProgress is test-directory's progress: it runs in one turn,
// so there is none to read.
func (s *Server) TestDirectoryProgress(context.Context, entity.Operation) apiv1.Progress {
	return apiv1.Progress{}
}

// TestDirectoryResult is a completed test-directory's Result: whether this
// node reached the directory, and why not when it did not.
type TestDirectoryResult struct {
	Reached bool   `json:"reached"`
	Error   string `json:"error,omitempty"`
}

// TestDirectoryStep runs a test-directory operation in one turn: it binds to
// the directory the applied State names, on a connection of its own, reads
// the record's base entry and answers this node's result. A directory that
// does not answer is the result, not a failure; no directory configured fails
// the operation with a *apiv1.Problem.
//
// ponytail: "every node" is this node: the answer is the node the runner ran
// the step on. Answer each registered node's test with the cluster.
func (s *Server) TestDirectoryStep(ctx context.Context, _ entity.Operation) (step.StepResult, error) {
	st := s.state.Load()
	if st == nil {
		return step.StepResult{}, errNotApplied
	}
	if st.dir == nil {
		return step.StepResult{}, &apiv1.Problem{Status: http.StatusConflict, Title: "the directory was not tested", Detail: "no directory is configured on this node"}
	}
	var r TestDirectoryResult
	if _, err := st.dir.client.Test(ctx); err != nil {
		if ctx.Err() != nil {
			return step.StepResult{}, ctx.Err() // the turn was stopped, not answered: run it again
		}
		r.Error = err.Error()
	} else {
		r.Reached = true
	}
	res, err := json.Marshal(r)
	return step.StepResult{Done: true, Result: res}, err
}
