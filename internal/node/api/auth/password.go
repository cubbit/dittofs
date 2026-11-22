package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// ntlmKey is the installation setting that turns NTLM sign-in on, whose
// record a password set derives its NT hash under.
const ntlmKey = "identity.ntlm"

var installationScope = entity.SettingScope{Kind: entity.ScopeInstallation}

// A password set's write refuses one of these when what it was sealed under
// moved since: the account's record, or identity.ntlm. Each is a store that
// could not answer the set as asked, so a retry seals again.
var (
	errAccountMoved = fmt.Errorf("%w: the account changed while its password was sealed; retry", vfserr.ErrUnavailable)
	errNTLMMoved    = fmt.Errorf("%w: identity.ntlm changed while the password was sealed; retry", vfserr.ErrUnavailable)
)

// sealTries bounds how many times a password change seals again after
// identity.ntlm moved under it.
const sealTries = 3

// PasswordSet is a new password sealed for one account and not yet written:
// the record it was derived from, the identity.ntlm setting read then, and
// the sealed password hash and NT hash, the NT hash's Version zero when the
// account gets none.
type PasswordSet struct {
	user   entity.User
	ntlm   identity.NTLMSetting
	pw, nt entity.Secret
}

// ntlmOf is identity.ntlm as its record s, ok false for none, leaves it: on
// unless the record holds false, as identity decodes it, with the record's
// generation.
func ntlmOf(s entity.Setting, ok bool) identity.NTLMSetting {
	var on bool
	return identity.NTLMSetting{On: !ok || json.Unmarshal(s.Value, &on) != nil || on, Generation: s.Generation}
}

// newPassword refuses an empty new password, 400.
func newPassword(p []byte) error {
	if len(p) == 0 {
		return fmt.Errorf("%w: a password is set with a value", vfserr.ErrInvalid)
	}
	return nil
}

// orNew is ref, or a fresh reference when ref is zero.
func orNew(ref entity.SecretRef) entity.SecretRef {
	if ref == (entity.SecretRef{}) {
		_, _ = rand.Read(ref[:]) // crypto/rand.Read never fails
	}
	return ref
}

// SealPassword derives u's hashes of password with identity's NewHashes,
// under identity.ntlm as the store holds it now, and seals them before the
// write: the password hash under the storage role, and the NT hash, when
// NewHashes derives one, under the protocol role, each as the next version
// of the reference u holds or of a new one. It writes nothing. An empty
// password, and a directory principal's, are refused ErrInvalid; a store or
// a sealer that cannot answer, ErrUnavailable.
func (s *Service) SealPassword(ctx context.Context, u entity.User, password []byte) (PasswordSet, error) {
	if err := newPassword(password); err != nil {
		return PasswordSet{}, err
	}
	if s.d.Sealer == nil {
		return PasswordSet{}, fmt.Errorf("%w: the node holds no secret provider to seal a password with", vfserr.ErrUnavailable)
	}
	ps := PasswordSet{user: u, ntlm: ntlmOf(entity.Setting{}, false)}
	for set, err := range s.d.Store.Settings(ctx, installationScope) {
		if err != nil {
			return PasswordSet{}, unavailable(err)
		}
		if set.Key == ntlmKey {
			ps.ntlm = ntlmOf(set, true)
			break
		}
	}
	h, err := identity.NewHashes(u, password, ps.ntlm)
	if err != nil {
		return PasswordSet{}, err
	}
	defer clear(h.Password)
	defer clear(h.NTHash)
	id := u.Principal.ID
	if ps.pw, err = s.d.Sealer.Seal(ctx, entity.SecretPassword, entity.UserSecretOwner(id, entity.UserPasswordKey), orNew(u.Password), h.Password); err != nil {
		return PasswordSet{}, unavailable(err)
	}
	if h.NTHash != nil {
		if ps.nt, err = s.d.Sealer.Seal(ctx, entity.SecretNTHash, entity.UserSecretOwner(id, entity.UserNTHashKey), orNew(u.NTHash), h.NTHash); err != nil {
			return PasswordSet{}, unavailable(err)
		}
	}
	return ps, nil
}

