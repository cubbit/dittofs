// Package identitymem is the real identity server over a store a test hands
// it, for pkg/vfs/vfstest/memory, which may not import internal/identity
// itself. No other non-test package imports it.
package identitymem

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/sim"
)

// New is the identity server over st, its first State applied from the
// identity settings st holds, started, and stopped when the test ends. Its
// clock is a test clock nothing advances.
//
// ponytail: it passes no Secrets or Logons, so its SignIn answers
// ErrUnavailable. Pass a key provider over the test's own key files and
// LogLines when a test of memory's SignIn needs one.
func New(t testing.TB, st *kvstore.Store) *identity.Server {
	t.Helper()
	srv, err := identity.New(identity.Deps{Store: st, Clock: sim.NewTestClock(time.Unix(0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap, _, err := settings.Read(ctx, st.Nodes(), settings.Table{identity.Section()}, entity.NodeID{})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := settings.Get[identity.Settings](snap, identity.SectionName, entity.NodeID{})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Apply(identity.State{Settings: ids}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return srv
}
