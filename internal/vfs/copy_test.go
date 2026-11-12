package vfs_test

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// copier is a filesystem over memory with two unprivileged users, alice and
// bob, each granted read and write on its one share.
type copier struct {
	*memory.Service
	ctx        context.Context
	alice, bob vfs.Call
}

var copyUIDs atomic.Uint32

func newCopier(t *testing.T) *copier {
	t.Helper()
	s := memory.New(t)
	user := func(name string) vfs.Call {
		cp := s.Store.Store.ControlPlane()
		u := entity.User{Principal: entity.NewUser(), Name: name, IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: strconv.Itoa(int(61000 + copyUIDs.Add(1)))}}}
		if err := cp.PutUser(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		if err := cp.SetGrant(t.Context(), entity.ShareGrant{Share: s.Store.Share.ID, Principal: u.Principal, Access: entity.AccessReadWrite}); err != nil {
			t.Fatal(err)
		}
		return vfs.Call{Identity: vfs.Identity{
			Proof: vfs.ProofTicket, Protection: vfs.ProtectionPrivacy,
			Evidence: vfs.Evidence{Principal: u.Principal}, Client: netip.AddrFrom4([4]byte{127, 0, 0, 1}),
		}}
	}
	return &copier{Service: s, ctx: t.Context(), alice: user("alice"), bob: user("bob")}
}

// file makes name in the root, owned by owner's caller with mode, holding p
// as the administrator wrote it.
func (s *copier) file(t *testing.T, name string, owner vfs.Call, mode uint32, p string) vfs.Handle {
	t.Helper()
	ctx := t.Context()
	h, _ := s.Store.Create(t, s.Store.Admin, s.Store.Root, name, kvstore.Regular(0o644))
	files := s.Store.Store.Files()
	if _, err := files.SetAttrs(ctx, s.Store.Admin, h, entity.Attrs{}.WithOwner(owner.Identity.Evidence.Principal)); err != nil {
		t.Fatal(err)
	}
	if _, err := files.SetAttrs(ctx, s.Store.Admin, h, entity.Attrs{}.WithMode(mode)); err != nil {
		t.Fatal(err)
	}
	if p != "" {
		if _, err := s.Write(ctx, s.Admin(), vfs.Anonymous(h), 0, []byte(p), vfs.Unstable); err != nil {
			t.Fatalf("Write of %s: %v", name, err)
		}
	}
	return h
}

