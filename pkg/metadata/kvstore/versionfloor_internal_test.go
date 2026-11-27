package kvstore

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// checkFloor asserts RFC 6 §8.3's index over the fixture's share: every
// File has exactly one entry, at its stored Version and in its bucket; every
// unfolded delta has one at its version; a released file not yet pruned has
// one at the File's last Version, which its removal carries; and there is no
// other entry.
func (f *cfix) checkFloor() {
	f.t.Helper()
	if err := f.s.Check(context.Background()); err != nil {
		f.t.Error(err)
	}
}

// The floor is the highest Version any file or delta of the share stores,
// whichever bucket it is in; it follows a removal and a release.
func TestVersionFloor(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	floor := func() entity.JournalVersion {
		v, err := f.s.VersionFloor(ctx, []entity.ShareID{f.share})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	var files []entity.FileID
	for range 40 { // enough to reach most buckets
		files = append(files, f.file(0))
	}
	for i, file := range files {
		f.exist(file, write(uint64(i+1), 0, mib))
	}
	f.checkFloor()
	highest := func() (top entity.JournalVersion) {
		for _, file := range files {
			top = entity.MaxVersion(top, f.get(file).Version)
		}
		return top
	}
	if got, want := floor(), highest(); got.Less(want) {
		t.Errorf("floor %d below the highest Version %d", got.Counter(), want.Counter())
	}
	f.remove(files[3], truncate(100, 0))
	f.checkFloor()
	if got, want := floor(), f.get(files[3]).Version; got.Less(want) {
		t.Errorf("floor %d below a truncate's Version %d", got.Counter(), want.Counter())
	}
	f.unlinked(files[5])
	f.remove(files[5], release(101))
	f.checkFloor()
	f.resume(files[5])
	if err := f.s.Content().PruneRemovals(ctx, f.share, files[5], entity.VersionOf(200)); err != nil {
		t.Fatal(err)
	}
	f.checkFloor()
	if other, err := f.s.VersionFloor(ctx, []entity.ShareID{entity.NewShareID()}); err != nil || !other.IsZero() {
		t.Errorf("an empty share's floor %v %v", other, err)
	}
}

// A share's version limit is only ever raised, and a raise for a share with
// no record changes nothing.
func TestVersionLimitsAreOnlyRaised(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	gone := entity.NewShareID()
	for _, c := range []struct{ raise, want uint64 }{{1 << 20, 1 << 20}, {1 << 10, 1 << 20}, {1 << 21, 1 << 21}} {
		if err := f.s.Lifecycle().RaiseVersionLimits(ctx, map[entity.ShareID]entity.JournalVersion{
			f.share: entity.VersionOf(c.raise),
			gone:    entity.VersionOf(1),
		}); err != nil {
			t.Fatal(err)
		}
		got, err := f.s.Lifecycle().VersionLimits(ctx, []entity.ShareID{f.share, gone})
		if err != nil {
			t.Fatal(err)
		}
		if got[f.share] != entity.VersionOf(c.want) {
			t.Errorf("after raising to %d: limit %d, want %d", c.raise, got[f.share].Counter(), entity.VersionOf(c.want).Counter())
		}
		if !got[gone].IsZero() {
			t.Errorf("a share with no record has limit %d, want zero", got[gone].Counter())
		}
	}
}

// A directory's fold replaces its deltas' version-index entries with the
// directory's own, leaving no stray entry behind.
func TestFoldDirectoryIndex(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	cp := f.s.ControlPlane()
	admin := entity.User{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Name: "admin"}
	if _, err := cp.PutUser(ctx, admin, 0, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.SetGrant(ctx, entity.ShareGrant{Share: f.share, Principal: admin.Principal, Access: entity.AccessAdmin}, 0); err != nil {
		t.Fatal(err)
	}
	id := entity.Identity{Principal: admin.Principal, Privileged: true}
	root, rf, err := f.s.Files().Root(ctx, id, f.share)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, _, err := f.s.Namespace().Create(ctx, id, root, []byte(name), NewFile{Type: entity.Regular, Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
	}
	f.checkFloor()
	if _, _, err := f.s.Namespace().FoldDirectory(ctx, f.share, rf.ID); err != nil {
		t.Fatal(err)
	}
	f.checkFloor()
}
