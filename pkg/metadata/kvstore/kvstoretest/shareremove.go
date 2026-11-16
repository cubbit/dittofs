package kvstoretest

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
)

func testDeleteShareMark(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	cp := f.s.ControlPlane()
	other := share("other", "/other")
	must(t, errOf(CreateShare(ctx, cp, other)))
	before, _ := cp.ShareList(ctx)
	if got, err := f.s.ShareIndex().Removing(ctx); err != nil || len(got) != 0 {
		t.Errorf("shares being removed before any removal: %v, %v", got, err)
	}
	must(t, cp.DeleteShare(ctx, f.share.ID, entity.SnapshotsRefuse, 1))
	// The one share being removed is listed, though no name finds it.
	if got, err := f.s.ShareIndex().Removing(ctx); err != nil || len(got) != 1 || got[0] != f.share.ID {
		t.Errorf("shares being removed: %v, %v; want only %v", got, err, f.share.ID)
	}

	if after, _ := cp.ShareList(ctx); after.Version != before.Version+1 {
		t.Errorf("ShareList %d -> %d", before.Version, after.Version)
	}
	if got := all(t, cp.Shares(ctx)); len(got) != 1 || got[0].ID != other.ID {
		t.Errorf("a share being removed is listed: %+v", got)
	}
	// The name and path are free at once.
	must(t, errOf(CreateShare(ctx, cp, share("data", "/data"))))

	// Removal is one-way, and the share is stale to every caller.
	for name, err := range map[string]error{
		"second delete": cp.DeleteShare(ctx, f.share.ID, entity.SnapshotsDelete, 0),
		"state change":  cp.SetShareState(ctx, f.share.ID, entity.ShareRemoving, entity.ShareEnabled),
		"rename":        errOf(cp.PutShare(ctx, f.share, 0)),
		"grant":         errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: f.admin.Principal, Access: entity.AccessRead}, 0)),
		"policy":        errOf(cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: f.share.ID}, 0)),
		"root":          errOf2(f.s.Files().Root(ctx, f.admin, f.share.ID)),
	} {
		if !errors.Is(err, entity.ErrStale) {
			t.Errorf("%s: %v, want stale", name, err)
		}
	}
	if err := cp.DeleteShare(ctx, entity.NewShareID(), entity.SnapshotsRefuse, 0); !merrs.IsNotFoundError(err) {
		t.Errorf("deleting a missing share: %v", err)
	}
	if err := cp.SetShareState(ctx, f.share.ID, entity.ShareEnabled, entity.ShareRemoving); err == nil {
		t.Error("SetShareState started a removal")
	}
}

func errOf2(_ entity.Handle, _ entity.File, err error) error { return err }

// The mark and a rename of the same share, in both commit orders: whichever
// commits second sees the other, so a removed share is never renamed back
// into the index.
func testDeleteShareRacesRename(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		for _, deleteHeld := range []bool{true, false} {
			name := b.name + "/rename commits first"
			if !deleteHeld {
				name = b.name + "/delete commits first"
			}
			t.Run(name, func(t *testing.T) {
				f, h := newHeldFS(t, b, entity.FoldIdentity)
				cp := f.s.ControlPlane()
				renamed := f.share
				renamed.Name = "renamed"
				del := func() error { return cp.DeleteShare(ctx, f.share.ID, entity.SnapshotsRefuse, 1) }
				ren := func() error { return errOf(cp.PutShare(ctx, renamed, 1)) }
				var delErr, renErr error
				if deleteHeld {
					// The mark read the share before the rename: it retries,
					// finds the rename's generation, and is refused, so the
					// caller reads the share again before removing it.
					delErr, renErr = race(h, del, ren)
					must(t, renErr)
					if !errors.Is(delErr, entity.ErrGenerationMismatch) {
						t.Fatalf("removal from a read before the rename: %v, want %v", delErr, entity.ErrGenerationMismatch)
					}
					must(t, cp.DeleteShare(ctx, f.share.ID, entity.SnapshotsRefuse, 2))
				} else {
					renErr, delErr = race(h, ren, del)
					must(t, delErr)
					if !errors.Is(renErr, entity.ErrStale) {
						t.Fatalf("rename after the removal: %v", renErr)
					}
				}
				if got := all(t, cp.Shares(ctx)); len(got) != 0 {
					t.Fatalf("index after both: %+v", got)
				}
				// Both names are free.
				must(t, errOf(CreateShare(ctx, cp, share("data", "/x"))))
				must(t, errOf(CreateShare(ctx, cp, share("renamed", "/y"))))
			})
		}
	}
}
