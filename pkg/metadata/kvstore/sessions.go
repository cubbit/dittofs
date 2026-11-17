package kvstore

import (
	"bytes"
	"context"
	"iter"
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// A session is SS‖session, with one empty SS‖session‖h‖hash row per refresh
// hash it has had, each mirrored by SSH‖hash naming the session, so deleting
// the session deletes its hashes by one range of its own. SSA‖account‖session
// lists an account's sessions. A token is AT‖token, with ATH‖hash and
// ATA‖account‖token. Each has one XP row at its Expires.
//
// A sign-in count is SF‖tag‖tail. Its XP row, where it has one, repeats the
// tail after its own tag, so the sweep finds the count from the row.

func sessionKey(id [16]byte) []byte { return keys.ID([]byte{keys.KindSS}, id[:]) }

func sessionHashesPrefix(id [16]byte) []byte { return append(sessionKey(id), keys.SSh) }

func sessionHashKey(id [16]byte, hash [32]byte) []byte {
	return keys.ID(sessionHashesPrefix(id), hash[:])
}

func refreshKey(hash [32]byte) []byte { return keys.ID([]byte{keys.KindSSH}, hash[:]) }

func accountSessionsPrefix(account entity.Principal) []byte {
	return keys.ID([]byte{keys.KindSSA}, account.ID[:])
}

func accountSessionKey(account entity.Principal, id [16]byte) []byte {
	return keys.ID(accountSessionsPrefix(account), id[:])
}

func tokenKey(id [16]byte) []byte { return keys.ID([]byte{keys.KindAT}, id[:]) }

func tokenHashKey(hash [32]byte) []byte { return keys.ID([]byte{keys.KindATH}, hash[:]) }

func accountTokensPrefix(account entity.Principal) []byte {
	return keys.ID([]byte{keys.KindATA}, account.ID[:])
}

func accountTokenKey(account entity.Principal, id [16]byte) []byte {
	return keys.ID(accountTokensPrefix(account), id[:])
}

func sessionExpiryKey(s entity.Session) []byte { return expiryKey(s.Expires, keys.XPSession, s.ID[:]) }

func tokenExpiryKey(t entity.APIToken) []byte { return expiryKey(t.Expires, keys.XPToken, t.ID[:]) }

// signInTail is k's SF tag and the bytes after it. A source is its masked
// address then its prefix length; it ends every key it is in, so it needs no
// terminator.
func signInTail(k entity.SignInKey) (byte, []byte, error) {
	var addr []byte
	if p := k.Source; p.IsValid() {
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		p = p.Masked()
		addr = append(p.Addr().AsSlice(), byte(p.Bits()))
	}
	switch account := k.Account != (entity.Principal{}); {
	case account && addr != nil:
		return keys.SFPair, append(k.Account.ID[:], addr...), nil
	case account:
		return keys.SFAccount, k.Account.ID[:], nil
	case addr != nil:
		return keys.SFSource, addr, nil
	}
	return 0, nil, merrs.NewInvalidArgumentError("a sign-in count names an account, a source or both")
}

func signInKey(tag byte, tail []byte) []byte { return keys.ID([]byte{keys.KindSF, tag}, tail) }

// signInExpiryKey is the XP row of a count holding f: a source's 15 min
// after Failed, a pair's 24 h after Last, and none for an account's.
func signInExpiryKey(tag byte, tail []byte, f entity.SignInFailures) []byte {
	switch tag {
	case keys.SFSource:
		return expiryKey(f.Failed.Add(entity.SourceSignInLife), keys.XPSource, tail)
	case keys.SFPair:
		return expiryKey(f.Last.Add(entity.PairSignInLife), keys.XPPair, tail)
	}
	return nil
}

// moveRow replaces the empty row old with now: either may be nil, for none.
func moveRow(tx kv.Txn, old, now []byte) error {
	if old != nil && !bytes.Equal(old, now) {
		if err := tx.Delete(old); err != nil {
			return err
		}
	}
	if now == nil {
		return nil
	}
	return tx.Set(now, nil)
}

func sessionIn(r kv.Reader, id [16]byte) (entity.Session, bool, error) {
	b, ok, err := get(r, sessionKey(id))
	if err != nil || !ok {
		return entity.Session{}, false, err
	}
	s, err := decodeSession(id, b)
	return s, err == nil, err
}

func sessionByRefreshIn(r kv.Reader, hash [32]byte) (entity.Session, bool, error) {
	b, ok, err := get(r, refreshKey(hash))
	if err != nil || !ok {
		return entity.Session{}, false, err
	}
	id, err := decodeID(b)
	if err != nil {
		return entity.Session{}, false, err
	}
	s, found, err := sessionIn(r, id)
	return ofLive(r, s, s.Account, found, err)
}

func tokenIn(r kv.Reader, id [16]byte) (entity.APIToken, bool, error) {
	b, ok, err := get(r, tokenKey(id))
	if err != nil || !ok {
		return entity.APIToken{}, false, err
	}
	t, err := decodeToken(id, b)
	return t, err == nil, err
}

func tokenByHashIn(r kv.Reader, hash [32]byte) (entity.APIToken, bool, error) {
	b, ok, err := get(r, tokenHashKey(hash))
	if err != nil || !ok {
		return entity.APIToken{}, false, err
	}
	id, err := decodeID(b)
	if err != nil {
		return entity.APIToken{}, false, err
	}
	t, found, err := tokenIn(r, id)
	return ofLive(r, t, t.Account, found, err)
}

// ofLive is v, found, unless account is deleted or marked deleted: from its
// account's mark, a session or a token is found by no read path. Given a
// kv.Txn, the liveness read is tracked, so a write that read v conflicts with
// a mark that lands before it commits; given a view's reader, it is not.
//
// decision: a deleted account's row and a marked one's both read as not
// found, with no error, so a caller cannot tell them apart. That holds only
// while every Put of a session or a token goes through accountLive; a write
// path that skips it could recreate what a marked account's reads hide.
func ofLive[T any](r kv.Reader, v T, account entity.Principal, found bool, err error) (T, bool, error) {
	if err == nil && found {
		found, err = live(r, account)
	}
	if err != nil || !found {
		var zero T
		return zero, false, err
	}
	return v, true, nil
}

// ofLiveAccount is seq when account is live as the listing starts, and
// empty otherwise.
//
// ponytail: the account is checked once, in its own view, before the first
// page, so a mark landing at any point after that check lets every page
// through until the deletion's batch drops the rows. Check it in each page's
// view if a listing must stop at the mark.
func ofLiveAccount[T any](ctx context.Context, db kv.KV, account entity.Principal, seq iter.Seq2[T, error]) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var ok bool
		if err := db.View(ctx, func(r kv.Reader) (err error) {
			ok, err = live(r, account)
			return err
		}); err != nil {
			var zero T
			yield(zero, err)
			return
		}
		if ok {
			seq(yield)
		}
	}
}

