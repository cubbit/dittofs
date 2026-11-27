// Package identitymem is the real identity server over a store a test hands
// it, for pkg/vfs/vfstest/memory, which may not import internal/identity
// itself. No other non-test package imports it.
package identitymem

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/sim"
)

// New is the identity server over st, its first State applied from the
// identity settings st holds, started, and stopped when the test ends. Its
// clock is a test clock nothing advances, reading time.Unix(0, 0).
//
// ponytail: it passes no Secrets, so an NTLM SignIn answers ErrUnavailable,
// and New holds no keytab, so a Kerberos one does too. Pass a key provider over
// the test's own key files when a test of memory's NTLM SignIn needs one.
func New(t testing.TB, st *kvstore.Store) *identity.Server {
	t.Helper()
	return WithKeytab(t, st, nil)
}

// WithKeytab is New with keytab, in MIT's keytab format, as the State's only
// keytab version, so its SignIn accepts the tickets of the realm the keytab
// is for, issued for the clock's time. Each attempt's logon event is dropped.
func WithKeytab(t testing.TB, st *kvstore.Store, keytab []byte) *identity.Server {
	t.Helper()
	deps := identity.Deps{Store: st, Clock: sim.NewTestClock(time.Unix(0, 0))}
	if keytab != nil {
		deps.Logons = identity.LogLines(slog.New(slog.DiscardHandler))
	}
	srv, err := identity.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap, _, err := settings.Read(ctx, st.Nodes(), settings.Table{identity.Section()}, entity.NodeID{})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := identity.SettingsOf(snap, entity.NodeID{})
	if err != nil {
		t.Fatal(err)
	}
	state := identity.State{Settings: ids}
	if keytab != nil {
		state.Keytab = []secrets.Version{{Number: 1, Value: keytab}}
	}
	if err := srv.Apply(state); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return srv
}
