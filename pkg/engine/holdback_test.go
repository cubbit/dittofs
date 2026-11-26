package engine_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// startHanding starts the rig's engine at restartAt with share one, share two
// and extra, and fails the test if Start does.
func (r *rig) startHanding(t *testing.T, extra engine.Share) {
	t.Helper()
	r.atRestart()
	if err := r.e.Start(t.Context(), []engine.Share{r.one, r.two, extra}); err != nil {
		t.Fatalf("Start = %v, want nil: a refused share stops no other", err)
	}
}

// heldBackFor checks that Stats names share id refused, with ErrUnavailable
// naming the refused share other, that a write on it answers
// ErrDelay(DelayFrozen), and that share two, which no clone ties, is served.
func (r *rig) heldBackFor(t *testing.T, id, other entity.ShareID) {
	t.Helper()
	got := r.e.Stats().Shares[id].Refused
	if !errors.Is(got, vfserr.ErrUnavailable) || !strings.Contains(fmtErr(got), other.String()) {
		t.Errorf("share %v refused with %v, want ErrUnavailable naming the refused share %v", id, got, other)
	}
	if err := r.starter.write(t, id); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("a write on held-back share %v = %v, want ErrDelay(DelayFrozen)", id, err)
	}
	if err := r.starter.write(t, r.two.ID); err != nil {
		t.Errorf("a write on share two, which no clone ties, = %v, want it served", err)
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// withBadSettings is sh with a setting Bind refuses.
func withBadSettings(sh engine.Share) engine.Share {
	sh.Settings.MaxAge = 0
	return sh
}

// A clone removal into share one whose source share Start refused holds share
// one back alone, instead of failing Start: the clone cannot be rebuilt
// without its source.
func TestADestinationOfACloneFromARefusedShareIsHeldBack(t *testing.T) {
	r := newRig(t)
	src := r.sibling(t, "source")
	r.cloneInto(t, src, r.newFile2(t, "dst"), entity.VersionOf(7))

	r.startHanding(t, withBadSettings(src))

	r.heldBackFor(t, r.one.ID, src.ID)
	r.wantPosted(t, 0)
}

// The source of a clone into a share Start refused is held back too, so no
// write reaches the cloned range before the clone is done: whether the clone
// is a removal the store holds or a marker the journal holds.
func TestTheSourceOfACloneIntoARefusedShareIsHeldBack(t *testing.T) {
	for _, c := range []struct {
		name  string
		clone func(r *rig, dst engine.Share, f, src entity.FileID)
	}{
		{"from its removal", func(r *rig, dst engine.Share, f, src entity.FileID) {
			spec := entity.CloneSpec{SrcShare: r.one.ID, Src: src, Len: 4096, AsOf: entity.VersionOf(1)}
			op := kvstore.CloneOp{Share: dst.ID, File: f, Spec: spec, Version: entity.VersionOf(7), Time: committed}
			if err := r.fs.Store.Existence().Clone(t.Context(), op, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"from its journal marker", func(r *rig, dst engine.Share, f, src entity.FileID) {
			spec := journal.CloneSpec{SrcShare: journal.ShareID(r.one.ID), Src: journal.NewFileID(src), Len: 4096}
			if _, err := r.handle(t, dst.Tag).CloneTarget(t.Context(), journal.NewFileID(f), spec); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			dst := r.sibling(t, "destination")
			src := r.fileIn(t, r.one, "src")
			r.commitSize(t, r.one.ID, src, 8192)
			c.clone(r, dst, r.fileIn(t, dst, "dst"), src)

			r.startHanding(t, withBadSettings(dst))

			r.heldBackFor(t, r.one.ID, dst.ID)
		})
	}
}
