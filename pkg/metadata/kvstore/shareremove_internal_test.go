package kvstore

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

func TestDeleteShareGates(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		rows  [][]byte
		disp  entity.SnapshotDisposition
		allow bool
	}{
		{"snapshots, refuse", [][]byte{{keys.SSnap, 1}}, entity.SnapshotsRefuse, false},
		{"snapshots, delete", [][]byte{{keys.SSnap, 1}}, entity.SnapshotsDelete, true},
		{"snapshots, detach", [][]byte{{keys.SSnap, 1}}, entity.SnapshotsDetach, true},
		{"use record, delete", [][]byte{{keys.SSnap, 1}, {keys.SUse, 1}}, entity.SnapshotsDelete, false},
		{"use record, detach", [][]byte{{keys.SSnap, 1}, {keys.SUse, 1}}, entity.SnapshotsDetach, true},
		{"re-home running", [][]byte{{keys.SRh}}, entity.SnapshotsDelete, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openStore(t)
			share := newShare(t, s)
			var kvs [][]byte
			for _, sub := range c.rows {
				kvs = append(kvs, append(shareKey(share, sub[0]), sub[1:]...), []byte{1})
			}
			rows(t, s, kvs...)
			err := s.ControlPlane().DeleteShare(ctx, share, c.disp, 1)
			if (err == nil) != c.allow {
				t.Fatalf("DeleteShare = %v, want allowed %v", err, c.allow)
			}
		})
	}
}

// A move pausing one of the share's namespaces refuses the removal.
func TestDeleteSharePausedNamespace(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	share := newShare(t, s)
	var nss shareNamespaces
	if err := s.kv.View(ctx, func(r kv.Reader) error {
		b, err := r.Get(namespacesKey(share))
		if err != nil {
			return err
		}
		nss, err = decodeNamespaces(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(nss.Namespaces) != 1 || nss.Write != 1 || nss.Namespaces[0].Namespace.IsZero() {
		t.Fatalf("a new share's namespaces: %+v", nss)
	}
	rows(t, s, gcPauseKey(nss.Namespaces[0].Namespace), []byte{1})
	if err := s.ControlPlane().DeleteShare(ctx, share, entity.SnapshotsDelete, 0); !errors.Is(err, ErrMoving) {
		t.Fatalf("DeleteShare under a pause: %v", err)
	}
}

// RemovalStep reads the step ContinueRemoval last answered, at every step of
// a removal run to its end, and refuses a share not being removed.
func TestRemovalStepReadsTheRemovalsRecord(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	share := newShare(t, s)
	nodes := s.Nodes()
	if _, _, err := nodes.RemovalStep(ctx, share); !isInvalidArgument(err) {
		t.Fatalf("RemovalStep of a live share: %v, want an invalid argument", err)
	}
	if err := s.ControlPlane().DeleteShare(ctx, share, entity.SnapshotsDelete, 1); err != nil {
		t.Fatal(err)
	}
	sh, _, err := s.ShareIndex().Share(ctx, share)
	if err != nil {
		t.Fatal(err)
	}
	want := uint8(1)
	for done := false; !done; {
		got, gotDone, err := nodes.RemovalStep(ctx, share)
		if err != nil || got != want || gotDone {
			t.Fatalf("RemovalStep: got step %d, done %v, %v; want step %d, not done", got, gotDone, err, want)
		}
		if want, done, err = nodes.ContinueRemoval(ctx, share); err != nil {
			t.Fatal(err)
		}
		if reached, ok := map[uint8]entity.TagState{3: entity.TagOffloaded, 4: entity.TagForgotten}[want]; ok {
			if err := nodes.RecordShardRemoval(ctx, share, sh.Shard, reached); err != nil {
				t.Fatal(err)
			}
		}
		for shardDone := want != 4; !shardDone; {
			if shardDone, err = nodes.ContinueShardRemoval(ctx, share, sh.Shard); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, done, err := nodes.RemovalStep(ctx, share); err != nil || !done {
		t.Errorf("RemovalStep once the removal ended: done %v, %v; want done", done, err)
	}
}

// A removal's state record naming a pass no removal reaches is refused as
// malformed, as ContinueRemoval refuses it, rather than read past the table.
func TestRemovalStepRefusesAPassPastTheLast(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	share := newShare(t, s)
	for _, pass := range []uint8{0, stepLast + 1, 255} {
		err := s.kv.Update(ctx, func(tx kv.Txn) error {
			return tx.Set(shareKey(share, keys.SSt), encodeState(shareState{State: entity.ShareRemoving, Step: pass}))
		})
		if err != nil {
			t.Fatalf("write a removal at pass %d: %v", pass, err)
		}
		if _, _, err := s.Nodes().RemovalStep(ctx, share); !errors.Is(err, errMalformed) {
			t.Errorf("RemovalStep at pass %d: %v, want %v", pass, err, errMalformed)
		}
	}
}

func isInvalidArgument(err error) bool {
	se, ok := errors.AsType[*merrs.StoreError](err)
	return ok && se.Code == merrs.ErrInvalidArgument
}
