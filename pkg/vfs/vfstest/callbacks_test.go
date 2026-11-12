package vfstest_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// CallbackFaults passes every callback to Inner, which keeps it as sent
// and picks its session, and then answers in Inner's place what Fail
// answers, and a lock grant refused when Refuse is set.
func TestCallbackFaultsFailOnCue(t *testing.T) {
	ctx := context.Background()
	failed := errors.New("no answer")
	alice := entity.NewUser()
	gone := vfs.SessionRef{ID: 9}
	var inner vfstest.Callbacks
	inner.SignIn(vfs.SessionRef{ID: 3}, alice)
	cb := &vfstest.CallbackFaults{
		Inner: &inner,
		Fail: func(_ context.Context, c vfstest.Callback) error {
			if c.Method == "RecallAny" || c.Method == "RecallGrant" && c.Args[0] == gone {
				return failed
			}
			return nil
		},
		Refuse: true,
	}
	g := vfs.CachingGrant{}
	w := vfs.LockWaiter{Wait: vfs.WaitCallback}
	if refused, err := cb.LockWaitOver(ctx, w, true); !refused || err != nil {
		t.Errorf("LockWaitOver = %v, %v; want refused, nil", refused, err)
	}
	if err := cb.RecallAny(ctx, 2); err != failed {
		t.Errorf("RecallAny = %v, want %v", err, failed)
	}
	if sent, err := cb.RecallGrant(ctx, gone, nil, g, vfs.GrantNone); sent || err != failed {
		t.Errorf("a recall on the failing session = %v, %v; want unsent, %v", sent, err, failed)
	}
	if sent, err := cb.RecallGrant(ctx, vfs.SessionRef{}, []vfs.Principal{alice}, g, vfs.GrantNone); !sent || err != nil {
		t.Errorf("a recall to alice's sessions = %v, %v; want sent, nil", sent, err)
	}
	if err := cb.Notify(ctx, vfs.WatchID{}, nil); err != nil {
		t.Errorf("Notify = %v, want nil", err)
	}
	want := []vfstest.Callback{
		{Method: "LockWaitOver", Args: []any{w, true}},
		{Method: "RecallAny", Args: []any{2}},
		{Method: "RecallGrant", Args: []any{gone, []vfs.Principal(nil), g, vfs.GrantNone}, Session: gone},
		{Method: "RecallGrant", Args: []any{vfs.SessionRef{}, []vfs.Principal{alice}, g, vfs.GrantNone}, Session: vfs.SessionRef{ID: 3}},
		{Method: "Notify", Args: []any{vfs.WatchID{}, []vfs.Change(nil)}},
	}
	if got := inner.Delivered(); !reflect.DeepEqual(got, want) {
		t.Errorf("Inner kept\n%+v\nwant\n%+v", got, want)
	}
}

// Callbacks keeps every callback delivered, in order, each answered as a
// client that is there and accepts, and sends a recall naming openers on the
// lowest-numbered session signed in as one of them, or on none.
func TestCallbacksKeepsWhatWasDelivered(t *testing.T) {
	ctx := context.Background()
	alice, bob, stranger := entity.NewUser(), entity.NewUser(), entity.NewUser()
	var cb vfstest.Callbacks
	cb.SignIn(vfs.SessionRef{ID: 5}, alice)
	cb.SignIn(vfs.SessionRef{ID: 3}, alice)
	cb.SignIn(vfs.SessionRef{ID: 2}, alice)
	cb.SignIn(vfs.SessionRef{ID: 1}, bob)
	cb.SignOff(vfs.SessionRef{ID: 2})
	g := vfs.CachingGrant{}
	w := vfs.LockWaiter{Wait: vfs.WaitCallback}
	changes := []vfs.Change{{}}
	watch := vfs.WatchID{Token: 4}
	for _, c := range []struct {
		name string
		call func() (bool, error)
		sent bool
	}{
		{"a recall on a named session", func() (bool, error) {
			return cb.RecallGrant(ctx, vfs.SessionRef{ID: 7}, nil, g, vfs.GrantNone)
		}, true},
		{"a recall to alice's sessions", func() (bool, error) {
			return cb.RecallGrant(ctx, vfs.SessionRef{}, []vfs.Principal{alice}, g, vfs.GrantNone)
		}, true},
		{"a recall to no session's principal", func() (bool, error) {
			return cb.RecallGrant(ctx, vfs.SessionRef{}, []vfs.Principal{stranger}, g, vfs.GrantNone)
		}, false},
		{"Notify", func() (bool, error) { return true, cb.Notify(ctx, watch, changes) }, true},
		{"Revoked", func() (bool, error) { cb.Revoked(ctx, vfs.Revocation{}); return true, nil }, true},
		{"LockWaitOver", func() (bool, error) {
			refused, err := cb.LockWaitOver(ctx, w, true)
			return !refused, err
		}, true},
		{"RecallAny", func() (bool, error) { return true, cb.RecallAny(ctx, 2) }, true},
	} {
		if sent, err := c.call(); sent != c.sent || err != nil {
			t.Errorf("%s: sent (or accepted) %v, %v; want %v, nil", c.name, sent, err, c.sent)
		}
	}
	want := []vfstest.Callback{
		{Method: "RecallGrant", Args: []any{vfs.SessionRef{ID: 7}, []vfs.Principal(nil), g, vfs.GrantNone}, Session: vfs.SessionRef{ID: 7}},
		{Method: "RecallGrant", Args: []any{vfs.SessionRef{}, []vfs.Principal{alice}, g, vfs.GrantNone}, Session: vfs.SessionRef{ID: 3}},
		{Method: "RecallGrant", Args: []any{vfs.SessionRef{}, []vfs.Principal{stranger}, g, vfs.GrantNone}},
		{Method: "Notify", Args: []any{watch, changes}},
		{Method: "Revoked", Args: []any{vfs.Revocation{}}},
		{Method: "LockWaitOver", Args: []any{w, true}},
		{Method: "RecallAny", Args: []any{2}},
	}
	if got := cb.Delivered(); !reflect.DeepEqual(got, want) {
		t.Errorf("Delivered =\n%+v\nwant\n%+v", got, want)
	}
}
