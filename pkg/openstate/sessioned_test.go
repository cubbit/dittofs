package openstate

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// State created on a session, an open, its lock state and its delegation, is
// refused ErrNotYoursSessioned to a call of its own client with no session,
// whatever its principal, through every way of naming it: an open reference,
// a bare state reference, a grant ID and a lock owner. The same calls on a
// session, and on state created with none, are answered as before.
func TestSessionStateRefusedWithoutSession(t *testing.T) {
	for _, onSession := range []bool{true, false} {
		f := newFixture(t)
		owner_ := entity.NewUser()
		id := f.connect(t, nfs("host", 1, owner_))
		maker := Caller{Client: id, Principal: entity.NewUser(), Sessioned: onSession}
		var s entity.SessionRef
		if onSession {
			s = entity.SessionRef{NodeEpoch: 1, ID: 1}
		}
		file := FileKey{Shard: 1, ID: entity.NewFileID()}
		o, g := f.grantOpen(t, maker, file, OpenRequest{Access: rd, Via: entity.NewFileID(), Want: GrantRead, Session: s})
		if g == nil {
			t.Fatal("no delegation granted")
		}
		lo := owner(maker, "lo")
		testLock := func(who Caller) error {
			_, err := f.Locks().TestLock(ctx, who, file, lo, br(20, 1), true)
			return err
		}
		ls := f.mustLock(t, maker, file, LockReq{Owner: lo, Open: refOf(o), Ranges: []LockRange{x(0, 10)}, Seqs: Seqs{NewOwner: 1}})

		// The opener, its client's owner and another principal of the client.
		for _, p := range []entity.Principal{maker.Principal, owner_, entity.NewUser()} {
			bare := Caller{Client: id, Principal: p}
			calls := map[string]error{
				"I/O through the open":       f.Checks().CheckIO(ctx, bare, file, refOf(o), br(0, 1), entity.ReadData),
				"I/O through the lock state": f.Checks().CheckIO(ctx, bare, file, OpenRef{Kind: RefLocked, State: ls.Ref, Handle: handle(file.ID)}, br(0, 1), entity.ReadData),
				"I/O through the delegation": f.Checks().CheckIO(ctx, bare, file, delegated(g, file), br(0, 1), entity.ReadData),
				"a state reference":          f.Recovery().TestState(ctx, bare, []StateRef{o.Ref})[0],
				"a grant ID":                 f.ack(bare, g, GrantNone),
				"a lock test by owner":       testLock(bare),
				"an owner release":           f.Locks().ReleaseLockOwner(ctx, bare, lo),
			}
			for name, err := range calls {
				switch {
				case onSession && !errors.Is(err, ErrNotYoursSessioned):
					t.Errorf("%s, created on a session, by %v with none: %v, want ErrNotYoursSessioned", name, p, err)
				case !onSession && errors.Is(err, ErrNotYoursSessioned):
					t.Errorf("%s, created with no session, by %v with none: %v", name, p, err)
				}
			}
		}

		// The opener on a session: the open, its lock state, the test by
		// owner and the delegation are its own.
		on := maker
		on.Sessioned = true
		if err := f.Checks().CheckIO(ctx, on, file, refOf(o), br(0, 1), entity.ReadData); err != nil {
			t.Errorf("I/O through the open on a session: %v", err)
		}
		if err := f.Recovery().TestState(ctx, on, []StateRef{o.Ref, ls.Ref})[0]; err != nil {
			t.Errorf("a state reference on a session: %v", err)
		}
		if err := testLock(on); err != nil {
			t.Errorf("a lock test by owner on a session: %v", err)
		}
		if err := f.ack(on, g, GrantNone); errors.Is(err, ErrNotYours) {
			t.Errorf("a return of the delegation on a session: %v", err)
		}
	}
}

// ErrNotYoursSessioned is an ErrNotYours, so a caller testing only the
// latter still refuses it.
func TestNotYoursSessionedIsNotYours(t *testing.T) {
	if !errors.Is(ErrNotYoursSessioned, ErrNotYours) {
		t.Error("ErrNotYoursSessioned does not wrap ErrNotYours")
	}
}

// An open named by its ID, as SMB names one, is refused the same way.
func TestSessionOpenByIDRefusedWithoutSession(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc"))
	file := anyFile()
	o := f.mustOpen(t, a, file, OpenRequest{Access: rd, Session: entity.SessionRef{NodeEpoch: 1, ID: 1}})
	byID := OpenRef{Kind: RefOpened, Open: o.ID, Handle: handle(file.ID)}
	bare := a
	bare.Sessioned = false
	if err := f.Checks().CheckIO(ctx, bare, file, byID, br(0, 1), entity.ReadData); !errors.Is(err, ErrNotYoursSessioned) {
		t.Errorf("I/O through the open by ID with no session: %v, want ErrNotYoursSessioned", err)
	}
	if err := f.Checks().CheckIO(ctx, a, file, byID, br(0, 1), entity.ReadData); err != nil {
		t.Errorf("I/O through the open by ID on a session: %v", err)
	}
}
