package vfs

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/internal/vfs/authz"
	"github.com/cubbit/dittofs/internal/vfs/pseudofs"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// profiles is every adapter's profile: each capability test runs its case
// under each, the bit cleared and set, so the rule follows the bit alone.
var profiles = map[string]vfs.Caps{
	"nfsv3": vfs.NFSv3Caps, "nfsv4": vfs.NFSv4Caps, "mount": vfs.MountCaps, "nlm": vfs.NLMCaps, "smb": vfs.SMBCaps,
}

// eachCase runs fn under each profile with x cleared and with it set. A
// profile's HostCall is left out unless x is HostCall: it refuses every call
// but a host's, which would hide the rule under test.
func eachCase(t *testing.T, x vfs.Caps, fn func(t *testing.T, c vfs.Caps, set bool)) {
	for name, p := range profiles {
		if x != vfs.HostCall {
			p &^= vfs.HostCall
		}
		for _, set := range []bool{false, true} {
			c := p &^ x
			if set {
				c |= x
			}
			t.Run(fmt.Sprintf("%s/%v", name, set), func(t *testing.T) { fn(t, c, set) })
		}
	}
}

// HostCall lowers a share's minimums only for the calls a host's mount or
// lock service makes: Root and ListShares meet none, a lock call, a GetAttr
// and an open meet MinProof lowered to Claimed and no MinProtection, and any
// other call
// naming the share, or any call with a Session, which no mount or lock
// service has, is ErrInvalid. Without it the same call meets the share's own
// minimums. The share asks a ticket with integrity; the caller signed in
// with a secret and nothing protected.
func TestHostCallLowersTheMinimumsOnlyForItsCalls(t *testing.T) {
	pastMinimums := errors.New("past the minimums: refused later, not ErrWrongSecurity")
	eachCase(t, vfs.HostCall, func(t *testing.T, caps vfs.Caps, set bool) {
		s := newShares(t, nil, nil)
		ctx := t.Context()
		bob := s.user(t, "bob", "", s.share, entity.AccessReadWrite)
		h, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "f", kvstore.Regular(0o644))
		s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share,
			MinProof: entity.ProofTicket, MinProtection: entity.ProtectionIntegrity}, policyVersion(t, s.cp, s.share))))
		secret := vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofSecret, Evidence: vfs.Evidence{Principal: bob.Principal}, Client: inLab}, Caps: caps}
		id, _, err := s.Connect(ctx, secret, nlmHost("host", 1), &vfstest.Callbacks{})
		s.must(t, err)
		secret.Client = id
		withSession := secret
		withSession.Session = vfs.SessionRef{ID: 1}
		// A call proving nothing resolves to no one, so past the minimums it
		// is refused by resolution, never ErrWrongSecurity.
		none := vfs.Call{Identity: vfs.Identity{Client: inLab}, Client: id, Caps: caps}

		o := vfs.LockOwner{Client: id, Name: []byte("lo")}
		calls := map[string]func(c vfs.Call) error{
			"Root": func(c vfs.Call) error {
				_, _, err := s.Root(ctx, c, vfs.ShareRef{Path: "/data"})
				return err
			},
			"ListShares": func(c vfs.Call) error {
				shares, err := s.ListShares(ctx, c)
				if err == nil && len(shares) == 0 {
					return vfs.ErrWrongSecurity // left out, as its minimums refuse it
				}
				return err
			},
			"TestLock": func(c vfs.Call) error {
				_, err := s.TestLock(ctx, c, vfs.Anonymous(h), o, vfs.ByteRange{Length: 1}, false)
				return err
			},
			"Open": func(c vfs.Call) error {
				_, err := s.Open(ctx, c, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData})
				return err
			},
			"GetAttr": func(c vfs.Call) error {
				_, err := s.GetAttr(ctx, c, h)
				return err
			},
			"SetAttr": func(c vfs.Call) error {
				_, err := s.SetAttr(ctx, c, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o644)})
				return err
			},
		}
		all := func(err error) map[string]error {
			return map[string]error{"Root": err, "ListShares": err, "TestLock": err, "Open": err, "GetAttr": err, "SetAttr": err}
		}
		cases := []struct {
			name string
			c    vfs.Call
			want map[string]error
		}{
			{"at a secret", secret, all(vfs.ErrWrongSecurity)},
			{"with a session", withSession, all(vfs.ErrWrongSecurity)},
		}
		if set {
			cases = []struct {
				name string
				c    vfs.Call
				want map[string]error
			}{
				{"at a secret", secret, map[string]error{"Root": nil, "ListShares": nil, "TestLock": nil, "Open": nil, "GetAttr": nil, "SetAttr": vfs.ErrInvalid}},
				{"with a session", withSession, all(vfs.ErrInvalid)},
				{"proving nothing", none, map[string]error{"Root": pastMinimums, "ListShares": vfs.ErrWrongSecurity, "TestLock": vfs.ErrWrongSecurity, "Open": vfs.ErrWrongSecurity, "GetAttr": vfs.ErrWrongSecurity, "SetAttr": vfs.ErrInvalid}},
			}
		}
		for _, tc := range cases {
			for name, want := range tc.want {
				err := calls[name](tc.c)
				var ok bool
				switch want {
				case nil:
					ok = err == nil
				case pastMinimums:
					ok = err != nil && !errors.Is(err, vfs.ErrWrongSecurity)
				default:
					ok = errors.Is(err, want)
				}
				if !ok {
					t.Errorf("under %q, %s: %s answered %v, want %v", caps, tc.name, name, err, want)
				}
			}
		}
	})
}

