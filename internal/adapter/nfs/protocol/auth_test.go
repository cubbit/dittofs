package protocol

import (
	"math/rand/v2"
	"net/netip"
	"reflect"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// addr is the client address every translation here is given.
var addr = netip.MustParseAddr("192.0.2.7")

// claimed is the Identity an AUTH_SYS credential of these ids gives, at addr
// (RFC 21 section 11.1's table).
func claimed(uid, gid uint32, gids ...uint32) vfs.Identity {
	return vfs.Identity{
		Proof:  vfs.ProofClaimed,
		Claims: vfs.Claims{UID: uid, GID: gid, GIDs: gids},
		Client: addr,
	}
}

// sameIdentity compares two Identities, a group list by its members: no list
// and an empty one are the same claim.
func sameIdentity(a, b vfs.Identity) bool {
	if len(a.Claims.GIDs) == 0 {
		a.Claims.GIDs = nil
	}

	if len(b.Claims.GIDs) == 0 {
		b.Claims.GIDs = nil
	}

	return reflect.DeepEqual(a, b)
}

// Every program and procedure RFC 21 Appendix D lists states the profile RFC
// 17 section 3.0 gives it: NFSv3's and MOUNT's others privilege alone, MNT and
// EXPORT and NLM and NSM the host call besides, NFSv4 the pseudo-filesystem. A
// procedure added to a table later is added here, so none takes a profile by
// default.
func TestCallCapsCoversAppendixD(t *testing.T) {
	const (
		nfs, mount, nlm, nsm = 100003, 100005, 100021, 100024
		v3, v4               = vfs.NFSv3Caps, vfs.NFSv4Caps
		host                 = vfs.MountCaps
		nlmCaps              = vfs.NLMCaps
	)

	type call struct {
		prog, vers uint32
		procs      []uint32
		want       vfs.Caps
	}
	for _, tc := range []call{
		// NFSv3's 22 procedures, NULL to COMMIT (RFC 1813 section 3).
		{nfs, 3, []uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, v3},
		{nfs, 3, []uint32{11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21}, v3},
		// NFSv4's NULL and COMPOUND (RFC 8881 section 16).
		{nfs, 4, []uint32{0, 1}, v4},
		// MOUNT's NULL, DUMP, UMNT and UMNTALL, then MNT and EXPORT (RFC 1813 Appendix I).
		{mount, 3, []uint32{0, 2, 3, 4}, v3},
		{mount, 3, []uint32{1, 5}, host},
		// NLM's NULL, TEST, LOCK, CANCEL, UNLOCK, GRANTED, the _MSG and _RES forms,
		// SHARE, UNSHARE and FREE_ALL.
		{nlm, 4, []uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, nlmCaps},
		{nlm, 4, []uint32{20, 21, 23}, nlmCaps},
		// NSM's NULL, MON, UNMON and NOTIFY.
		{nsm, 1, []uint32{0, 2, 3, 6}, nlmCaps},
	} {
		for _, proc := range tc.procs {
			got := callCaps(tc.prog, tc.vers, proc)
			if got != tc.want {
				t.Errorf("program %d version %d procedure %d states %q, want %q",
					tc.prog, tc.vers, proc, got, tc.want)
			}

			if !got.Has(vfs.PrivilegeOnEveryCall) {
				t.Errorf(
					"program %d procedure %d lacks the privilege every call states",
					tc.prog,
					proc,
				)
			}

			isHost := tc.prog == mount && (proc == 1 || proc == 5) || tc.prog == nlm ||
				tc.prog == nsm
			if got.Has(vfs.HostCall) != isHost {
				t.Errorf("program %d procedure %d: host call is %v, want %v",
					tc.prog, proc, got.Has(vfs.HostCall), isHost)
			}
		}
	}

	if got := callCaps(1, 1, 1); got != 0 {
		t.Errorf("a program no table serves states %q, want the narrowest profile", got)
	}
}

// A program no table serves has no label, as it states the narrowest profile.
func TestAProgramNoTableServesHasNoLabel(t *testing.T) {
	if got := callProtocol(1, 1); got != 0 {
		t.Errorf("a program no table serves carries the label %v, want none", got)
	}
}

// Each AUTH_SYS credential of the golden table gives its golden Identity,
// whatever its stamp and machine name; uid 0 stays the claim 0 and AUTH_NONE
// claims nothing.
func TestTheCredentialsGoldenIdentity(t *testing.T) {
	for _, tc := range []struct {
		file string
		want vfs.Identity
	}{
		{"rfc-cred-sys-bare", claimed(1000, 1000, 10, 20, 30)},
		{"rfc-cred-sys-named", claimed(1000, 1000, 10, 20, 30)},
		{"rfc-cred-sys-renamed", claimed(1000, 1000, 10, 20, 30)},
		{"rfc-cred-sys-root", claimed(0, 0)},
		{"rfc-cred-sys-16-groups", claimed(1000, 1000,
			1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16)},
	} {
		sys, err := wire.DecodeAuthSys(golden(t, tc.file))
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}

		var gids [16]uint32

		got, err := identity(wire.AuthSys, &sys, addr, &gids)
		if err != nil || !sameIdentity(got, tc.want) {
			t.Errorf("%s gives %+v, %v, want %+v", tc.file, got, err, tc.want)
		}
	}

	var gids [16]uint32

	want := vfs.Identity{Client: addr}
	if got, err := identity(wire.AuthNone, nil, addr, &gids); err != nil ||
		!sameIdentity(got, want) {
		t.Errorf("AUTH_NONE gives %+v, %v, want %+v", got, err, want)
	}
}

// identity is total: from any flavour and any credential it answers an
// Identity or a refusal, never a default. The oracle: a flavour but AUTH_NONE
// and AUTH_SYS, or AUTH_SYS with no credential, is refused with the zero
// Identity; AUTH_NONE gives the anonymous Identity whatever credential it is
// handed, and AUTH_SYS the claims of the one it is (RFC 21 section 11.6).
func TestIdentityIsTotal(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))

	flavours := []uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 0xffffffff}
	for range 200 {
		flavours = append(flavours, rng.Uint32())
	}

	for _, flavour := range flavours {
		for _, withSys := range []bool{false, true} {
			var sys *wire.AuthSysParms
			if withSys {
				sys = &wire.AuthSysParms{
					Stamp: rng.Uint32(), UID: rng.Uint32(), GID: rng.Uint32(), NGIDs: rng.IntN(17),
				}
				for i := range sys.GIDs {
					sys.GIDs[i] = rng.Uint32()
				}
			}

			var gids [16]uint32

			got, err := identity(flavour, sys, addr, &gids)
			switch {
			case flavour != wire.AuthNone && flavour != wire.AuthSys,
				flavour == wire.AuthSys && sys == nil:
				if err == nil || !reflect.DeepEqual(got, vfs.Identity{}) {
					t.Errorf(
						"flavour %d, credential %v gives %+v, %v, want a refusal, zero",
						flavour,
						withSys,
						got,
						err,
					)
				}
			case flavour == wire.AuthNone:
				if want := (vfs.Identity{Client: addr}); err != nil || !sameIdentity(got, want) {
					t.Errorf(
						"AUTH_NONE, credential %v gives %+v, %v, want %+v",
						withSys,
						got,
						err,
						want,
					)
				}
			default:
				want := claimed(sys.UID, sys.GID, sys.GIDs[:sys.NGIDs]...)
				if err != nil || !sameIdentity(got, want) {
					t.Errorf("AUTH_SYS gives %+v, %v, want %+v", got, err, want)
				}
			}
		}
	}
}