// accountLive refuses a write for an account deleted or marked deleted. Its
// read is tracked, so the write conflicts with the mark and with the batch
// that deletes the record.
func accountLive(tx kv.Txn, account entity.Principal) error {
	if ok, err := live(tx, account); err != nil || !ok {
		return notFound(err, account)
	}
	return nil
}

func (c controlTx) Session(id [16]byte) (entity.Session, bool, error) {
	s, found, err := sessionIn(c.tx, id)
	return ofLive(c.tx, s, s.Account, found, err)
}

func (c controlTx) SessionByRefresh(hash [32]byte) (entity.Session, bool, error) {
	return sessionByRefreshIn(c.tx, hash)
}

// beforeEpoch reports a time the expiry index cannot order, the zero time
// among them.
func beforeEpoch(t time.Time) bool { return t.Before(time.Unix(0, 0)) }

// hashFree refuses a hash indexed under key to an ID other than id, whose
// index row deleting that ID would then delete.
func hashFree(r kv.Reader, key []byte, id [16]byte) error {
	b, ok, err := get(r, key)
	if err != nil || !ok {
		return err
	}
	other, err := decodeID(b)
	if err == nil && other != id {
		err = merrs.NewAlreadyExistsError("hash")
	}
	return err
}

func (c controlTx) PutSession(s entity.Session) error {
	if _, found, err := sessionIn(c.tx, s.ID); err != nil || found {
		if found {
			err = merrs.NewAlreadyExistsError("session")
		}
		return err
	}
	return c.writeSession(s, nil)
}

func (c controlTx) UpdateSession(s entity.Session) error {
	old, found, err := sessionIn(c.tx, s.ID)
	if err != nil {
		return err
	}
	if !found {
		return merrs.NewNotFoundError("session", "session")
	}
	if old.Account != s.Account {
		return merrs.NewInvalidArgumentError("a session keeps its account")
	}
	return c.writeSession(s, sessionExpiryKey(old))
}