// A HostCall open is lowered only when it opens an existing file by handle
// and neither creates, overwrites nor deletes on close: any other HostCall
// open is ErrInvalid before it reaches the file, on a share whose minimums
// the caller does not meet, and the file and the directory are unchanged.
func TestAHostCallOpenThatChangesAFileIsRefused(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	bob := s.user(t, "bob", "", s.share, entity.AccessReadWrite)
	h, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "f", kvstore.Regular(0o666))
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share,
		MinProof: entity.ProofTicket, MinProtection: entity.ProtectionIntegrity}, policyVersion(t, s.cp, s.share))))
	c := vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofSecret, Evidence: vfs.Evidence{Principal: bob.Principal}, Client: inLab}, Caps: vfs.NLMCaps}
	id, _, err := s.Connect(ctx, c, nlmHost("host", 1), &vfstest.Callbacks{})
	s.must(t, err)
	c.Client = id
	rw := entity.ReadData | entity.WriteData
	for name, req := range map[string]vfs.OpenRequest{
		"a create by name":  byName(s.fs.Root, "g", vfs.CreateNew, rw),
		"an open by name":   byName(s.fs.Root, "f", vfs.OpenExisting, rw),
		"an overwrite":      {H: h, Disposition: vfs.Overwrite, Access: rw},
		"a delete on close": {H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.Delete, DeleteOnClose: true},
	} {
		if _, err := s.Open(ctx, c, req); !errors.Is(err, vfs.ErrInvalid) {
			t.Errorf("a HostCall open, %s: %v, want ErrInvalid", name, err)
		}
	}
	if _, _, err := s.Lookup(ctx, proven(s.fs.Admin.Principal, inLab), s.fs.Root, []byte("g")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("after the refused HostCall create: lookup of g %v, want ErrNotFound", err)
	}
	if _, err := s.Open(ctx, c, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: rw}); err != nil {
		t.Errorf("a HostCall open of the existing file by handle: %v, want admitted", err)
	}
}