// state is h's content and mode, as the administrator reads them.
func (s *copier) state(t *testing.T, h vfs.Handle) (string, uint32) {
	t.Helper()
	f, err := s.GetAttr(t.Context(), s.Admin(), h)
	if err != nil {
		t.Fatalf("GetAttr: %v", err)
	}
	buf := make([]byte, 64)
	n, _, err := s.Read(t.Context(), s.Admin(), vfs.Anonymous(h), 0, buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return string(buf[:n]), f.Mode
}

// copyOrClone is one call of either.
type copyOrClone func(s *copier, c vfs.Call, a vfs.CopyArgs) (int64, vfs.WCC, error)

var calls = map[string]copyOrClone{
	"Copy": func(s *copier, c vfs.Call, a vfs.CopyArgs) (int64, vfs.WCC, error) {
		r, err := s.Copy(s.ctx, c, a)
		return r.N, r.WCC, err
	},
	"Clone": func(s *copier, c vfs.Call, a vfs.CopyArgs) (int64, vfs.WCC, error) {
		w, err := s.Clone(s.ctx, c, a)
		return 0, w, err
	},
}

func args(src, dst vfs.Handle, xs ...vfs.Extent) vfs.CopyArgs {
	return vfs.CopyArgs{Src: vfs.Anonymous(src), Dst: vfs.Anonymous(dst), Extents: xs}
}

// A copy and a clone land the source's bytes in the destination, growing it,
// and answer its WCC from before to after; a copy answers the bytes copied
// across its extents, in order.
func TestACopyOrCloneLandsTheSourceBytes(t *testing.T) {
	s := newCopier(t)
	ctx := t.Context()
	src := s.file(t, "src", s.alice, 0o644, "abcdefgh")
	dst := s.file(t, "dst", s.alice, 0o644, "0123")
	r, err := s.Copy(ctx, s.alice, args(src, dst, vfs.Extent{SrcOff: 0, DstOff: 2, Len: 4}, vfs.Extent{SrcOff: 6, DstOff: 6, Len: 2}))
	if err != nil || r.N != 6 {
		t.Fatalf("Copy = %d, %v; want 6, nil", r.N, err)
	}
	if r.WCC.Before == nil || r.WCC.Before.Size != 4 || r.WCC.After.Size != 8 {
		t.Errorf("Copy's WCC: Before %+v, After size %d; want sizes 4 then 8", r.WCC.Before, r.WCC.After.Size)
	}
	if got, _ := s.state(t, dst); got != "01abcdgh" {
		t.Errorf("after the copy the destination reads %q, want %q", got, "01abcdgh")
	}
	w, err := s.Clone(ctx, s.alice, args(src, dst, vfs.Extent{SrcOff: 4, DstOff: 8, Len: 4}))
	if err != nil || w.Before == nil || w.Before.Size != 8 || w.After.Size != 12 {
		t.Fatalf("Clone = %+v, %v; want sizes 8 then 12", w, err)
	}
	if got, _ := s.state(t, dst); got != "01abcdghefgh" {
		t.Errorf("after the clone the destination reads %q, want %q", got, "01abcdghefgh")
	}
}

// A copy or clone refused before the engine runs changes nothing: neither
// the destination's bytes nor its setuid and setgid bits, which an
// unprivileged caller's copy or clone would clear.
func TestARefusedCopyOrCloneChangesNothing(t *testing.T) {
	x := func(src, dst, n int64) vfs.Extent { return vfs.Extent{SrcOff: src, DstOff: dst, Len: n} }
	for _, c := range []struct {
		name string
		call string
		one  bool // source and destination are one file
		xs   []vfs.Extent
		hide bool // the source is alice's 0600 file, which bob cannot read
		want error
	}{
		{"a clone of two extents", "Clone", false, []vfs.Extent{x(0, 0, 2), x(2, 2, 2)}, false, vfs.ErrInvalid},
		{"a clone of none", "Clone", false, nil, false, vfs.ErrInvalid},
		{"an empty extent", "Copy", false, []vfs.Extent{x(0, 0, 2), x(2, 2, 0)}, false, vfs.ErrInvalid},
		{"a negative offset", "Clone", false, []vfs.Extent{x(-1, 0, 2)}, false, vfs.ErrInvalid},
		{"a copy past the source's size", "Copy", false, []vfs.Extent{x(0, 0, 2), x(4, 4, 8)}, false, vfs.ErrInvalid},
		{"a clone past the source's size", "Clone", false, []vfs.Extent{x(6, 0, 4)}, false, vfs.ErrInvalid},
		{"an overlapping copy within one file", "Copy", true, []vfs.Extent{x(0, 2, 4)}, false, vfs.ErrInvalid},
		{"an overlapping clone within one file", "Clone", true, []vfs.Extent{x(2, 0, 4)}, false, vfs.ErrInvalid},
		{"no read on the source", "Copy", false, []vfs.Extent{x(0, 0, 2)}, true, vfs.ErrAccess},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newCopier(t)
			// bob writes the destination, alice's setuid file.
			dst := s.file(t, "dst", s.alice, 0o6777, "01234567")
			src := dst
			if !c.one {
				mode := uint32(0o644)
				if c.hide {
					mode = 0o600
				}
				src = s.file(t, "src", s.alice, mode, "abcdefgh")
			}
			if _, _, err := calls[c.call](s, s.bob, args(src, dst, c.xs...)); !errors.Is(err, c.want) {
				t.Errorf("%s: %v, want %v", c.call, err, c.want)
			}
			if got, mode := s.state(t, dst); got != "01234567" || mode != 0o6777 {
				t.Errorf("after the refusal the destination reads %q mode %o, want %q mode 6777", got, mode, "01234567")
			}
		})
	}
}

// V22: an unprivileged caller's copy or clone into a setuid and setgid file,
// the owner's included, clears both bits; a privileged caller's keeps them.
// A copy of no extent changes nothing, as a write of no byte does.
func TestACopyOrCloneClearsSetID(t *testing.T) {
	for name, call := range calls {
		for _, c := range []struct {
			who  string
			want uint32
		}{{"bob", 0o0777}, {"alice", 0o0777}, {"admin", 0o6777}} {
			t.Run(name+" by "+c.who, func(t *testing.T) {
				s := newCopier(t)
				who := map[string]vfs.Call{"bob": s.bob, "alice": s.alice, "admin": s.Admin()}[c.who]
				src := s.file(t, "src", s.alice, 0o644, "abcd")
				dst := s.file(t, "dst", s.alice, 0o6777, "")
				if _, _, err := call(s, who, args(src, dst, vfs.Extent{SrcOff: 0, DstOff: 0, Len: 4})); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if got, mode := s.state(t, dst); got != "abcd" || mode != c.want {
					t.Errorf("the destination reads %q mode %o, want %q mode %o", got, mode, "abcd", c.want)
				}
			})
		}
	}
	s := newCopier(t)
	src := s.file(t, "src", s.alice, 0o644, "abcd")
	dst := s.file(t, "dst", s.alice, 0o6777, "xy")
	r, err := s.Copy(s.ctx, s.bob, args(src, dst))
	if err != nil || r.N != 0 || r.WCC.Before == nil || r.WCC.Before.Mode != r.WCC.After.Mode || r.WCC.Before.Size != r.WCC.After.Size || r.WCC.Before.Modify != r.WCC.After.Modify {
		t.Errorf("a copy of no extent = %+v, %v; want 0 and an unchanged WCC", r, err)
	}
	if got, mode := s.state(t, dst); got != "xy" || mode != 0o6777 {
		t.Errorf("after a copy of no extent the destination reads %q mode %o, want %q mode 6777", got, mode, "xy")
	}
}

