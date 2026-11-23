package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// NTLMKey is the installation setting that turns NTLM sign-in on, whose
// record a password set derives its NT hash under.
const NTLMKey = "identity.ntlm"

var installationScope = entity.SettingScope{Kind: entity.ScopeInstallation}

// A password write refuses one of these when what it was sealed under moved
// since: the account's record, or identity.ntlm. Each is a store that could
// not answer the set as asked, so a retry seals again.
var (
	errAccountMoved = fmt.Errorf("%w: the account changed while its password was sealed; retry", vfserr.ErrUnavailable)
	errNTLMMoved    = fmt.Errorf("%w: identity.ntlm changed while the password was sealed; retry", vfserr.ErrUnavailable)
)

// sealTries bounds how many times a password change seals again after
// identity.ntlm moved under it.
const sealTries = 3

// NTLM is identity.ntlm as its installation record s leaves it, ok false for
// none: the default, on, with no record, else the record's JSON bool, with
// the record's generation, which a password write is conditional on.
//
// decision: a record that holds no JSON bool is refused, ErrUnavailable,
// never read as on or off, because identity refuses it too and keeps
// running the value it last applied, which this read cannot know. So a
// password set is refused while such a record is stored, until an admin's
// write of identity.ntlm repairs it, the one read that takes it as on; the
// settings table refuses writing one, so only a store written around it
// holds one. Overturned if identity ever reads such a record as a value.
func NTLM(s entity.Setting, ok bool) (identity.NTLMSetting, error) {
	if !ok {
		return identity.NTLMSetting{On: true}, nil
	}
	var on bool
	if err := json.Unmarshal(s.Value, &on); err != nil {
		return identity.NTLMSetting{}, fmt.Errorf("%w: the stored %s is no bool: %w", vfserr.ErrUnavailable, NTLMKey, err)
	}
	return identity.NTLMSetting{On: on, Generation: s.Generation}, nil
}

// settingsReader is the store's untracked read of one scope's settings.
type settingsReader interface {
	Settings(ctx context.Context, scope entity.SettingScope) iter.Seq2[entity.Setting, error]
}

// ReadNTLM is identity.ntlm as st holds it now, read untracked, as NTLM
// decodes it.
func ReadNTLM(ctx context.Context, st settingsReader) (identity.NTLMSetting, error) {
	for set, err := range st.Settings(ctx, installationScope) {
		if err != nil {
			return identity.NTLMSetting{}, unavailable(err)
		}
		if set.Key == NTLMKey {
			return NTLM(set, true)
		}
	}
	return NTLM(entity.Setting{}, false)
}

