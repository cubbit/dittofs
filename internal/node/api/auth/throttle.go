package auth

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The sign-in throttle's bounds.
const (
	freeAttempts = 5   // a pair's or a source's counted attempts before each further one waits
	freeFailures = 100 // an account's, across every source
	firstWait    = time.Second
	maxWait      = 5 * time.Minute
)

// kind is one of an attempt's three counts.
type kind int

const (
	accountCount kind = iota
	sourceCount
	pairCount
)

// counts is an attempt's three counts as read, indexed by kind; a count the
// attempt has none of, or that is absent, is zero.
type counts [3]entity.SignInFailures

// attempt names the counts one sign-in attempt is counted on: its account,
// zero for a name that resolves to none, and its source, invalid for a
// caller on the local socket.
//
// ponytail: every loopback address is its own /32, so a guesser on the host
// rotating through 127.0.0.0/8 gets a fresh source count, and with it a fresh
// pair, per address; only the account's 100 holds it. Upgrade to counting
// 127.0.0.0/8 as one source when a guesser on the host is in scope, since a
// shell there already reaches the local socket.
type attempt struct {
	account entity.Principal
	source  netip.Prefix
}

// key is the count of kind k the attempt is counted on, and false when it
// has none: no account count or pair for a name that resolves to no
// account, no source count or pair on the local socket.
func (a attempt) key(k kind) (entity.SignInKey, bool) {
	hasAccount, hasSource := a.account != (entity.Principal{}), a.source.IsValid()
	switch k {
	case accountCount:
		return entity.SignInKey{Account: a.account}, hasAccount
	case sourceCount:
		return entity.SignInKey{Source: a.source}, hasSource
	}
	return entity.SignInKey{Account: a.account, Source: a.source}, hasAccount && hasSource
}

// sourceOf is the source a connection from addr is counted on: none for a
// caller on the local socket, entity.SourceKey's for any other.
func sourceOf(addr netip.Addr, local bool) netip.Prefix {
	if local {
		return netip.Prefix{}
	}
	return entity.SourceKey(addr)
}

// expired reports whether a count of kind k holding f is forgotten at now: a
// source's 15 min after its last settled failure, a pair's 24 h after its
// last counted attempt. An account's never expires.
func expired(k kind, f entity.SignInFailures, now time.Time) bool {
	switch k {
	case sourceCount:
		return !now.Before(f.Failed.Add(entity.SourceSignInLife))
	case pairCount:
		return !now.Before(f.Last.Add(entity.PairSignInLife))
	}
	return false
}

// backoff is the wait after a count's last counted attempt once it holds
// over its free ones: 1 s at the first over, doubling, up to 5 min.
func backoff(over uint32) time.Duration {
	if over >= 9 {
		return maxWait
	}
	return min(firstWait<<over, maxWait)
}

// decide is the wait an attempt with counts c owes at now, or, when it owes
// none, the counts its raise writes: each count it has one higher and its
// last attempt at now. An expired count reads as absent; a source count it
// creates takes a last settled failure at now, and no nonce, which the write
// draws. A wait is never more than the backoff itself, so a count whose last
// attempt is ahead of now, as store time is ahead of a node clock stepped
// back, owes no more than the cap.
//
// decision: a name that resolves to no account has no account count and no
// pair, only its source's, and a pair outlives a source (24 h against
// 15 min), so once a source's count expires a second quick attempt on an
// existing name meets that pair's wait while one on an unknown name does not;
// the account count's 100 tells the same from fresh addresses. The difference
// says the name exists. It follows the counts as specified; closing it is a
// change to them, counting an unknown name on a pseudo-account keyed by a MAC
// of the folded name under the installation's secret, and is open.
//
// decision: the fast wait is keyed by account and source together, as
// identity providers' smart lockout separates familiar and unfamiliar
// locations, so a guesser on one address cannot hold an account's owner on
// another. Behind one proxy they are one address, and the next decision says
// what that costs. Only the 100-failure account count reaches across
// sources: a guesser spread over enough addresses to reach it can still hold
// the account, and the break-glass reset clears admin's. Revisit if that is
// reported: the upgrade is a source the account has signed in from before,
// exempt from the account-wide count.
//
// decision: a source is the peer address, so every client behind one proxy,
// NAT or Kubernetes SNAT shares one source count, and one guesser there can
// hold that source's wait at its 5-minute cap by one checked attempt each
// time it expires, for every account signing in through it, and the pair's
// wait of every account it guesses. A proxy on the host makes loopback such a
// source, so loopback is not exempt. The count is forgotten 15 min after its
// last failure rather than 24 h, a 429 does not raise it, a caller on the
// local socket has none and no pair, and the break-glass reset clears it; a
// success from the source does not reset it, since a guesser holding one
// valid account could then reset its own spray count. Revisit when a shared
// source's wait is reported: the upgrade is a forwarding header read only
// from a configured list of trusted proxies.
func decide(a attempt, c counts, now time.Time) (time.Duration, counts) {
	var wait time.Duration
	var raised counts
	for k := range c {
		if _, ok := a.key(kind(k)); !ok {
			continue
		}
		f := c[k]
		if expired(kind(k), f, now) {
			f = entity.SignInFailures{}
		}
		free := uint32(freeAttempts)
		if kind(k) == accountCount {
			free = freeFailures
		}
		if f.Count >= free {
			b := backoff(f.Count - free)
			wait = max(wait, min(f.Last.Add(b).Sub(now), b))
		}
		f.Count++
		f.Last = now
		if kind(k) == sourceCount && f.Failed.IsZero() {
			f.Failed = now
		}
		raised[k] = f
	}
	if wait > 0 {
		return wait, counts{}
	}
	return 0, raised
}

