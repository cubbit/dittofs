package node

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// cancelingView wraps a real settings view and cancels its context after the
// tenth row it lists, then goes on listing, as a store that ignores its
// context does.
type cancelingView struct {
	entity.SettingsReader
	cancel context.CancelFunc
}

func (v cancelingView) AllSettings(ctx context.Context, after entity.SettingKey) iter.Seq2[entity.Setting, error] {
	return func(yield func(entity.Setting, error) bool) {
		i := 0

		for s, err := range v.SettingsReader.AllSettings(ctx, after) {
			if i++; i == 10 {
				v.cancel()
			}

			if !yield(s, err) {
				return
			}
		}
	}
}

// A walk whose context ends while the store goes on listing ends with that
// error, so Read gives no snapshot: a walk cut short is never published as a
// whole one.
func TestSettingsWalkCancelledMidWalkReadsAsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	for i := range 300 {
		s := entity.Setting{
			Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
			Key:   fmt.Sprintf("pad.k%04d", i),
			Value: []byte("v"),
		}
		if _, err := st.ControlPlane().PutSetting(ctx, s, 0); err != nil {
			t.Fatalf("put %s: %v", s.Key, err)
		}
	}

	table := settings.Table{}
	w := &settingsWalk{
		view:  cancelingView{SettingsReader: st.Nodes(), cancel: cancel},
		table: table,
		bound: func() <-chan struct{} { return make(chan struct{}) },
		rows:  map[string][]entity.Setting{},
	}

	snap, _, err := settings.Read(ctx, w, table, entity.NodeID{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Read of a walk cancelled mid-listing: error %v, want context.Canceled", err)
	}

	if snap != nil {
		t.Fatalf("Read of a walk cancelled mid-listing gave a snapshot, want none")
	}
}
