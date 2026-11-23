package node

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
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

// paddedStore is a real store holding n installation settings of no section,
// which the walk lists and no section claims.
func paddedStore(t *testing.T, n int) *kvstore.Store {
	t.Helper()

	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if err := kvstore.Init(t.Context(), db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	st, err := kvstore.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}

	for i := range n {
		s := entity.Setting{
			Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
			Key:   fmt.Sprintf("pad.k%04d", i),
			Value: []byte("v"),
		}
		if _, err := st.ControlPlane().PutSetting(t.Context(), s, 0); err != nil {
			t.Fatalf("put %s: %v", s.Key, err)
		}
	}

	return st
}

// A walk arms its bound once per page the store reads, 256 rows each, never
// once per row: 1000 rows are four reads, at 0, 256, 512 and 768 rows, and no
// fifth, since the last read holds 232 rows, fewer than a page, which ends the
// listing.
func TestSettingsWalkArmsItsBoundOncePerPage(t *testing.T) {
	st := paddedStore(t, 1000)

	var waits []int

	w := &settingsWalk{
		view:    st.Nodes(),
		bound:   func() <-chan struct{} { return make(chan struct{}) },
		waiting: func(seen int) { waits = append(waits, seen) },
		rows:    map[string][]entity.Setting{},
	}

	if _, _, err := settings.Read(t.Context(), w, settings.Table{}, entity.NodeID{}); err != nil {
		t.Fatalf("Read of a walk of 1000 rows: %v", err)
	}

	if want := []int{0, 256, 512, 768}; !slices.Equal(waits, want) {
		t.Fatalf("waits armed after %d rows (%d in all), want %v", waits, len(waits), want)
	}
}

// heldView wraps a real settings view and holds its listing at row at until
// the context ends, as a store stuck reading a later page.
type heldView struct {
	entity.SettingsReader
	at int
}

func (v heldView) AllSettings(
	ctx context.Context,
	after entity.SettingKey,
) iter.Seq2[entity.Setting, error] {
	return func(yield func(entity.Setting, error) bool) {
		i := 0

		for s, err := range v.SettingsReader.AllSettings(ctx, after) {
			if i == v.at {
				<-ctx.Done()

				return
			}

			i++

			if !yield(s, err) {
				return
			}
		}
	}
}

// A walk that times out names the rows it had read, so its error tells it
// apart from the counters read's, which has the same bound.
func TestSettingsWalkTimeoutNamesTheRowsRead(t *testing.T) {
	st := paddedStore(t, 300)
	arms := 0

	w := &settingsWalk{
		view: heldView{SettingsReader: st.Nodes(), at: 256},
		bound: func() <-chan struct{} {
			arms++
			if arms == 1 {
				return make(chan struct{})
			}

			fired := make(chan struct{})
			close(fired)

			return fired
		},
		rows: map[string][]entity.Setting{},
	}

	var got error

	for _, err := range w.AllSettings(t.Context(), entity.SettingKey{}) {
		if err != nil {
			got = err
		}
	}

	want := "the store did not answer within 20s, after 256 rows"
	if got == nil || got.Error() != want {
		t.Fatalf("the walk held at its second page: error %v, want %q", got, want)
	}
}