// writeSession writes s, moving its expiry row from oldExpiry.
func (c controlTx) writeSession(s entity.Session, oldExpiry []byte) error {
	if s.Account.Kind != entity.PrincipalUser || beforeEpoch(s.Expires) {
		return merrs.NewInvalidArgumentError("a session needs a user account and an expiry after 1970")
	}
	if err := accountLive(c.tx, s.Account); err != nil {
		return err
	}
	if err := hashFree(c.tx, refreshKey(s.Refresh), s.ID); err != nil {
		return err
	}
	for _, kvp := range []kv.KeyValue{
		{Key: sessionKey(s.ID), Value: encodeSession(s)},
		{Key: accountSessionKey(s.Account, s.ID)},
		{Key: refreshKey(s.Refresh), Value: encodeID(s.ID)},
		{Key: sessionHashKey(s.ID, s.Refresh)},
	} {
		if err := c.tx.Set(kvp.Key, kvp.Value); err != nil {
			return err
		}
	}
	return moveRow(c.tx, oldExpiry, sessionExpiryKey(s))
}

func (c controlTx) DeleteSession(id [16]byte) error {
	s, found, err := sessionIn(c.tx, id)
	if err != nil || !found {
		return err
	}
	rows, err := sessionHashRows(c.tx, id)
	if err != nil {
		return err
	}
	return deleteRows(c.tx, append(rows, sessionKey(id), accountSessionKey(s.Account, id), sessionExpiryKey(s)))
}

// sessionHashRows lists the rows of every refresh hash session id has had,
// each followed by its SSH mirror.
func sessionHashRows(r kv.Reader, id [16]byte) ([][]byte, error) {
	prefix := sessionHashesPrefix(id)
	var rows [][]byte
	for kvp, err := range r.Scan(prefix, nil) {
		if err != nil {
			return nil, err
		}
		rows = append(rows, bytes.Clone(kvp.Key), refreshKey([32]byte(kvp.Key[len(prefix):])))
	}
	return rows, nil
}