// PrivilegeOnEveryCall lets an admin's privilege count on every call.
// Without it the privilege counts only on an open with backup intent and on
// calls through the handle it made: an open without intent of a file whose
// DACL denies everyone is refused, so are a SetAttr and an Unlink of it with
// no open, while a SetAttr through the backup handle goes; and the share
// root's maximal access leaves out what only privilege grants.
func TestPrivilegeOnEveryCallDecidesWherePrivilegeCounts(t *testing.T) {
	eachCase(t, vfs.PrivilegeOnEveryCall, func(t *testing.T, caps vfs.Caps, set bool) {
		n := newNames(t)
		ctx := t.Context()
		h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
		// bob owns f, so the admin has no owner's right to its mode.
		bob := n.user(t, "bob", "", n.share, entity.AccessReadWrite)
		_, err := n.fs.Store.Files().SetAttrs(ctx, n.fs.Admin, h, entity.Attrs{}.WithOwner(bob.Principal))
		n.must(t, err)
		deny := entity.ACL{Entries: []entity.ACE{{Type: entity.ACEDeny, Mask: entity.EntryRights, Who: entity.Everyone}}}
		for _, f := range []vfs.Handle{h, n.fs.Root} {
			_, err := n.fs.Store.Files().SetACL(ctx, n.fs.Admin, f, deny)
			n.must(t, err)
		}
		c := n.smbClient(t, "pc", n.fs.Admin.Principal)
		c.Caps = caps
		req := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}
		if _, err := n.Open(ctx, c, req); set != (err == nil) || !set && !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("under %q: an admin's open without intent: %v, want ErrAccess %v", caps, err, !set)
		}
		req.Backup = true
		r, err := n.Open(ctx, c, req)
		if err != nil {
			t.Fatalf("under %q: an admin's backup-intent open: %v", caps, err)
		}
		_, info, err := n.Root(ctx, c, vfs.ShareRef{Path: "/data"})
		if err != nil || (info.MaximalAccess&entity.ReadData != 0) != set {
			t.Errorf("under %q: the root's maximal access %#x, %v; want read data %v", caps, info.MaximalAccess, err, set)
		}
		chmod := vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o600)}
		if _, err := n.SetAttr(ctx, c, vfs.Target{H: h}, chmod); set != (err == nil) || !set && !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("under %q: an admin's SetAttr with no open: %v, want ErrAccess %v", caps, err, !set)
		}
		if _, err := n.SetAttr(ctx, c, vfs.Target{H: h, Open: &r.Ref}, chmod); err != nil {
			t.Errorf("under %q: an admin's SetAttr through its backup-intent handle: %v", caps, err)
		}
		n.must(t, errOf(n.Close(ctx, c, r.Ref, vfs.Seqs{})))
		if _, err := n.Unlink(ctx, c, vfs.Target{H: n.fs.Root}, []byte("f")); set != (err == nil) || !set && !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("under %q: an admin's Unlink with no open: %v, want ErrAccess %v", caps, err, !set)
		}
	})
}

// OpenFilesKeepNames decides whether an open made through a directory keeps
// the directory's name: with it a rename of the directory is refused,
// without it the rename goes.
func TestOpenFilesKeepNamesDecidesTheNameRules(t *testing.T) {
	eachCase(t, vfs.OpenFilesKeepNames, func(t *testing.T, caps vfs.Caps, set bool) {
		n := newNames(t)
		n.writableRoot(t)
		d := n.create(t, n.admin, n.fs.Root, "d", vfs.CreateDir)
		f := n.create(t, n.admin, d.Handle, "f", vfs.CreateRegular)
		holder := n.smbClient(t, "holder", n.fs.Admin.Principal)
		n.open(t, holder, d.Handle, f.Handle, "f", entity.ReadData)
		c := n.admin
		c.Caps = caps
		_, err := n.Rename(t.Context(), c, vfs.Target{H: n.fs.Root}, []byte("d"), n.fs.Root, []byte("e"), false)
		if set != errors.Is(err, vfs.ErrShareViolation) || !set && err != nil {
			t.Errorf("under %q: a rename of a directory an open was made through: %v, want ErrShareViolation %v", caps, err, set)
		}
	})
}