// settle is a failure's settle of the source count f it was counted on under
// nonce: f with its last settled failure at now, and false when f is another
// count than the one the attempt raised, swept or recreated since, or
// expired, so the settle does nothing.
func settle(f entity.SignInFailures, nonce uint64, now time.Time) (entity.SignInFailures, bool) {
	if f.Nonce != nonce || expired(sourceCount, f, now) {
		return f, false
	}
	f.Failed = now
	return f, true
}

// takeBack is a success's takeback of its own raise of the source count f
// it was counted on under nonce: f one lower, neither of its times moved, and
// false on another count than that one, as settle.
func takeBack(f entity.SignInFailures, nonce uint64, now time.Time) (entity.SignInFailures, bool) {
	if f.Nonce != nonce || expired(sourceCount, f, now) || f.Count == 0 {
		return f, false
	}
	f.Count--
	return f, true
}

// throttled is an attempt refused until its wait has passed: the API answers
// it 429 with Retry-After.
type throttled struct{ wait time.Duration }

func (e *throttled) Error() string { return fmt.Sprintf("sign-in throttled for %v", e.wait) }

// RetryAfter is how long the caller waits before the next attempt counts.
func (e *throttled) RetryAfter() time.Duration { return e.wait }

// signInStore is the store the throttle counts on: an untracked read of one
// count, and a write whose reads are tracked.
type signInStore interface {
	SignInFailures(ctx context.Context, k entity.SignInKey) (entity.SignInFailures, bool, error)
	Write(ctx context.Context, meta entity.WriteMeta, fn func(entity.ControlPlaneTx) error) error
}

// clock is the node's clock.
type clock interface{ Now() time.Time }

// counted is an attempt reserve let through, with the nonce of the source
// count it raised, which its settle or takeback applies to.
type counted struct {
	attempt
	nonce uint64
}

// reserve counts an attempt before its password is checked. It decides
// first on the counts read untracked at the node's clock, and an attempt
// that must wait is refused there with nothing written. Only an attempt let
// through enters the tracked write, which decides again on the counts it
// reads at store time and writes their raises, so concurrent attempts past
// the bound are refused there. A write that fails refuses the attempt.
//
// decision: the untracked check runs at the node's clock and the write at
// store time, because an untracked read has no store time to read. Store
// time is the clock held under a ceiling raised CeilingAhead, about 2 s,
// past it, so the two part by up to that much: store time behind lets the
// check pass an attempt at the edge of its wait that the write then refuses,
// an Update that commits nothing; store time ahead, after the clock steps
// back, makes the check's Retry-After that much longer than store time owes.
// Upgrade when the store offers a Now readable outside a write: both
// decisions then take store time.
func reserve(ctx context.Context, st signInStore, clk clock, a attempt) (counted, error) {
	var read counts
	for k := range read {
		key, ok := a.key(kind(k))
		if !ok {
			continue
		}
		f, _, err := st.SignInFailures(ctx, key)
		if err != nil {
			return counted{}, err
		}
		read[k] = f
	}
	if wait, _ := decide(a, read, clk.Now()); wait > 0 {
		return counted{}, &throttled{wait}
	}
	c := counted{attempt: a}
	err := st.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		var read counts
		for k := range read {
			key, ok := a.key(kind(k))
			if !ok {
				continue
			}
			f, _, err := tx.SignInFailures(key)
			if err != nil {
				return err
			}
			read[k] = f
		}
		wait, raised := decide(a, read, tx.Now())
		if wait > 0 {
			return &throttled{wait}
		}
		if raised[sourceCount].Nonce == 0 {
			raised[sourceCount].Nonce = rand.Uint64() | 1
		}
		for k, f := range raised {
			if key, ok := a.key(kind(k)); ok {
				if err := tx.PutSignInFailures(key, f); err != nil {
					return err
				}
			}
		}
		c.nonce = raised[sourceCount].Nonce
		return nil
	})
	if err != nil {
		return counted{}, err
	}
	return c, nil
}

// onSource applies fn to the source count c was counted on, in the
// caller's write at store time, and writes what fn returns when it reports a
// change; an attempt with no source, or a count since swept, is left alone.
func onSource(tx entity.ControlPlaneTx, c counted, fn func(entity.SignInFailures, uint64, time.Time) (entity.SignInFailures, bool)) error {
	key, ok := c.key(sourceCount)
	if !ok {
		return nil
	}
	f, found, err := tx.SignInFailures(key)
	if err != nil || !found {
		return err
	}
	if f, ok := fn(f, c.nonce, tx.Now()); ok {
		return tx.PutSignInFailures(key, f)
	}
	return nil
}

// settleFailure settles a failed attempt in the caller's write: its source
// count's last settled failure moves to store time.
func settleFailure(tx entity.ControlPlaneTx, c counted) error { return onSource(tx, c, settle) }

// takeBackRaise takes back an attempt's raise of its source's count in the
// caller's write, and touches no other count: an attempt the store could not
// answer is neither a failure nor a success.
func takeBackRaise(tx entity.ControlPlaneTx, c counted) error { return onSource(tx, c, takeBack) }

// takeBackSuccess settles a successful attempt in the caller's write: its
// account's count and its pair's are reset, and its raise of its source's is
// taken back.
func takeBackSuccess(tx entity.ControlPlaneTx, c counted) error {
	for _, k := range []kind{accountCount, pairCount} {
		if key, ok := c.key(k); ok {
			if err := tx.DeleteSignInFailures(key); err != nil {
				return err
			}
		}
	}
	return takeBackRaise(tx, c)
}

// ResetAccount resets an account's own sign-in count in the caller's write,
// as :set-password and the break-glass reset do.
func ResetAccount(tx entity.ControlPlaneTx, account entity.Principal) error {
	return tx.DeleteSignInFailures(entity.SignInKey{Account: account})
}