func deleteRows(tx kv.Txn, rows [][]byte) error {
	for _, k := range rows {
		if err := tx.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

func (c controlTx) APIToken(id [16]byte) (entity.APIToken, bool, error) {
	t, found, err := tokenIn(c.tx, id)
	return ofLive(c.tx, t, t.Account, found, err)
}

func (c controlTx) APITokenByHash(hash [32]byte) (entity.APIToken, bool, error) {
	return tokenByHashIn(c.tx, hash)
}

func (c controlTx) PutAPIToken(t entity.APIToken) error {
	if t.Account.Kind != entity.PrincipalUser || beforeEpoch(t.Expires) {
		return merrs.NewInvalidArgumentError("a token needs a user account and an expiry after 1970")
	}
	if err := accountLive(c.tx, t.Account); err != nil {
		return err
	}
	if _, found, err := tokenIn(c.tx, t.ID); err != nil || found {
		if found {
			err = merrs.NewAlreadyExistsError("api token")
		}
		return err
	}
	if err := hashFree(c.tx, tokenHashKey(t.Hash), t.ID); err != nil {
		return err
	}
	for _, kvp := range []kv.KeyValue{
		{Key: tokenKey(t.ID), Value: encodeToken(t)},
		{Key: tokenHashKey(t.Hash), Value: encodeID(t.ID)},
		{Key: accountTokenKey(t.Account, t.ID)},
		{Key: tokenExpiryKey(t)},
	} {
		if err := c.tx.Set(kvp.Key, kvp.Value); err != nil {
			return err
		}
	}
	return nil
}

func (c controlTx) DeleteAPIToken(id [16]byte) error {
	t, found, err := tokenIn(c.tx, id)
	if err != nil || !found {
		return err
	}
	for _, k := range [][]byte{tokenKey(id), tokenHashKey(t.Hash), accountTokenKey(t.Account, id), tokenExpiryKey(t)} {
		if err := c.tx.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// SignInFailures reads a count tracked, so concurrent raises of it conflict.
func (c controlTx) SignInFailures(k entity.SignInKey) (entity.SignInFailures, bool, error) {
	tag, tail, err := signInTail(k)
	if err != nil {
		return entity.SignInFailures{}, false, err
	}
	return signInIn(c.tx, tag, tail)
}

func signInIn(r kv.Reader, tag byte, tail []byte) (entity.SignInFailures, bool, error) {
	b, ok, err := get(r, signInKey(tag, tail))
	if err != nil || !ok {
		return entity.SignInFailures{}, false, err
	}
	f, err := decodeSignIn(b)
	return f, err == nil, err
}

func (c controlTx) PutSignInFailures(k entity.SignInKey, f entity.SignInFailures) error {
	tag, tail, err := signInTail(k)
	if err != nil {
		return err
	}
	if tag == keys.SFSource && beforeEpoch(f.Failed) || tag == keys.SFPair && beforeEpoch(f.Last) {
		return merrs.NewInvalidArgumentError("a count's expiry follows a time after 1970")
	}
	if k.Account != (entity.Principal{}) {
		if err := accountLive(c.tx, k.Account); err != nil {
			return err
		}
	}
	old, found, err := signInIn(c.tx, tag, tail)
	if err != nil {
		return err
	}
	var oldExpiry []byte
	if found {
		oldExpiry = signInExpiryKey(tag, tail, old)
	}
	if err := c.tx.Set(signInKey(tag, tail), encodeSignIn(f)); err != nil {
		return err
	}
	return moveRow(c.tx, oldExpiry, signInExpiryKey(tag, tail, f))
}

func (c controlTx) DeleteSignInFailures(k entity.SignInKey) error {
	tag, tail, err := signInTail(k)
	if err != nil {
		return err
	}
	old, found, err := signInIn(c.tx, tag, tail)
	if err != nil || !found {
		return err
	}
	if err := c.tx.Delete(signInKey(tag, tail)); err != nil {
		return err
	}
	return moveRow(c.tx, signInExpiryKey(tag, tail, old), nil)
}

// SignInFailures reads a count untracked, for a refusal that writes nothing.
func (c ControlPlane) SignInFailures(ctx context.Context, k entity.SignInKey) (f entity.SignInFailures, found bool, err error) {
	tag, tail, err := signInTail(k)
	if err != nil {
		return f, false, err
	}
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		f, found, err = signInIn(r, tag, tail)
		return err
	})
	return f, found, err
}

// ClearSourceSignIns deletes a batch of source counts with their expiry
// rows, in one transaction within the key budget, and reports whether
// source counts are left. Account and pair counts are untouched.
func (c ControlPlane) ClearSourceSignIns(ctx context.Context) (more bool, err error) {
	n := max(1, c.s.k/2)
	prefix := []byte{keys.KindSF, keys.SFSource}
	err = c.s.kv.Update(ctx, func(tx kv.Txn) error {
		more = false
		var rows [][]byte
		for row, err := range tx.Scan(prefix, nil) {
			if err != nil {
				return err
			}
			if len(rows) == 2*n {
				more = true
				break
			}
			f, err := decodeSignIn(row.Value)
			if err != nil {
				return err
			}
			tail := row.Key[len(prefix):]
			rows = append(rows, bytes.Clone(row.Key), signInExpiryKey(keys.SFSource, bytes.Clone(tail), f))
		}
		for _, k := range rows {
			if err := tx.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	return more, err
}

// Session reads one session: the per-call check of an access token's. A
// session whose account is deleted or marked deleted is not found.
func (c ControlPlane) Session(ctx context.Context, id [16]byte) (s entity.Session, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		s, found, err = sessionIn(r, id)
		s, found, err = ofLive(r, s, s.Account, found, err)
		return err
	})
	return s, found, err
}

// SessionByRefresh finds the session a refresh hash was issued to: its
// current one, or one it replaced, which the caller compares with Refresh.
func (c ControlPlane) SessionByRefresh(ctx context.Context, hash [32]byte) (s entity.Session, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		s, found, err = sessionByRefreshIn(r, hash)
		return err
	})
	return s, found, err
}

// Sessions lists an account's sessions; a deleted or marked account has none.
func (c ControlPlane) Sessions(ctx context.Context, account entity.Principal) iter.Seq2[entity.Session, error] {
	prefix := accountSessionsPrefix(account)
	return ofLiveAccount(ctx, c.s.kv, account, pages(ctx, c.s.kv, prefix, nil, func(r kv.Reader, row kv.KeyValue) (entity.Session, error) {
		id := [16]byte(row.Key[len(prefix):])
		b, err := r.Get(sessionKey(id))
		if err != nil {
			return entity.Session{}, err
		}
		return decodeSession(id, b)
	}))
}

// PutSession creates a session.
func (c ControlPlane) PutSession(ctx context.Context, s entity.Session) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.PutSession(s) })
}

