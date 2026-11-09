package vfs

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// State created on a session, presented by a call of its own client and
// principal with none, is StateError NotYours, Sessioned, through an open
// reference and a grant ID alike: an adapter tells it apart from a plain
// NotYours under errors.Is. On the session the same calls are served.
func TestSessionStateWithoutSessionIsNotYoursSessioned(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	s.user(t, "carol", "1002", s.share, entity.AccessReadWrite)
	h, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "f", kvstore.Regular(0o666))
	on := s.nfs41(t, "host", 1002, 1)
	r, err := s.Open(ctx, on, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.WriteData, Want: openstate.GrantRead | openstate.GrantWrite})
	s.must(t, err)
	if r.Grant == nil {
		t.Fatal("no delegation offered")
	}
	bare := on
	bare.Session = vfs.SessionRef{}

	sessioned := &vfs.StateError{Kind: vfs.StateNotYours, Sessioned: true}
	plain := &vfs.StateError{Kind: vfs.StateNotYours}
	for name, err := range map[string]error{
		"a close through the open": func() error { _, err := s.Close(ctx, bare, r.Ref, vfs.Seqs{}); return err }(),
		"a recall answer":          s.AckRecall(ctx, bare, r.Grant.ID, openstate.GrantNone),
	} {
		if !errors.Is(err, sessioned) || errors.Is(err, plain) {
			t.Errorf("%s with no session: %v, want NotYours Sessioned and not plain NotYours", name, err)
		}
	}
	if err := s.AckRecall(ctx, on, r.Grant.ID, openstate.GrantNone); errors.Is(err, vfs.ErrState) {
		t.Errorf("the delegation returned on its session: %v", err)
	}
	if _, err := s.Close(ctx, on, r.Ref, vfs.Seqs{}); err != nil {
		t.Errorf("the open closed on its session: %v", err)
	}
}
