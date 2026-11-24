package clisuite

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// user creates a user of a name of its own and returns its name and ID.
func user(t *testing.T, c *client.Client) (string, string) {
	t.Helper()
	name := uniq("u")
	res, err := c.Users().Create(t.Context(), apiv1.UserSpec{Name: name})
	if err != nil || res.Resource == nil {
		t.Fatalf("create user %s: %+v, %v, want the user", name, res, err)
	}
	return name, res.Resource.ID
}

// group creates a group of a name of its own and returns its name and ID.
func group(t *testing.T, c *client.Client) (string, string) {
	t.Helper()
	name := uniq("g")
	g, err := c.Groups().Create(t.Context(), apiv1.GroupSpec{Name: name})
	if err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	return name, g.ID
}

// grantRows are the rows of the grant commands, harness-only for the share
// each needs.
func grantRows() []row {
	return []row{
		{name: "grant set, list and remove by user and by group", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			uname, uid := user(t, c)
			gname, gid := group(t, c)
			access := func(id string) string {
				g, err := c.Grants(sh.ID).Get(t.Context(), id)
				if cmdutil.HasStatus(err, http.StatusNotFound) {
					return "(no grant)"
				}
				if err != nil {
					t.Fatalf("read the grant of %s: %v", id, err)
				}
				return g.Spec.Access
			}
			for _, tc := range []struct{ flag, name, id string }{{"--user", uname, uid}, {"--group", gname, gid}} {
				// The first set creates the grant, the second changes it.
				for _, want := range []string{"read-write", "none"} {
					mustRun(t, run, "grant", "set", sh.Spec.Name, tc.flag, tc.name, "--access", want)
					if got := access(tc.id); got != want {
						t.Errorf("grant set %s %s --access %s: the server holds %s, want %s", tc.flag, tc.name, want, got, want)
					}
				}
			}
			out := mustRun(t, run, "grant", "list", sh.Spec.Name)
			if !strings.Contains(out, uid) || !strings.Contains(out, gid) {
				t.Errorf("grant list: got %q, want rows for %s and %s", out, uid, gid)
			}
			mustRun(t, run, "grant", "remove", sh.Spec.Name, "--user", uname)
			if got := access(uid); got != "(no grant)" {
				t.Errorf("grant remove --user %s: the server holds %s, want no grant", uname, got)
			}
			if got := access(gid); got != "none" {
				t.Errorf("grant remove --user %s: the group's grant is %s, want none, untouched", uname, got)
			}
		}},
		{name: "grant set by a directory name with no directory resolves it, is refused and grants nothing", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			// The resolution runs on the runner's turns, which Await drives.
			type ran struct {
				errOut string
				status int
			}
			done := make(chan ran, 1)
			go func() {
				_, errOut, status := run(t.Context(), "grant", "set", sh.Spec.Name, "--directory", `CORP\bob`, "--access", "read")
				done <- ran{errOut, status}
			}()
			r := Await(t, s, "grant set --directory", done)
			if r.status == 0 || !strings.Contains(r.errOut, `resolving "CORP\\bob"`) || !strings.Contains(r.errOut, "failed") {
				t.Errorf("grant set --directory CORP\\bob with no directory: status %d, stderr %q, want non-zero naming the resolution's failure", r.status, r.errOut)
			}
			for g, err := range c.Grants(sh.ID).List(t.Context(), nil) {
				if err != nil {
					t.Fatalf("list share %s's grants: %v", sh.Spec.Name, err)
				}
				t.Errorf("grant set --directory with no directory: the server holds grant %+v, want none", g)
			}
		}},
		{name: "grant set of a user no account holds is refused naming it", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			name := uniq("nobody")
			refused(t, run, []string{name}, "grant", "set", sh.Spec.Name, "--user", name, "--access", "read")
		}},
	}
}