// UpdateSession rewrites an existing session, keeping every refresh hash it
// had before findable beside its new one.
func (c ControlPlane) UpdateSession(ctx context.Context, s entity.Session) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.UpdateSession(s) })
}

// DeleteSession deletes a session, its account's index row and every
// refresh hash it had, in one transaction.
//
// ponytail: the transaction deletes every hash the session was ever given,
// about 48 rows for a 12 h session refreshed every 15 min, growing with the
// session lifetime. Delete only the current hash and let the sweep reap the
// rest if a long lifetime makes this transaction large.
func (c ControlPlane) DeleteSession(ctx context.Context, id [16]byte) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.DeleteSession(id) })
}

// APIToken reads one token; one whose account is deleted or marked deleted
// is not found.
func (c ControlPlane) APIToken(ctx context.Context, id [16]byte) (t entity.APIToken, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		t, found, err = tokenIn(r, id)
		t, found, err = ofLive(r, t, t.Account, found, err)
		return err
	})
	return t, found, err
}

// APITokenByHash finds the token a presented token's hash names.
func (c ControlPlane) APITokenByHash(ctx context.Context, hash [32]byte) (t entity.APIToken, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		t, found, err = tokenByHashIn(r, hash)
		return err
	})
	return t, found, err
}

// APITokens lists an account's tokens; a deleted or marked account has none.
func (c ControlPlane) APITokens(ctx context.Context, account entity.Principal) iter.Seq2[entity.APIToken, error] {
	prefix := accountTokensPrefix(account)
	return ofLiveAccount(ctx, c.s.kv, account, pages(ctx, c.s.kv, prefix, nil, func(r kv.Reader, row kv.KeyValue) (entity.APIToken, error) {
		id := [16]byte(row.Key[len(prefix):])
		b, err := r.Get(tokenKey(id))
		if err != nil {
			return entity.APIToken{}, err
		}
		return decodeToken(id, b)
	}))
}

// PutAPIToken creates a token with its hash and account index rows.
func (c ControlPlane) PutAPIToken(ctx context.Context, t entity.APIToken) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.PutAPIToken(t) })
}

// DeleteAPIToken deletes a token with its index rows; an absent one is
// already deleted.
func (c ControlPlane) DeleteAPIToken(ctx context.Context, id [16]byte) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.DeleteAPIToken(id) })
}

func encodeSession(s entity.Session) []byte {
	w := putPrincipal(codec.NewWriter(v1), s.Account).Fixed(s.Refresh[:])
	return putTime(putTime(w, s.Created), s.Expires).Bool(s.Revoked).Encode()
}

func decodeSession(id [16]byte, b []byte) (entity.Session, error) {
	r, err := open(b)
	if err != nil {
		return entity.Session{}, err
	}
	s := entity.Session{ID: id, Account: readPrincipal(r)}
	copy(s.Refresh[:], r.Fixed(32))
	s.Created, s.Expires, s.Revoked = readTime(r), readTime(r), r.Bool()
	return s, r.Done()
}

func encodeToken(t entity.APIToken) []byte {
	w := putPrincipal(codec.NewWriter(v1), t.Account).Fixed(t.Hash[:])
	return putTime(putTime(w, t.Created), t.Expires).Encode()
}

func decodeToken(id [16]byte, b []byte) (entity.APIToken, error) {
	r, err := open(b)
	if err != nil {
		return entity.APIToken{}, err
	}
	t := entity.APIToken{ID: id, Account: readPrincipal(r)}
	copy(t.Hash[:], r.Fixed(32))
	t.Created, t.Expires = readTime(r), readTime(r)
	return t, r.Done()
}

func encodeSignIn(f entity.SignInFailures) []byte {
	w := codec.NewWriter(v1).Uint32(f.Count)
	return putTime(putTime(w, f.Last), f.Failed).Uint64(f.Nonce).Encode()
}

func decodeSignIn(b []byte) (entity.SignInFailures, error) {
	r, err := open(b)
	if err != nil {
		return entity.SignInFailures{}, err
	}
	f := entity.SignInFailures{Count: r.Uint32(), Last: readTime(r), Failed: readTime(r), Nonce: r.Uint64()}
	return f, r.Done()
}
