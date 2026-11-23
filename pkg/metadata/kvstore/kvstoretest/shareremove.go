package kvstoretest

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

func testDeleteShareMark(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	cp := f.s.ControlPlane()
	other := share("other", "/other")
	must(t, errOf(CreateShare(ctx, cp, other)))
	before := counters(t, f.s).ShareList
	if got, err := f.s.ShareIndex().Removing(ctx); err != nil || len(got) != 0 {
		t.Errorf("shares being removed before any removal: %v, %v", got, err)
	}
	must(t, cp.DeleteShare(ctx, f.share.ID, entity.SnapshotsRefuse, 1))
	// The one share being removed is listed, though no name finds it.
	if got, err := f.s.ShareIndex().Removing(ctx); err != nil || len(got) != 1 || got[0] != f.share.ID {
		t.Errorf("shares being removed: %v, %v; want only %v", got, err, f.share.ID)
	}

	if after := counters(t, f.s).ShareList; after != before+1 {
		t.Errorf("ShareList %d -> %d", before, after)
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

// oneRowBatches reports a transaction entry limit that leaves a store's
// batches room for one row each: the five records every batch may write, and
// twelve keys. Every call passes through to the backend beneath.
type oneRowBatches struct{ kv.KV }

func (b oneRowBatches) Limits() kv.TxnLimits {
	l := b.KV.Limits()
	l.TxnEntries = 5 + 12
	return l
}

// A share's removal runs to its end in batches of one row each, the store
// reopened over its data before every batch, as a restart between batches
// leaves it.
func testShareRemovalAcrossReopens(t *testing.T, d Durable) {
	open := d(t, newClock().Now)
	reopen := func() *kvstore.Store {
		s, err := kvstore.Open(ctx, oneRowBatches{open()})
		must(t, err)
		return s
	}
	db := open()
	must(t, kvstore.Init(ctx, db, uuid.New()))
	s := reopen()
	cp := s.ControlPlane()
	sh := share("gone", "/gone")
	must(t, errOf(CreateShare(ctx, cp, sh)))
	rows := 0
	for _, p := range []entity.Principal{entity.AuthenticatedUsers, entity.Everyone} {
		must(t, errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: p, Access: entity.AccessRead}, 0)))
		rows++
	}
	for _, k := range []string{"a", "b", "c", "d"} {
		must(t, errOf(cp.PutSetting(ctx, entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeShare, ID: sh.ID}, Key: k, Value: []byte{1}}, 0)))
		rows++
	}
	got, _, err := s.ShareIndex().Share(ctx, sh.ID)
	must(t, err)
	must(t, cp.DeleteShare(ctx, sh.ID, entity.SnapshotsDelete, got.Generation))

	atFive := 0
	for done, batches := false, 0; !done; batches++ {
		if batches > 100 {
			t.Fatal("the removal has not finished after 100 batches")
		}
		var step uint8
		step, done, err = reopen().Nodes().ContinueRemoval(ctx, sh.ID)
		must(t, err)
		if step == 5 {
			atFive++
		}
		if reached, ok := stepTags[step]; ok {
			must(t, reopen().Nodes().RecordShardRemoval(ctx, sh.ID, got.Shard, reached))
		}
		for shardDone := step != 4; !shardDone; {
			shardDone, err = reopen().Nodes().ContinueShardRemoval(ctx, sh.ID, got.Shard)
			must(t, err)
		}
	}
	if atFive < rows {
		t.Errorf("batches at step 5: got %d, want one per grant and setting, %d", atFive, rows)
	}
	s = reopen()
	if _, _, err := s.ShareIndex().Share(ctx, sh.ID); !merrs.IsNotFoundError(err) {
		t.Errorf("the removed share after a reopen: %v, want not found", err)
	}
	if err := errOf(s.ControlPlane().PutShare(ctx, sh, 0)); !errors.Is(err, entity.ErrStale) {
		t.Errorf("PutShare naming the removed share's ID: %v, want %v", err, entity.ErrStale)
	}
	if err := s.Check(ctx); err != nil {
		t.Errorf("the store after the removal: %v", err)
	}
}
