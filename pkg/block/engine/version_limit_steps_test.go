package engine_test

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// dueOnVersion arms the unit so that the raise falls due on the n-th version
// the journal issues from now, and on no version before it: the engine's
// block is set so that half of it is the versions left at the (n-1)-th.
func dueOnVersion(t *testing.T, u *unit, n uint64) {
	t.Helper()

	next := u.j.Stats().NextVersion.Counter
	u.e.SetBlockForTest(2 * (u.limit - next - n + 1))
}

// raised reports whether the journal was given a limit above the one the
// unit started with, once every raise has ended.
func raised(t *testing.T, u *unit) bool {
	t.Helper()

	u.e.WaitRaises(t)

	return u.j.Stats().VersionLimit.Counter > u.limit
}

// A version issued by a removal or a clone step, not only by a write or a
// Draw, is evaluated against the trigger and wakes the raise (RFC 8 §2.7:
// "on every version it issues"). Each row issues the version that crosses the
// trigger with that step alone.
func TestEveryVersionIssueCountsTowardsTheRaise(t *testing.T) {
	ctx := t.Context()

	for _, c := range []struct {
		name string
		// run arms the unit to fall due on the version of the step under
		// test, then runs the step.
		deps []func(*engine.Deps)
		run  func(t *testing.T, u *unit)
	}{
		{
			name: "truncate",
			run: func(t *testing.T, u *unit) {
				f := u.committedFile(t, "f", fill(mib, 'a'))
				dueOnVersion(t, u, 1)

				if err := u.e.Truncate(ctx, u.share.ID, f, mib/2); err != nil {
					t.Fatalf("Truncate: %v", err)
				}
			},
		},
		{
			name: "deallocate",
			run: func(t *testing.T, u *unit) {
				f := u.committedFile(t, "f", fill(mib, 'a'))
				dueOnVersion(t, u, 1)

				if err := u.e.Deallocate(ctx, u.share.ID, f, 0, mib/2); err != nil {
					t.Fatalf("Deallocate: %v", err)
				}
			},
		},
		{
			name: "delete",
			// Every Remove finds the file gone, so the release commits.
			deps: []func(*engine.Deps){onRemove(func(
				context.Context,
				engine.Existence,
				entity.ShareID,
				entity.FileID,
				uint64,
				kvstore.RemoveOp,
				[]kvstore.PendingOp,
			) error {
				return entity.ErrStale
			})},
			run: func(t *testing.T, u *unit) {
				f := u.committedFile(t, "f", fill(mib, 'a'))
				dueOnVersion(t, u, 1)

				if _, err := u.e.Delete(ctx, u.share.ID, f); err != nil {
					t.Fatalf("Delete: %v", err)
				}
			},
		},
		{
			// A source of holes: the clone has nothing to copy, so
			// CloneTarget's version is the only one it issues.
			name: "clone target",
			run: func(t *testing.T, u *unit) {
				src := u.create(t, "src")
				if err := u.e.Truncate(ctx, u.share.ID, src, mib); err != nil {
					t.Fatalf("grow the source: %v", err)
				}

				dst := u.create(t, "dst")
				dueOnVersion(t, u, 1)
				u.clone(t, src, dst, vfserr.Extent{Len: mib})

				if n := len(u.j.ReservedWrites()); n != 0 {
					t.Fatalf("the clone copied %d runs, want none", n)
				}
			},
		},
		{
			// The raise falls due on the first copy, not on CloneTarget's
			// version before it.
			name: "clone copy",
			run: func(t *testing.T, u *unit) {
				src := u.committedFile(t, "src", fill(mib, 's'))
				dst := u.create(t, "dst")
				dueOnVersion(t, u, 2)
				u.clone(t, src, dst, vfserr.Extent{Len: mib})

				if n := len(u.j.ReservedWrites()); n != 1 {
					t.Fatalf("the clone copied %d runs, want 1", n)
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := clonedUnit(t, c.deps...)
			u.e.TrackRaisesForTest()

			c.run(t, u)

			if !raised(t, u) {
				t.Errorf("no raise ran after the %s issued the version that crosses the trigger", c.name)
			}
		})
	}
}
