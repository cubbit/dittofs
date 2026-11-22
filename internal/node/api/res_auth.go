package api

import (
	"context"
	"net/http"
	"reflect"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// signIns is sign-in, refresh and the password change, each auditing its
// own write from the request's record as it arrived, and saying what it
// audited; and a user's :set-password, sealed before its write and written
// in it.
type signIns interface {
	SignIn(ctx context.Context, rec entity.AuditRecord, in apiv1.SignIn) (apiv1.Tokens, auth.Audited, error)
	Refresh(ctx context.Context, rec entity.AuditRecord, in apiv1.Refresh) (apiv1.Tokens, auth.Audited, error)
	ChangePassword(ctx context.Context, rec entity.AuditRecord, in apiv1.PasswordChange) (apiv1.Tokens, auth.Audited, error)
	SealPassword(ctx context.Context, u entity.User, password []byte) (auth.PasswordSet, error)
	SetPassword(tx entity.ControlPlaneTx, ps auth.PasswordSet) error
}

// authRoutes are sign-in, refresh, the password change and sign-out.
// Sign-in and refresh are public, since they are how a caller gets a token,
// and answer the tokens; the password change is the caller's own, admitted
// while its password must change, and answers the tokens of the session it
// opens; sign-out ends the caller's own session.
var authRoutes = []route{
	{method: http.MethodPost, pattern: "/api/v1/auth/sign-in", right: auth.Public, serve: credential(signIns.SignIn, vfserr.ErrLogonFailure, auth.ErrThrottled)},
	{method: http.MethodPost, pattern: "/api/v1/auth/refresh", right: auth.Public, serve: credential(signIns.Refresh, vfserr.ErrLogonFailure)},
	{method: http.MethodPost, pattern: "/api/v1/auth/password", kind: entity.ResourceUser, right: auth.Self, mustChange: true,
		serve: credential(signIns.ChangePassword, vfserr.ErrLogonFailure, auth.ErrThrottled, vfserr.ErrInvalid)},
	// Sign-out takes no key: the idempotency record is keyed by account, so
	// a key one session used would answer another session's sign-out from
	// the first one's record and leave it live; a retry of one that applied
	// is refused 401, its session gone.
	{method: http.MethodPost, pattern: "/api/v1/auth/sign-out", kind: entity.ResourceSession, right: auth.Self, mustChange: true, keyless: true, ownRecord: true, serve: write(sessions, nil, signOut)},
}

// credential adapts sign-in, refresh or the password change: a call
// answering the tokens h issues, or refused with one of refuses by the
// credential it carries; each audit record h wrote is counted by its
// outcome.
//
// decision: credential calls keep no idempotency record (RFC 23 §4.5),
// because replaying their answer means storing a usable credential for the
// record's 24 h, which RFC 23 §5.3's hash-only rule forbids: a retried
// sign-in opens a second session, and a retried refresh presents the token
// the lost answer rotated, which revokes the session. Revisit only if a
// client class appears that cannot sign in again after a lost refresh, and
// then give refresh a short reuse grace, not a stored answer.
func credential[B any](h func(signIns, context.Context, entity.AuditRecord, B) (apiv1.Tokens, auth.Audited, error), refuses ...error) adapter {
	return adapter{form: formCall, body: reflect.TypeFor[B](), out: reflect.TypeFor[apiv1.Tokens](), refuses: refuses,
		run: func(ctx context.Context, s *Server, r *http.Request, _ *route, id [16]byte) (answer, error) {
			rq, err := decodeRequest[B](r, id)
			if err != nil {
				return answer{}, err
			}
			if s.d.Credentials == nil {
				return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node signs no caller in"}
			}
			t, audited, err := h(s.d.Credentials, ctx, s.auditOf(ctx, r), rq.Body)
			if audited != auth.AuditedNothing {
				s.audits.WithLabelValues(string(audited)).Inc()
			}
			return jsonOr(t, err)
		}}
}

// sessions is the caller's own session, which sign-out deletes: its path
// names no ID, so it is the one the caller's access token names, the zero ID
// for a caller that presented none, whose sign-out deletes nothing.
var sessions = writing[none]{kind: entity.ResourceSession,
	self: func(ctx context.Context, _ Deps) ([16]byte, error) {
		c, _ := auth.CallerFrom(ctx)
		id, _ := c.Session()
		return id, nil
	},
	read: func(tx entity.ControlPlaneTx, id [16]byte, _ string) (none, uint64, bool, error) {
		_, ok, err := tx.Session(id)
		return none{}, 0, ok, err
	}}

// signOut deletes the caller's session, so neither its access tokens nor its
// refresh token are accepted again.
func signOut(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	return none{}, rq.ID, tx.DeleteSession(rq.ID)
}
