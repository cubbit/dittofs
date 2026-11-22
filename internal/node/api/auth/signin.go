package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// sourceIn is the source an attempt from the request rec records is counted
// on: none on the local socket, else its peer address's.
func sourceIn(ctx context.Context, rec entity.AuditRecord) (netip.Prefix, error) {
	if Local(ctx) {
		return netip.Prefix{}, nil
	}
	ap, err := netip.ParseAddrPort(rec.Source)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("auth: the peer address %q: %w", rec.Source, err)
	}
	return sourceOf(ap.Addr(), false), nil
}

// SignIn checks in's name and password and opens a session: the attempt is
// counted on the principal Lookup resolves the name to, and on the source rec
// names, before the password is checked; then one write settles it with its
// audit record, rec completed, holding the session on a success. It answers
// the tokens and what it audited. A wrong password, an unknown name and a
// disabled or deleted account are refused alike, ErrLogonFailure, and so is
// an account disabled, deleted or changed between the check and the write,
// which that write reads again. A store that cannot check the password is no
// failure: the attempt's raise of its source is taken back, nothing is
// audited, and ErrUnavailable is returned.
//
// decision: an attempt is audited under the principal its name resolves to,
// the zero principal for a name that resolves to none, and never under the
// name as presented, which could be a password typed in the wrong field. So
// a failure for an unknown name names no actor, as the schedule's records
// do. Overturned if the audit must tell the presented names apart.
//
// ponytail: a checked sign-in commits twice, the count before the check and
// its settlement after, so one source's checked sign-ins are bounded by the
// store's commit rate, each paying two. Upgrade to counts cached per node and
// settled in batches, which RFC 23 §5.3 would have to allow first, when the
// sign-in benchmark shows them costing the metadata commits beside them.
func (s *Service) SignIn(ctx context.Context, rec entity.AuditRecord, in apiv1.SignIn) (apiv1.Tokens, Audited, error) {
	t, audited, err := s.signIn(ctx, rec, in)
	s.signIns.WithLabelValues("password", resultOf(err)).Inc()
	return t, audited, err
}

func (s *Service) signIn(ctx context.Context, rec entity.AuditRecord, in apiv1.SignIn) (apiv1.Tokens, Audited, error) {
	password := []byte(in.Password)
	defer clear(password)
	src, err := sourceIn(ctx, rec)
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, err
	}
	// The signing key is read before anything commits, so a key that
	// cannot be read refuses the attempt with nothing counted or audited,
	// and no session is left behind an answer that failed.
	k, err := s.d.Tokens.current(ctx)
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	}
	p, err := s.d.Accounts.Lookup(ctx, in.Name)
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	}
	c, err := reserve(ctx, s.d.Store, s.d.Clock, attempt{account: p, source: src})
	if merrs.IsNotFoundError(err) {
		// The account was deleted since Lookup: the name counts as one
		// that resolves to none, on its source only.
		p = entity.Principal{}
		c, err = reserve(ctx, s.d.Store, s.d.Clock, attempt{source: src})
	}
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	}
	// The record the password is checked against, whose generation the
	// success's write requires unchanged.
	u, aerr := s.d.Accounts.Account(ctx, p)
	verr := s.d.Accounts.VerifyPassword(ctx, p, password)
	for _, err := range []error{aerr, verr} {
		if err != nil && !errors.Is(err, vfserr.ErrLogonFailure) {
			return apiv1.Tokens{}, AuditedNothing, unavailable(errors.Join(err, unreserve(ctx, s.d.Store, c)))
		}
	}
	rec.Actor = p
	rec.Target = entity.ResourceRef{Kind: entity.ResourceSession}
	failed := rec
	failed.Outcome = []byte("refused: " + apiv1.CodeUnauthenticated)
	if aerr != nil || verr != nil {
		if err := s.d.Store.Write(ctx, entity.WriteMeta{Audit: &failed}, func(tx entity.ControlPlaneTx) error {
			return settleFailure(tx, c)
		}); err != nil {
			return apiv1.Tokens{}, AuditedNothing, unavailable(err)
		}
		return apiv1.Tokens{}, AuditedRefused, vfserr.ErrLogonFailure
	}
	refresh, hash := newRefresh()
	var sess entity.Session
	_, _ = rand.Read(sess.ID[:]) // crypto/rand.Read never fails
	lifetime := s.d.Settings().SessionLifetime
	var now time.Time
	var mustChange bool
	// out is the record the write commits, rebuilt from rec on each attempt.
	var out entity.AuditRecord
	err = s.d.Store.Write(ctx, entity.WriteMeta{Audit: &out}, func(tx entity.ControlPlaneTx) error {
		cur, ok, err := tx.User(p)
		if err != nil {
			return err
		}
		if !ok || cur.Disabled || cur.Generation != u.Generation {
			out = failed
			return settleFailure(tx, c)
		}
		out = rec
		out.ActorName, out.Credential = cur.Name, hex.EncodeToString(sess.ID[:])
		out.Target.ID, out.Outcome = sess.ID[:], []byte("applied")
		if err := takeBackSuccess(tx, c); err != nil {
			return err
		}
		now, mustChange = tx.Now(), cur.MustChange
		sess = entity.Session{ID: sess.ID, Account: p, Refresh: hash, Created: issuedAt(now, cur.NotBefore),
			Expires: now.Add(lifetime)}
		return tx.PutSession(sess)
	})
	switch {
	case err != nil:
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	case out.Target.ID == nil:
		return apiv1.Tokens{}, AuditedRefused, vfserr.ErrLogonFailure
	}
	t, err := tokens(s.d.Tokens.signer(k), p, sess.ID, sess.Created, refresh, mustChange)
	return t, AuditedApplied, err
}

// unreserve takes back the raise of an attempt the store could not check, in
// a write of its own that records nothing: the attempt checked no password,
// so it is counted, not audited.
func unreserve(ctx context.Context, st Writer, c counted) error {
	return st.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return takeBackRaise(tx, c) })
}
