package vfstest

import (
	"context"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// Callback is one callback as Callbacks or CallbackFaults saw it: the
// method's name and its arguments, the context left out. Session is, for a
// RecallGrant, the session it was sent on: the one named, or the one picked
// from the sessions signed in; zero for one naming no session and no
// openers, or one sent nowhere.
type Callback struct {
	Method  string
	Args    []any
	Session vfs.SessionRef
}

// Callbacks is a client's callback path that keeps every callback the
// service delivered, its output, for a test to assert on, and answers each
// as a client that is there and accepts: no error, no grant refused. A
// RecallGrant naming no session but openers goes, as an adapter sends it, on
// the lowest-numbered session SignIn recorded as one of openers, or on none,
// answering unsent. A client that fails, stalls or refuses is a fault
// wrapped around it.
type Callbacks struct {
	mu        sync.Mutex
	delivered []Callback
	sessions  map[vfs.SessionRef]vfs.Principal
}

var _ vfs.Callbacks = (*Callbacks)(nil)

// SignIn records session s of the client as signed in now as p, replacing
// what it was signed in as before: a re-authentication, a guest's included,
// which signs in as the zero principal.
func (cb *Callbacks) SignIn(s vfs.SessionRef, p vfs.Principal) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.sessions == nil {
		cb.sessions = map[vfs.SessionRef]vfs.Principal{}
	}
	cb.sessions[s] = p
}

// SignOff forgets session s.
func (cb *Callbacks) SignOff(s vfs.SessionRef) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	delete(cb.sessions, s)
}

// Delivered is every callback so far, in the order delivered.
func (cb *Callbacks) Delivered() []Callback {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return append([]Callback(nil), cb.delivered...)
}

func (cb *Callbacks) keep(c Callback) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.delivered = append(cb.delivered, c)
}

// RecallGrant keeps the recall with the session it goes on: s, or with s
// zero and openers given, the one picked, none leaving it unsent.
func (cb *Callbacks) RecallGrant(_ context.Context, s vfs.SessionRef, openers []vfs.Principal, g vfs.CachingGrant, to vfs.GrantKind) (bool, error) {
	on, picks := s, s == (vfs.SessionRef{}) && len(openers) > 0
	if picks {
		on = cb.pick(openers)
	}
	cb.keep(Callback{Method: "RecallGrant", Args: []any{s, openers, g, to}, Session: on})
	return !picks || on != (vfs.SessionRef{}), nil
}

// pick is the lowest-numbered session signed in as one of openers, or zero.
func (cb *Callbacks) pick(openers []vfs.Principal) vfs.SessionRef {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	var got vfs.SessionRef
	for s, p := range cb.sessions {
		if !p.IsZero() && slices.Contains(openers, p) && (got == vfs.SessionRef{} || s.ID < got.ID) {
			got = s
		}
	}
	return got
}

func (cb *Callbacks) Notify(_ context.Context, w vfs.WatchID, changes []vfs.Change) error {
	cb.keep(Callback{Method: "Notify", Args: []any{w, changes}})
	return nil
}

func (cb *Callbacks) Revoked(_ context.Context, what vfs.Revocation) {
	cb.keep(Callback{Method: "Revoked", Args: []any{what}})
}

func (cb *Callbacks) LockWaitOver(_ context.Context, w vfs.LockWaiter, granted bool) (bool, error) {
	cb.keep(Callback{Method: "LockWaitOver", Args: []any{w, granted}})
	return false, nil
}

func (cb *Callbacks) RecallAny(_ context.Context, keep int) error {
	cb.keep(Callback{Method: "RecallAny", Args: []any{keep}})
	return nil
}

// CallbackFaults is a client's callback path that fails on cue around Inner,
// the client's real path. Each callback reaches Inner first, so Inner keeps
// it as sent; Fail, when set, then answers in Inner's place, as a client that
// got the callback and failed it, stalled until ctx ended, or whose answer
// was lost. Fail sees the callback without the session Inner picked.
// Refuse is LockWaitOver's answer that the client refused a grant. Both are
// set before the path is registered.
type CallbackFaults struct {
	Inner  vfs.Callbacks
	Fail   func(ctx context.Context, c Callback) error
	Refuse bool
}

var _ vfs.Callbacks = (*CallbackFaults)(nil)

// fail is Fail's answer to c, nil when Fail is not set.
func (f *CallbackFaults) fail(ctx context.Context, method string, args ...any) error {
	if f.Fail == nil {
		return nil
	}
	return f.Fail(ctx, Callback{Method: method, Args: args})
}

// RecallGrant answers unsent when the fault fails it.
func (f *CallbackFaults) RecallGrant(ctx context.Context, s vfs.SessionRef, openers []vfs.Principal, g vfs.CachingGrant, to vfs.GrantKind) (bool, error) {
	sent, err := f.Inner.RecallGrant(ctx, s, openers, g, to)
	if ferr := f.fail(ctx, "RecallGrant", s, openers, g, to); ferr != nil {
		return false, ferr
	}
	return sent, err
}

func (f *CallbackFaults) Notify(ctx context.Context, w vfs.WatchID, changes []vfs.Change) error {
	err := f.Inner.Notify(ctx, w, changes)
	if ferr := f.fail(ctx, "Notify", w, changes); ferr != nil {
		return ferr
	}
	return err
}

// Revoked has no answer to give, so Fail's error is dropped; a stall still
// holds the call.
func (f *CallbackFaults) Revoked(ctx context.Context, what vfs.Revocation) {
	f.Inner.Revoked(ctx, what)
	_ = f.fail(ctx, "Revoked", what)
}

func (f *CallbackFaults) LockWaitOver(ctx context.Context, w vfs.LockWaiter, granted bool) (bool, error) {
	refused, err := f.Inner.LockWaitOver(ctx, w, granted)
	if ferr := f.fail(ctx, "LockWaitOver", w, granted); ferr != nil {
		return f.Refuse, ferr
	}
	return refused || f.Refuse, err
}

func (f *CallbackFaults) RecallAny(ctx context.Context, keep int) error {
	err := f.Inner.RecallAny(ctx, keep)
	if ferr := f.fail(ctx, "RecallAny", keep); ferr != nil {
		return ferr
	}
	return err
}