// ClientIsMachine decides whether a client record names an owner: a second
// principal connecting the same name joins a machine's record, and is refused
// one that names its first principal.
func TestClientIsMachineLetsAnotherUserJoin(t *testing.T) {
	eachCase(t, vfs.ClientIsMachine, func(t *testing.T, caps vfs.Caps, set bool) {
		s := newShares(t, nil, nil)
		ctx := t.Context()
		alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
		bob := s.user(t, "bob", "", s.share, entity.AccessReadWrite)
		info := vfs.ClientInfo{Protocol: entity.ProtocolSMB, Name: []byte("host"), Boot: [8]byte{1}, Recovery: entity.RecoverReconnect}
		asAlice, asBob := proven(alice.Principal, inLab), proven(bob.Principal, inLab)
		asAlice.Caps, asBob.Caps = caps, caps
		id, _, err := s.Connect(ctx, asAlice, info, &vfstest.Callbacks{})
		s.must(t, err)
		again, _, err := s.Connect(ctx, asBob, info, &vfstest.Callbacks{})
		if joined := err == nil && again == id; joined != set || !set && !clientInUse(err) {
			t.Errorf("under %q: a second user's Connect answered %d, %v; want to join %d: %v", caps, again, err, id, set)
		}
	})
}

// A client that recovers by reconnecting must have proved who it is to
// connect: a claimed call is ErrInvalid. One that recovers otherwise passes
// that gate, and is refused here only by resolution, which resolves no
// claim in this fixture.
func TestAReconnectingClientMustBeProvedToConnect(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	claimed := vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofClaimed, Claims: vfs.Claims{UID: 1000, GID: 1000}, Client: inLab}, Caps: vfs.NLMCaps}
	for _, tc := range []struct {
		recovery entity.RecoveryKind
		refused  bool
	}{
		{entity.RecoverReconnect, true},
		{entity.RecoverNotice, false},
	} {
		info := vfs.ClientInfo{Protocol: entity.ProtocolNLM, Name: []byte("host"), Boot: [8]byte{1}, Recovery: tc.recovery}
		_, _, err := s.Connect(t.Context(), claimed, info, &vfstest.Callbacks{})
		if tc.refused != errors.Is(err, vfs.ErrInvalid) {
			t.Errorf("recovering by %v: a claimed Connect answered %v, want ErrInvalid %v", tc.recovery, err, tc.refused)
		}
	}
}

// Open hands the call's Caps to open state whole: under WantOpenID an open
// is named by an OpenID, and under GrantBelongsToOpen its grant asked with no
// lease key caches handles.
func TestOpenHandsItsCapsToOpenState(t *testing.T) {
	open := func(t *testing.T, caps vfs.Caps) vfs.OpenResult {
		n := newNames(t)
		h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
		c := n.smbClient(t, "pc", n.fs.Admin.Principal)
		c.Caps = caps
		r, err := n.Open(t.Context(), c, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData,
			Want: openstate.GrantRead | openstate.GrantHandle})
		n.must(t, err)
		return r
	}
	eachCase(t, vfs.WantOpenID, func(t *testing.T, caps vfs.Caps, set bool) {
		if r := open(t, caps); (r.Ref.Open != 0) != set {
			t.Errorf("under %q: the open's ID %d, want one %v", caps, r.Ref.Open, set)
		}
	})
	eachCase(t, vfs.GrantBelongsToOpen, func(t *testing.T, caps vfs.Caps, set bool) {
		if r := open(t, caps); r.Grant == nil || (r.Grant.Kind&openstate.GrantHandle != 0) != set {
			t.Errorf("under %q: grant %+v, want handle caching %v", caps, r.Grant, set)
		}
	})
}

