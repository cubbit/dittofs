package identitymem_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/identitymem"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
)

// New answers a started server over the store it is handed, which resolves
// a user seeded there, and stops it when the test ends.
func TestNewServesTheStoreUntilTheTestEnds(t *testing.T) {
	fs := kvstoretest.NewFS(t, memKV)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice"}
	if _, err := fs.Store.ControlPlane().PutUser(t.Context(), alice, 0, entity.Alloc{}); err != nil {
		t.Fatalf("PutUser: %v", err)
	}
	var srv *identity.Server
	t.Run("serving", func(t *testing.T) {
		srv = identitymem.New(t, fs.Store)
		id := pid.Identity{Proof: pid.ProofTicket, Evidence: pid.Evidence{Principal: alice.Principal, Name: alice.Name}}
		c, err := srv.Resolve(t.Context(), id, entity.SharePolicy{}, pid.ReadOnlyNotNarrowed)
		if err != nil || c.Mapped != alice.Principal {
			t.Fatalf("Resolve of the seeded alice = %+v, %v; want mapped to %v", c, err, alice.Principal)
		}
		if srv.Current() == 0 {
			t.Error("Current while serving = 0, want the serial of an applied State")
		}
	})
	if got := srv.Current(); got != 0 {
		t.Errorf("Current once the test that built it ended = %d, want 0: the server was not stopped", got)
	}
}

func memKV(tb testing.TB) kv.KV {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	return db
}