// SetPassword writes ps in tx, as a user's :set-password does: the account's
// password, its NT hash or none, the previous versions of both pruned, its
// not-before raised and its must-change cleared, then its sign-in count
// reset. It answers what writePassword refuses.
func (s *Service) SetPassword(tx entity.ControlPlaneTx, ps PasswordSet) error {
	if _, _, err := writePassword(tx, ps); err != nil {
		return err
	}
	return ResetAccount(tx, ps.user.Principal)
}

// writePassword writes ps in tx and returns the record before and after. It
// reads identity.ntlm tracked and refuses errNTLMMoved if it is not the
// setting ps was sealed under, so no NT hash derived while NTLM was on is
// written after it was turned off; and it refuses errAccountMoved if the
// account is gone, marked deleted or written since ps read it. Then it puts
// both sealed versions and prunes every version below each, sets the
// account's references to them, raises its not-before to the whole second
// after tx's store time, so every credential issued before the write is
// refused, and clears its must-change.
//
// decision: an account that gets no NT hash, as one does while NTLM is off,
// names none after the write, and the hash its reference named, the old
// password's, is left to the NT-hash deletion that turning NTLM off starts,
// which drops every secret of that kind whether or not a record names it.
// NewHashes derives no NT hash with NTLM on only for an account holding no
// uid, which never held one. Overturned if an account can lose its NT hash
// while NTLM stays on, which would leave an orphan no deletion reaches.
func writePassword(tx entity.ControlPlaneTx, ps PasswordSet) (before, after entity.User, err error) {
	set, ok, err := tx.Setting(installationScope, ntlmKey)
	if err != nil {
		return before, after, err
	}
	if ntlmOf(set, ok) != ps.ntlm {
		return before, after, errNTLMMoved
	}
	before, found, err := tx.User(ps.user.Principal)
	switch {
	case err != nil:
		return before, after, err
	case !found || before.Deleted || before.Generation != ps.user.Generation:
		return before, after, errAccountMoved
	}
	after = before
	after.Password, after.NTHash = ps.pw.ID, ps.nt.ID
	for _, sec := range []entity.Secret{ps.pw, ps.nt} {
		if sec.Version == 0 {
			continue
		}
		if err := tx.PutSecret(sec, sec.Version-1); err != nil {
			return before, after, err
		}
		if err := tx.PruneSecret(sec.ID, sec.Version); err != nil {
			return before, after, err
		}
	}
	after.MustChange = false
	if nb := tx.Now().Truncate(time.Second).Add(time.Second); nb.After(after.NotBefore) {
		after.NotBefore = nb
	}
	if after.Generation, err = tx.PutUser(after, before.Generation, entity.Alloc{}); err != nil {
		return before, after, err
	}
	return before, after, nil
}

// passwordChange is a password write's audit change: the password, and the
// NT hash when one was sealed, as "set", never their values.
func passwordChange(ps PasswordSet) []byte {
	c := map[string]string{"password": "set"}
	if ps.nt.Version != 0 {
		c["nt_hash"] = "set"
	}
	b, _ := json.Marshal(c) // a map of strings always encodes
	return b
}