// A rename through the caller's own exclusive open goes, as an SMB client
// renames through one: that open's deny-delete was judged at open. An NFS
// rename of the same file, through no open, is refused against it.
func TestARenameThroughItsOwnExclusiveOpenGoes(t *testing.T) {
	n := newNames(t)
	n.writableRoot(t)
	ctx := t.Context()
	a := n.create(t, n.admin, n.fs.Root, "a", vfs.CreateRegular)
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	who := openstate.Caller{Client: c.Client, Principal: n.fs.Admin.Principal, Caps: c.Caps}
	r, err := n.tab.Opens().Open(ctx, who, keyOf(a.File), openstate.OpenRequest{
		Via: fileOf(n.fs.Root), Name: []byte("a"), Access: entity.ReadData | entity.Delete, Deny: entity.ReadData | entity.WriteData | entity.Delete})
	n.must(t, err)
	ref := vfs.OpenRef{Kind: openstate.RefOpened, State: r.Open.Ref, Open: r.Open.ID, Handle: a.Handle}
	if _, err := n.Rename(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("a"), n.fs.Root, []byte("b"), false); !errors.Is(err, vfs.ErrShareViolation) {
		t.Errorf("an NFS rename of a file another client holds exclusively: %v, want ErrShareViolation", err)
	}
	if _, err := n.Rename(ctx, c, vfs.Target{H: n.fs.Root, Open: &ref}, []byte("a"), n.fs.Root, []byte("b"), false); err != nil {
		t.Errorf("an SMB rename through its own exclusive open: %v, want nil", err)
	}
}

// SeesPseudoFS decides where ".." at a share's root goes: with it, up to the
// pseudo directory the share is mounted in; without it, the root itself.
func TestSeesPseudoFSLeadsUpFromAShareRoot(t *testing.T) {
	eachCase(t, vfs.SeesPseudoFS, func(t *testing.T, caps vfs.Caps, set bool) {
		n := newNames(t)
		c := n.admin
		c.Caps = caps
		h, _, err := n.Lookup(t.Context(), c, n.fs.Root, []byte(".."))
		n.must(t, err)
		if up := pseudofs.IsHandle(h); up != set {
			t.Errorf("under %q: .. at the share's root answered a pseudo directory %v, want %v", caps, up, set)
		}
	})
}

// The protocol label is for observability alone: a context without one is
// admitted and opens as one with it does, and a release run after the call
// that left it, the deferred unlink of a delete on close, is audited with
// the label of that call's context, which the service's derived context
// keeps.
func TestTheProtocolLabelChangesNothingAndReachesAReleasesEvent(t *testing.T) {
	rec := &recorder{}
	n := namesOver(t, newShares(t, rec, func(vfs.ShareID) authz.Policy { return authz.AuditAll }))
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	for _, ctx := range []context.Context{t.Context(), vfs.WithProtocol(t.Context(), entity.ProtocolSMB)} {
		h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
		req := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}
		r, err := n.Open(ctx, c, req)
		if err != nil {
			t.Fatalf("an open, labelled %q: %v", label(ctx), err)
		}
		n.must(t, errOf(n.Close(ctx, c, r.Ref, vfs.Seqs{})))
		_, err = n.Unlink(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("f"))
		n.must(t, err)
	}
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "g", kvstore.Regular(0o644))
	ctx := vfs.WithProtocol(t.Context(), entity.ProtocolSMB)
	req := byName(n.fs.Root, "g", vfs.OpenExisting, entity.ReadData|entity.Delete)
	req.DeleteOnClose, req.Backup = true, true
	r, err := n.Open(ctx, c, req)
	n.must(t, err)
	rec.mu.Lock()
	rec.got = nil
	rec.mu.Unlock()
	n.must(t, errOf(n.Close(ctx, c, r.Ref, vfs.Seqs{})))
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var unlinks int
	for _, e := range rec.got {
		if e.Op == vfs.OpUnlink {
			unlinks++
			if e.Protocol.Label() != "smb" {
				t.Errorf("the deferred unlink's access event names protocol %q, want smb", e.Protocol.Label())
			}
		}
	}
	if unlinks != 1 {
		t.Errorf("the deferred unlink emitted %d access events, want one", unlinks)
	}
}

// label is the word ctx's protocol label is written as.
func label(ctx context.Context) string {
	p, _ := vfs.ProtocolFrom(ctx)
	return p.Label()
}