// A copy or clone needs write on the destination, and a read-only one
// refuses it before anything changes.
func TestACopyOrCloneNeedsWriteOnTheDestination(t *testing.T) {
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			s := newCopier(t)
			src := s.file(t, "src", s.bob, 0o644, "abcd")
			dst := s.file(t, "dst", s.alice, 0o644, "0123")
			if _, _, err := call(s, s.bob, args(src, dst, vfs.Extent{SrcOff: 0, DstOff: 0, Len: 4})); !errors.Is(err, vfs.ErrAccess) {
				t.Errorf("bob's %s into alice's 0644 file: %v, want ErrAccess", name, err)
			}
			if got, _ := s.state(t, dst); got != "0123" {
				t.Errorf("after the refusal the destination reads %q, want %q", got, "0123")
			}
		})
	}
}

// What a copy or clone lands past the destination's size is reserved
// against its owner's quota before anything changes, and refused ErrQuota
// past a hard limit, the destination unchanged.
func TestACopyOrClonePastAQuotaIsRefused(t *testing.T) {
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			s := newCopier(t)
			src := s.file(t, "src", s.alice, 0o644, "abcdefgh")
			dst := s.file(t, "dst", s.alice, 0o6777, "")
			// alice is charged the source's 8 bytes once it commits, so 12
			// leaves her room for 4 bytes more.
			if err := s.Store.Store.ControlPlane().SetQuota(s.ctx, s.Store.Share.ID, entity.Quota{Principal: s.alice.Identity.Evidence.Principal, Hard: entity.Limit{Bytes: 12}}); err != nil {
				t.Fatal(err)
			}
			if err := s.Content.CommitExistence(s.ctx, s.Store.Share.ID, fileID(t, s, src)); err != nil {
				t.Fatal(err)
			}
			_, _, err := call(s, s.bob, args(src, dst, vfs.Extent{SrcOff: 0, DstOff: 0, Len: 8}))
			var qe *vfs.QuotaError
			if !errors.As(err, &qe) || qe.Scope != vfs.QuotaUser {
				t.Errorf("a %s of 8 bytes with 4 left: %v, want ErrQuota of the user", name, err)
			}
			if got, mode := s.state(t, dst); got != "" || mode != 0o6777 {
				t.Errorf("after the refusal the destination reads %q mode %o, want empty, mode 6777", got, mode)
			}
			if _, _, err := call(s, s.bob, args(src, dst, vfs.Extent{SrcOff: 0, DstOff: 0, Len: 4})); err != nil {
				t.Errorf("a %s of the 4 bytes left: %v", name, err)
			}
			// Once those 4 bytes commit, alice is charged 12 and holds no
			// reservation, so a limit of 13 admits a write of one byte more.
			if err := s.Content.CommitExistence(s.ctx, s.Store.Share.ID, fileID(t, s, dst)); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.Store.ControlPlane().SetQuota(s.ctx, s.Store.Share.ID, entity.Quota{Principal: s.alice.Identity.Evidence.Principal, Hard: entity.Limit{Bytes: 13}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Write(s.ctx, s.Admin(), vfs.Anonymous(dst), 4, []byte("x"), vfs.Unstable); err != nil {
				t.Errorf("a write of the one byte left after the %s committed: %v, want it admitted: the %s's reservation was not released", name, err, name)
			}
		})
	}
}

func fileID(t *testing.T, s *copier, h vfs.Handle) entity.FileID {
	t.Helper()
	f, err := s.Store.Store.Files().Get(t.Context(), s.Store.Admin, h)
	if err != nil {
		t.Fatal(err)
	}
	return f.ID
}