// ChangePassword is POST /auth/password: the caller's own password changed,
// on presenting its current one, must-change or not. The check of the
// current password is an attempt as a sign-in's is: counted on the
// caller's account and the source rec names before it is checked, waited on,
// and settled in the write that follows, a failure audited as refused and
// answered ErrLogonFailure. On a success the new password is sealed, then
// one write puts it as SetPassword's does, settles the attempt as a
// success, and creates the session it answers, created at the not-before
// that write sets, so every earlier session, the one that made the change
// included, refreshes no more. identity.ntlm moving between the seal and the
// write seals again, up to sealTries times; an account changed since its
// password was checked is refused as a failed attempt.
func (s *Service) ChangePassword(ctx context.Context, rec entity.AuditRecord, in apiv1.PasswordChange) (apiv1.Tokens, Audited, error) {
	current, next := []byte(in.Current), []byte(in.New)
	defer clear(current)
	defer clear(next)
	c, ok := CallerFrom(ctx)
	if !ok {
		return apiv1.Tokens{}, AuditedNothing, vfserr.ErrLogonFailure
	}
	if err := newPassword(next); err != nil {
		return apiv1.Tokens{}, AuditedNothing, err
	}
	src, err := sourceIn(ctx, rec)
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, err
	}
	k, err := s.d.Tokens.current(ctx)
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	}
	p := c.Principal
	cnt, err := reserve(ctx, s.d.Store, s.d.Clock, attempt{account: p, source: src})
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	}
	u, aerr := s.d.Accounts.Account(ctx, p)
	verr := s.d.Accounts.VerifyPassword(ctx, p, current)
	for _, err := range []error{aerr, verr} {
		if err != nil && !errors.Is(err, vfserr.ErrLogonFailure) {
			return apiv1.Tokens{}, AuditedNothing, unavailable(errors.Join(err, unreserve(ctx, s.d.Store, cnt)))
		}
	}
	rec.Target = entity.ResourceRef{Kind: entity.ResourceUser, ID: p.ID[:]}
	failed := rec
	failed.Outcome = []byte("refused: " + apiv1.CodeUnauthenticated)
	if aerr != nil || verr != nil {
		if err := s.d.Store.Write(ctx, entity.WriteMeta{Audit: &failed}, func(tx entity.ControlPlaneTx) error {
			return settleFailure(tx, cnt)
		}); err != nil {
			return apiv1.Tokens{}, AuditedNothing, unavailable(err)
		}
		return apiv1.Tokens{}, AuditedRefused, vfserr.ErrLogonFailure
	}
	refresh, hash := newRefresh()
	sess := entity.Session{Account: p, Refresh: hash}
	_, _ = rand.Read(sess.ID[:]) // crypto/rand.Read never fails
	lifetime := s.d.Settings().SessionLifetime
	var out entity.AuditRecord
	var refused bool
	for try := 1; ; try++ {
		ps, err := s.SealPassword(ctx, u, next)
		if err != nil {
			if !errors.Is(err, vfserr.ErrInvalid) {
				err = unavailable(err)
			}
			return apiv1.Tokens{}, AuditedNothing, errors.Join(err, unreserve(ctx, s.d.Store, cnt))
		}
		err = s.d.Store.Write(ctx, entity.WriteMeta{Audit: &out}, func(tx entity.ControlPlaneTx) error {
			before, after, err := writePassword(tx, ps)
			if refused = errors.Is(err, errAccountMoved); refused {
				out = failed
				return settleFailure(tx, cnt)
			}
			if err != nil {
				return err
			}
			out = rec
			out.Before, out.After, out.Change, out.Outcome = before.Generation, after.Generation, passwordChange(ps), []byte("applied")
			if err := takeBackSuccess(tx, cnt); err != nil {
				return err
			}
			sess.Created, sess.Expires = after.NotBefore, tx.Now().Add(lifetime)
			return tx.PutSession(sess)
		})
		switch {
		case errors.Is(err, errNTLMMoved) && try < sealTries:
			continue
		case err != nil:
			return apiv1.Tokens{}, AuditedNothing, errors.Join(unavailable(err), unreserve(ctx, s.d.Store, cnt))
		case refused:
			return apiv1.Tokens{}, AuditedRefused, vfserr.ErrLogonFailure
		}
		t, err := tokens(s.d.Tokens.signer(k), p, sess.ID, sess.Created, refresh, false)
		return t, AuditedApplied, err
	}
}
