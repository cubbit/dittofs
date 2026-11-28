package adaptertest

import (
	"context"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// OtherUser stores a second user, a copy of the administrator under another
// principal, name and ids, and answers a call proved as that user under no
// protection. The adapters' tests may not import the store's entity types, so
// the user is copied and its store call typed by inference.
func OtherUser(t testing.TB, mem *memory.Service) vfs.Call {
	t.Helper()

	cp := mem.Store.Store.ControlPlane()

	u, err := mem.Store.Store.Principals().User(t.Context(), mem.Store.Admin.Principal)
	if err != nil {
		t.Fatal(err)
	}

	u.Principal.ID[0] ^= 1
	u.Name = "alice"
	u.IDs = slices.Clone(u.IDs)

	for i := range u.IDs {
		u.IDs[i].Value += "1"
	}

	put(t, cp.PutUser, u)

	c := mem.Admin()
	c.Identity.Protection = vfs.ProtectionNone
	c.Identity.Evidence.Principal = u.Principal

	return c
}

// put stores v through f with the zero value of its allocation argument.
func put[V, A any](t testing.TB, f func(context.Context, V, uint64, A) (uint64, error), v V) {
	t.Helper()

	var a A

	if _, err := f(t.Context(), v, 0, a); err != nil {
		t.Fatal(err)
	}
}
