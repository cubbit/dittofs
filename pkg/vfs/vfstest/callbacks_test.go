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

func TestRecordingCallbacksAnswerAsSet(t *testing.T) {
	ctx := context.Background()
	failed := errors.New("no answer")
	cb := &vfstest.RecordingCallbacks{
		Answer: func(_ context.Context, c vfstest.Callback) error {
			if c.Method == "RecallAny" {
				return failed
			}
			return nil
		},
		Refuse: true,
	}
	w := vfs.LockWaiter{Wait: vfs.WaitCallback}
	if refused, err := cb.LockWaitOver(ctx, w, true); !refused || err != nil {
		t.Fatalf("LockWaitOver = %v, %v; want refused, nil", refused, err)
	}
	if err := cb.RecallAny(ctx, 2); err != failed {
		t.Fatalf("RecallAny = %v, want %v", err, failed)
	}
	want := []vfstest.Callback{{Method: "LockWaitOver", Args: []any{w, true}}, {Method: "RecallAny", Args: []any{2}}}
	if got := cb.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Calls = %v, want %v", got, want)
	}
}

// Callbacks keeps every callback delivered, in order, each answered as a
// client that is there and accepts, and sends a recall naming openers on the
// lowest-numbered session signed in as one of them, or on none.
func TestCallbacksKeepsWhatWasDelivered(t *testing.T) {
	ctx := context.Background()
	alice, bob, stranger := entity.NewUser(), entity.NewUser(), entity.NewUser()
	var cb vfstest.Callbacks
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