// PasswordSet is a new password sealed for one account and not yet written:
// the record it was derived from, the identity.ntlm setting it was derived
// under, and the sealed password hash and NT hash, the NT hash's Version
// zero when the account gets none.
type PasswordSet struct {
	user   entity.User
	ntlm   identity.NTLMSetting
	pw, nt entity.Secret
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

// SealPassword derives u's hashes of password with identity's NewHashes
// under ntlm, and seals them through s before the write: the password hash,
// and the NT hash when NewHashes derives one, each as the next version of
// the reference u holds or of a new one. It writes nothing. An empty
// password, one that is not UTF-8 and a directory principal's are refused
// ErrInvalid; what the sealer refuses is returned as it is.
func SealPassword(ctx context.Context, s Sealer, u entity.User, password []byte, ntlm identity.NTLMSetting) (PasswordSet, error) {
	if err := newPassword(password); err != nil {
		return PasswordSet{}, err
	}
	h, err := identity.NewHashes(u, password, ntlm)
	if err != nil {
		return PasswordSet{}, err
	}
	defer clear(h.Password)
	defer clear(h.NTHash)
	ps, id := PasswordSet{user: u, ntlm: ntlm}, u.Principal.ID
	if ps.pw, err = s.Seal(ctx, entity.SecretPassword, entity.UserSecretOwner(id, entity.UserPasswordKey), orNew(u.Password), h.Password); err != nil {
		return PasswordSet{}, err
	}
	if h.NTHash != nil {
		if ps.nt, err = s.Seal(ctx, entity.SecretNTHash, entity.UserSecretOwner(id, entity.UserNTHashKey), orNew(u.NTHash), h.NTHash); err != nil {
			return PasswordSet{}, err
		}
	}
	return ps, nil
}

// SealPassword seals password for u as SealPassword does, under
// identity.ntlm as the store holds it now; a store or a sealer that cannot
// answer is ErrUnavailable.
func (s *Service) SealPassword(ctx context.Context, u entity.User, password []byte) (PasswordSet, error) {
	ntlm, err := ReadNTLM(ctx, s.d.Store)
	if err != nil {
		return PasswordSet{}, err
	}
	ps, err := SealPassword(ctx, s.d.Sealer, u, password, ntlm)
	if err != nil && !errors.Is(err, vfserr.ErrInvalid) {
		err = unavailable(err)
	}
	return ps, err
}

// SetPassword writes ps in tx, as a user's :set-password does: the write
// WritePassword makes, must-change cleared, then the account's sign-in count
// reset.
func (s *Service) SetPassword(tx entity.ControlPlaneTx, ps PasswordSet) error {
	if _, _, err := WritePassword(tx, ps, WriteOpts{}); err != nil {
		return err
	}
	return ResetAccount(tx, ps.user.Principal)
}

// WriteOpts is what a password write sets beside the password: whether the
// account must change it before any other call.
type WriteOpts struct {
	MustChange bool
}

// WritePassword writes ps in tx and returns the record before and after. It
// reads identity.ntlm tracked and refuses errNTLMMoved if it is not the
// setting ps was sealed under, so no NT hash derived while NTLM was on is
// written after it was turned off; and it refuses errAccountMoved if the
// account is gone, marked deleted or written since ps read it. Then it puts
// both sealed versions and prunes every version below each with
// tx.PruneSecret, at once and whatever a node reports holding, as a password
// set drops the old password's hashes; sets the account's references to
// them; raises its not-before strictly past both tx's store time and the
// not-before it held, so every credential issued before the write is
// refused, one a write earlier in the same second issued included; and sets
// its must-change to o's.
//
// decision: an account that gets no NT hash, as one does while NTLM is off,
// names none after the write, and the hash its reference named, the old
// password's, is left to the NT-hash deletion that turning NTLM off starts,
// which drops every secret of that kind whether or not a record names it.
// With NTLM on, NewHashes derives none only for an account holding no uid,
// and the store's keepSID rule, a user holds a uid and a SID or neither,
// refuses dropping a uid, so such an account never held one. Overturned if
// an account can lose its uid, or its NT hash while NTLM stays on, which
// would leave an orphan no deletion reaches.
func WritePassword(tx entity.ControlPlaneTx, ps PasswordSet, o WriteOpts) (before, after entity.User, err error) {
	set, ok, err := tx.Setting(installationScope, NTLMKey)
	if err != nil {
		return before, after, err
	}
	ntlm, err := NTLM(set, ok)
	if err != nil {
		return before, after, err
	}
	if ntlm != ps.ntlm {
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
	after.MustChange = o.MustChange
	after.NotBefore = issuedAt(tx.Now(), after.NotBefore).Add(time.Second)
	if after.Generation, err = tx.PutUser(after, before.Generation, entity.Alloc{}); err != nil {
		return before, after, err
	}
	return before, after, nil
}

// PasswordChange is a password write's audit change, from the account
// before and after WritePassword, in the API's form: a secret field as
// "set", never its value; an NT hash the account stops naming as its old
// reference and null; and must-change and not-before as their old and new
// values.
func PasswordChange(before, after entity.User) ([]byte, error) {
	c := map[string]any{"password": "set"}
	switch {
	case after.NTHash != (entity.SecretRef{}):
		c["nt_hash"] = "set"
	case before.NTHash != (entity.SecretRef{}):
		c["nt_hash"] = [2]any{fmt.Sprintf("%x-%x-%x-%x-%x", before.NTHash[:4], before.NTHash[4:6], before.NTHash[6:8], before.NTHash[8:10], before.NTHash[10:]), nil}
	}
	if before.MustChange != after.MustChange {
		c["must_change"] = [2]any{before.MustChange, after.MustChange}
	}
	c["not_before"] = [2]any{before.NotBefore, after.NotBefore}
	return json.Marshal(c)
}

// ChangePassword is POST /auth/password: the caller's own password changed,
// on presenting its current one, must-change or not. An empty new password
// is refused ErrInvalid and audited, before anything is counted. The check
// of the current password is an attempt as a sign-in's is: counted on the
// caller's account and the source rec names before it is checked, waited on,
// and settled, a failure audited as refused and answered ErrLogonFailure. On
// a success the new password is sealed, then one write puts it as
// SetPassword's does, settles the attempt as a success, and creates the
// session it answers, created at the not-before that write sets, so every
// earlier session, the one that made the change included, refreshes no
// more. identity.ntlm moving between the seal and the write seals again, up
// to sealTries times. A new password NewHashes refuses is ErrInvalid,
// audited, its attempt's raise taken back.
//
// The password fields arrive as strings, which cannot be cleared: only the
// byte copies made here are, and the request's body, which the adapter
// clears once decoded.
//
// decision: an account written between the check and the write answers
// ErrUnavailable, its attempt's raise taken back, never a settled failure:
// the current password was right, and what moved the account is the
// NT-hash deletion, a caller holding the Principals right, which can set
// the password anyway, or the account's own concurrent change, whose retry
// is then refused as the current password is no longer current. The client
// signs in again or retries. Overturned if a 503 here must tell those apart.
func (s *Service) ChangePassword(ctx context.Context, rec entity.AuditRecord, in apiv1.PasswordChange) (apiv1.Tokens, Audited, error) {
	current, next := []byte(in.Current), []byte(in.New)
	defer clear(current)
	defer clear(next)
	c, ok := CallerFrom(ctx)
	if !ok {
		return apiv1.Tokens{}, AuditedNothing, vfserr.ErrLogonFailure
	}
	p := c.Principal
	rec.Target = entity.ResourceRef{Kind: entity.ResourceUser, ID: p.ID[:]}
	invalid := rec
	invalid.Outcome = []byte("refused: invalid")
	if err := newPassword(next); err != nil {
		if err := s.d.Store.Write(ctx, entity.WriteMeta{Audit: &invalid}, func(entity.ControlPlaneTx) error { return nil }); err != nil {
			return apiv1.Tokens{}, AuditedNothing, unavailable(err)
		}
		return apiv1.Tokens{}, AuditedRefused, err
	}
	src, err := sourceIn(ctx, rec)
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, err
	}
	k, err := s.d.Tokens.current(ctx)
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	}
	cnt, err := reserve(ctx, s.d.Store, s.d.Clock, attempt{account: p, source: src})
	if err != nil {
		return apiv1.Tokens{}, AuditedNothing, unavailable(err)
	}
	failed := rec
	failed.Outcome = []byte("refused: " + apiv1.CodeUnauthenticated)
	u, audited, err := s.checked(ctx, cnt, p, current, failed)
	if err != nil {
		return apiv1.Tokens{}, audited, err
	}
	refresh, hash := newRefresh()
	sess := entity.Session{Account: p, Refresh: hash}
	_, _ = rand.Read(sess.ID[:]) // crypto/rand.Read never fails
	lifetime := s.d.Settings().SessionLifetime
	for try := 1; ; try++ {
		ps, err := s.SealPassword(ctx, u, next)
		if errors.Is(err, vfserr.ErrInvalid) {
			if werr := s.d.Store.Write(ctx, entity.WriteMeta{Audit: &invalid}, func(tx entity.ControlPlaneTx) error {
				return takeBackRaise(tx, cnt)
			}); werr != nil {
				return apiv1.Tokens{}, AuditedNothing, unavailable(werr)
			}
			return apiv1.Tokens{}, AuditedRefused, err
		}
		if err != nil {
			return apiv1.Tokens{}, AuditedNothing, unavailable(errors.Join(err, unreserve(ctx, s.d.Store, cnt)))
		}
		out := rec
		err = s.d.Store.Write(ctx, entity.WriteMeta{Audit: &out}, func(tx entity.ControlPlaneTx) error {
			before, after, err := WritePassword(tx, ps, WriteOpts{})
			if err != nil {
				return err
			}
			change, err := PasswordChange(before, after)
			if err != nil {
				return err
			}
			out = rec
			out.Before, out.After, out.Change, out.Outcome = before.Generation, after.Generation, change, []byte("applied")
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
			return apiv1.Tokens{}, AuditedNothing, unavailable(errors.Join(err, unreserve(ctx, s.d.Store, cnt)))
		}
		t, err := tokens(s.d.Tokens.signer(k), p, sess.ID, sess.Created, refresh, false)
		return t, AuditedApplied, err
	}
}
