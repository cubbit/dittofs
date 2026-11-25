package openstate

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/caps"
)

// profiles is every adapter's profile: each capability test runs its case
// under each, the bit cleared and set, so the rule follows the bit alone.
var profiles = map[string]caps.Caps{
	"nfsv3": caps.NFSv3Caps, "nfsv4": caps.NFSv4Caps, "mount": caps.MountCaps, "nlm": caps.NLMCaps, "smb": caps.SMBCaps,
}

// eachCase runs fn under each profile with x cleared and with it set.
func eachCase(t *testing.T, x caps.Caps, fn func(t *testing.T, c caps.Caps, set bool)) {
	for name, p := range profiles {
		for _, set := range []bool{false, true} {
			c := p &^ x
			if set {
				c |= x
			}
			t.Run(fmt.Sprintf("%s/%v", name, set), func(t *testing.T) { fn(t, c, set) })
		}
	}
}

// WantOpenID decides whether an open is issued an OpenID, and only an open
// issued one may ask durability.
func TestWantOpenIDNamesTheOpenByItsID(t *testing.T) {
	eachCase(t, caps.WantOpenID, func(t *testing.T, c caps.Caps, set bool) {
		f := newFixture(t)
		a := f.caller(t, smb("pc-a"))
		a.Caps = c
		res, err := f.Opens().Open(ctx, a, anyFile(), OpenRequest{Access: rd})
		if err != nil || (res.Open.ID != 0) != set {
			t.Errorf("an open under %q: ID %d, %v; want an ID %v", c, res.Open.ID, err, set)
		}
		r := keep(OpenRequest{Access: rd}, entity.DurabilityDurable, s1, time.Minute, 1)
		if _, err := f.Opens().Open(ctx, a, anyFile(), r); set != (err == nil) || !set && !errors.Is(err, ErrInvalid) {
			t.Errorf("a durable open under %q: %v; want ErrInvalid %v", c, err, !set)
		}
	})
}

// GrantBelongsToOpen decides whether a grant asked with no lease key is an
// oplock, which caches handles, is narrowed by its own client's other opens
// and is broken by its next one, or a delegation, which caches no handle and
// counts every open of its client as its own.
func TestGrantBelongsToOpenMakesAnOplock(t *testing.T) {
	eachCase(t, caps.GrantBelongsToOpen, func(t *testing.T, c caps.Caps, set bool) {
		kind, beside, second := rw, rw, DelayCause(0)
		if set {
			kind, beside, second = rwh, GrantNone, DelayRecall
		}
		f := newFixture(t)
		a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
		a.Caps = c
		file := anyFile()
		res, err := f.Opens().Open(ctx, a, file, OpenRequest{Access: rd, Want: rwh})
		if err != nil || res.Grant == nil || res.Grant.Kind != kind {
			t.Fatalf("under %q: grant %+v, %v; want kind %v", c, res.Grant, err, kind)
		}
		_, err = f.Opens().Open(ctx, a, file, OpenRequest{Access: rd | wr})
		if got := delayedFor(err); got != second || second == 0 && err != nil {
			t.Errorf("under %q: the client's second open: %v; want delay %v", c, err, second)
		}
		other := anyFile()
		f.mustOpen(t, a, other, OpenRequest{Access: rd | wr})
		res, err = f.Opens().Open(ctx, a, other, OpenRequest{Access: rd, Want: rwh})
		got := GrantNone
		if res.Grant != nil {
			got = res.Grant.Kind
		}
		if err != nil || got != beside {
			t.Errorf("under %q, beside the client's own write open: grant %+v, %v; want kind %v", c, res.Grant, err, beside)
		}
	})
}

// OpenFilesKeepNames decides whether a directory an open was made through
// keeps its name: a rename of it is refused while the bit is set.
func TestOpenFilesKeepNamesRefusesARenameOfAnOpenDirectory(t *testing.T) {
	eachCase(t, caps.OpenFilesKeepNames, func(t *testing.T, c caps.Caps, set bool) {
		f := newFixture(t)
		s := f.caller(t, smb("alice-pc"))
		d := entity.NewFileID()
		f.mustOpen(t, s, anyFile(), OpenRequest{Via: d, Access: rd})
		who := Caller{Client: s.Client, Principal: s.Principal, Caps: c}
		err := f.Checks().CheckChange(ctx, who, FileKey{ID: d}, nil, ChangeRename)
		if set != errors.Is(err, ErrShareViolation) || !set && err != nil {
			t.Errorf("a rename under %q of a directory with an open child: %v; want ErrShareViolation %v", c, err, set)
		}
	})
}

// A lease key is the caller's to ask: an open naming one is granted a lease
// under it whatever else the caller states.
func TestALeaseKeyIsGrantedToAnyCallerAskingOne(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	res, err := f.Opens().Open(ctx, a, anyFile(), OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rwh})
	if err != nil || res.Grant == nil || res.Grant.Key != (LeaseKey{1}) {
		t.Errorf("an open naming a lease key: %+v, %v; want a lease under it", res.Grant, err)
	}
}

// A client's recovery kind, never its record's kind, decides whether it
// holds a lease: one told of a loss by a status lapses after a silent lease
// period, one that reconnects does not.
func TestOnlyAClientThatRenewsHoldsALease(t *testing.T) {
	f := newFixture(t)
	renews := f.connect(t, Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolNLM, Name: []byte("r"), Boot: [8]byte{1}, Recovery: entity.RecoverStatus}})
	silent := f.connect(t, Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolNFSv4, Name: []byte("s"), Boot: [8]byte{1}, Recovery: entity.RecoverReconnect}})
	f.clock.advance(leasePeriod + time.Second)
	if _, err := f.Renew(ctx, renews); !errors.Is(err, ErrStaleClient) {
		t.Errorf("a client recovering by status, silent past its lease: %v, want ErrStaleClient", err)
	}
	if _, err := f.Renew(ctx, silent); err != nil {
		t.Errorf("a client recovering by reconnect, silent as long: %v, want nil", err)
	}
}
