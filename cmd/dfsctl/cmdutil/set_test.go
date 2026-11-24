package cmdutil_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// A grant's first set creates it with If-None-Match: *, so a grant another
// writer created between the command's read, which found none, and its
// write is refused 412 and keeps the other writer's access; run again, the
// command reads the grant it now finds and changes it.
func TestSetCreatesOnlyWhatIsAbsent(t *testing.T) {
	f := newFixture(t)
	u, err := f.c.Users().Create(t.Context(), apiv1.UserSpec{Name: "alice"})
	if err != nil || u.Resource == nil {
		t.Fatalf("create alice: %+v, %v", u, err)
	}
	alice := u.Resource.ID
	access := func() string {
		g, err := f.c.Grants(f.share.ID).Get(t.Context(), alice)
		if err != nil {
			t.Fatalf("read alice's grant: %v", err)
		}
		return g.Spec.Access
	}
	paused := 0
	env := cmdutil.PauseAfterRead(f.env, func() {
		paused++
		if _, err := f.c.Grants(f.share.ID).Put(t.Context(), alice, apiv1.GrantSpec{Access: "read"}, "*"); err != nil {
			t.Fatalf("grant alice read in the pause: %v", err)
		}
	})
	args := []string{"grant", "set", "builds", "--user", "alice", "--access", "admin"}
	_, err = run(t.Context(), env, args...)
	if paused != 1 {
		t.Fatalf("the pause after the read ran %d times, want once", paused)
	}
	if !cmdutil.HasStatus(err, http.StatusPreconditionFailed) || !strings.Contains(err.Error(), "was written since it was read absent, so nothing was written") {
		t.Fatalf("dfsctl %s racing a create: got %v, want the 412 refused", strings.Join(args, " "), err)
	}
	if got := access(); got != "read" {
		t.Errorf("alice's grant after the refused create: %s, want read, the other writer's", got)
	}
	if _, err := run(t.Context(), f.env, args...); err != nil {
		t.Fatalf("dfsctl %s again: %v", strings.Join(args, " "), err)
	}
	if got := access(); got != "admin" {
		t.Errorf("alice's grant after the second set: %s, want admin", got)
	}
}
