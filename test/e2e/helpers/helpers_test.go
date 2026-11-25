//go:build e2e

package helpers_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/test/e2e/helpers"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// The helpers run against a real dfs serving no protocol: the API calls
// each admission case makes are what these tests check, and no mount.
func TestMain(m *testing.M) {
	os.Exit(helpers.Run(m, registry.Fixture{Server: "dfs", Profile: "memory"}))
}

// Every admission case arranges its share and accounts and runs each step's
// change on a real server: the share is applied, each account but the
// bootstrap administrator holds its uid, and each step's change changes the
// share's grants. Once the case's test ends, its share is removing and its
// accounts are gone.
func TestAdmissionArranges(t *testing.T) {
	api := helpers.Start(t).API
	for _, c := range helpers.Admission() {
		var share helpers.Share
		var users []string
		t.Run(c.Row, func(t *testing.T) {
			sh, steps := c.Arrange(t)
			share = sh
			ctx := context.Background()
			if got, err := api.Shares().Get(ctx, sh.ID); err != nil || got.Spec.Name != sh.Name {
				t.Fatalf("share %s: read back %+v, %v; want it named %s", sh.ID, got.Spec, err, sh.Name)
			}
			before := grants(t, sh)
			for _, st := range steps {
				if st.Change != nil {
					st.Change(t)
					if after := grants(t, sh); maps.Equal(after, before) {
						t.Errorf("%s: the grants are %v after the change as before it, want them changed", st.Name, after)
					} else {
						before = after
					}
				}
				for _, e := range st.Want {
					if e.Who.ID == "" {
						continue // the bootstrap administrator
					}
					users = append(users, e.Who.ID)
					u, err := api.Users().Get(ctx, e.Who.ID)
					if err != nil || u.Spec.UID == nil || *u.Spec.UID != e.Who.UID {
						t.Errorf("%s: user %s read back %+v, %v; want uid %d", st.Name, e.Who.Name, u.Spec, err, e.Who.UID)
					}
				}
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		// A deleted share stays removing until its deletion's operation
		// ends, which it cannot yet.
		if sh, err := api.Shares().Get(ctx, share.ID); !notFound(err) && (err != nil || sh.Status.State != "removing") {
			t.Errorf("%s: share %s after its test: state %q, %v; want it removing or gone", c.Row, share.Name, sh.Status.State, err)
		}
		for _, id := range users {
			if _, err := api.Users().Get(ctx, id); !notFound(err) {
				t.Errorf("%s: user %s after its test: %v, want 404", c.Row, id, err)
			}
		}
		cancel()
	}
}

// grants is each principal's access on sh, by the principal's ID.
func grants(t *testing.T, sh helpers.Share) map[string]string {
	t.Helper()
	out := map[string]string{}
	for g, err := range helpers.Start(t).API.Grants(sh.ID).List(context.Background(), nil) {
		if err != nil {
			t.Fatalf("listing the grants on %s: %v", sh.Name, err)
		}
		out[g.ID] = g.Spec.Access
	}
	return out
}

func notFound(err error) bool {
	var p *apiv1.Problem
	return errors.As(err, &p) && p.Status == http.StatusNotFound
}
