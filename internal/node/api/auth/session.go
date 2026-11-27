package auth

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Refresh rotates the session in's refresh token names: it answers new
// tokens and what it audited, and the one presented refreshes no more. A
// token the session has already replaced revokes the whole session, and is
// refused. A session expired, revoked, of an account disabled or deleted, or
// created before the account's not-before is refused, as is a token no
// session holds; each ErrLogonFailure. The account is read through Accounts
// as the per-call check reads it, so a store that cannot answer is
// ErrUnavailable, never a refusal; and read again in the write, so one
// disabled or deleted since is refused there.
//
// Each refusal is counted as a refused authentication, a reused token under
// its own reason; a store that cannot answer is not.
func (s *Service) Refresh(ctx context.Context, rec entity.AuditRecord, in apiv1.Refresh) (apiv1.Tokens, Audited, error) {
	t, audited, err := s.refresh(ctx, rec, in)
	if errors.Is(err, vfserr.ErrLogonFailure) {
		reason := "refresh"
		if audited == AuditedRefused { // only a reused token's refusal is audited
			reason = "refresh_reused"
		}
		err = s.Refused(reason)
	}
	return t, audited, err
}

func (s *Service) refresh(ctx context.Context, rec entity.AuditRecord, in apiv1.Refresh) (apiv1.Tokens, Audited, error) {
	hash, ok := refreshHash(in.Token)
	if !ok {
		return apiv1.Tokens{}, AuditedNothing, vfserr.ErrLogonFailure
	}
	// The signing key is read before the token rotates, so a key that
	// cannot be read refuses the refresh with the presented token still
	// current, which the client's retry presents again.
	k, err := s.d.Tokens.current(ctx)
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	}
	sess, found, err := s.d.Store.SessionByRefresh(ctx, hash)
	if err != nil || !found {
		return apiv1.Tokens{}, AuditedNothing, unavailable(cmp.Or(err, vfserr.ErrLogonFailure))
	}
	u, err := s.d.Accounts.Account(ctx, sess.Account)
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	}
	refresh, next := newRefresh()
	rec.Actor, rec.ActorName, rec.Credential = sess.Account, u.Name, hex.EncodeToString(sess.ID[:])
	rec.Target = entity.ResourceRef{Kind: entity.ResourceSession, ID: sess.ID[:]}
	var iat time.Time
	var mustChange, reused bool
	err = s.d.Store.Write(ctx, entity.WriteMeta{Audit: &rec}, func(tx entity.ControlPlaneTx) error {
		cur, found, err := tx.SessionByRefresh(hash)
		if err != nil || !found {
			return cmp.Or(err, vfserr.ErrLogonFailure)
		}
		if reused = cur.Refresh != hash; reused {
			rec.Outcome = []byte("revoked: a replaced refresh token was presented")
			return tx.DeleteSession(cur.ID)
		}
		u, ok, err := tx.User(cur.Account)
		now := tx.Now()
		switch {
		case err != nil:
			return err
		case !ok || u.Disabled || cur.Revoked || !now.Before(cur.Expires) || before(cur.Created, u.NotBefore):
			return vfserr.ErrLogonFailure
		}
		cur.Refresh = next
		iat, mustChange = issuedAt(now, u.NotBefore), u.MustChange
		rec.Outcome = []byte("applied")
		return tx.UpdateSession(cur)
	})
	switch {
	case err != nil:
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	case reused:
		return apiv1.Tokens{}, AuditedRefused, vfserr.ErrLogonFailure
	}
	t, err := tokens(s.d.Tokens.signer(k), sess.Account, sess.ID, iat, refresh, mustChange)
	return t, AuditedApplied, err
}

// credential is what a presented credential resolves to before its account
// is admitted: the principal it names, its kind and ID, and its issue time,
// which the account's not-before is checked against.
type credential struct {
	principal entity.Principal
	kind      CredentialKind
	id        [16]byte
	issued    time.Time
}

// Authenticate answers who r is: the credential its bearer token presents,
// an access token or an API token, resolved, then the account it names admitted as its record is now. Each
// refusal is ErrLogonFailure, counted by its reason; a store or a signing
// key that cannot be read answers ErrUnavailable.
func (s *Service) Authenticate(r *http.Request) (Caller, error) {
	ctx := r.Context()
	// The scheme's name is case-insensitive (RFC 7235 §2.1).
	h := r.Header.Get("Authorization")
	const scheme = "Bearer "
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return Caller{}, s.Refused("missing")
	}
	// An access token is a JWT, whose parts are joined by dots; an API
	// token is base64url, which holds none.
	raw, resolve := h[len(scheme):], s.session
	if !strings.Contains(raw, ".") {
		resolve = s.apiToken
	}
	cred, err := resolve(ctx, raw)
	if err != nil {
		return Caller{}, err
	}
	return s.admit(ctx, cred)
}

// session resolves raw as an access token: refused when its signature or
// expiry does not verify, and when its session is gone, revoked, expired or
// another account's.
//
// decision: a session's expiry is written at store time and compared here
// with the node's clock, since a read outside a write has no store time. The
// two part by at most the store's ceiling, about 2 s ahead of a node's
// clock, so a session can be refused up to that much early, or accepted that
// much late after the clock steps back. Withdraw it when the store offers a
// Now readable outside a write.
func (s *Service) session(ctx context.Context, raw string) (credential, error) {
	cl, err := s.d.Tokens.Verify(ctx, raw)
	switch {
	case errors.Is(err, errToken):
		return credential{}, s.Refused("token")
	case err != nil:
		return credential{}, unavailable(err)
	}
	sess, found, err := s.d.Store.Session(ctx, cl.Session)
	switch {
	case err != nil:
		return credential{}, unavailable(err)
	case !found || sess.Revoked || sess.Account != cl.Principal || !s.d.Clock.Now().Before(sess.Expires):
		return credential{}, s.Refused("session")
	}
	// An access token's issue time is never before its session's creation,
	// both raised to the account's not-before, so it alone is checked.
	return credential{principal: cl.Principal, kind: CredentialSession, id: sess.ID, issued: cl.IssuedAt}, nil
}

// admit is the caller cred names: its account as the record is now, refused
// when disabled or deleted, or when cred was issued before its not-before.
func (s *Service) admit(ctx context.Context, cred credential) (Caller, error) {
	u, err := s.d.Accounts.Account(ctx, cred.principal)
	switch {
	case errors.Is(err, vfserr.ErrLogonFailure):
		return Caller{}, s.Refused("account")
	case err != nil:
		return Caller{}, unavailable(err)
	case before(cred.issued, u.NotBefore):
		return Caller{}, s.Refused("not_before")
	}
	return Caller{Principal: u.Principal, Name: u.Name, Kind: cred.kind, ID: cred.id, Credential: hex.EncodeToString(cred.id[:]),
		Role: u.Role, MustChange: u.MustChange}, nil
}
