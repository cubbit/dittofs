package clisuite

import (
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// newUser creates a user of a name of its own and returns its name and ID.
func newUser(t *testing.T, c *client.Client) (string, string) {
	t.Helper()
	name := uniq("u")
	res, err := c.Users().Create(t.Context(), apiv1.UserSpec{Name: name})
	if err != nil || res.Resource == nil {
		t.Fatalf("create user %s: %+v, %v, want the user", name, res, err)
	}
	return name, res.Resource.ID
}

// newGroup creates a group of a name of its own and returns its name and ID.
func newGroup(t *testing.T, c *client.Client) (string, string) {
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
			uname, uid := newUser(t, c)
			gname, gid := newGroup(t, c)
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
			gs, err := decodePrinted[[]apiv1.Grant]("json", mustRun(t, run, "grant", "list", sh.Spec.Name, "-o", "json"))
			got := map[string]string{}
			for _, g := range gs {
				got[g.ID] = g.Spec.Access
			}
			if want := map[string]string{uid: "none", gid: "none"}; err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("grant list -o json: got %v (%v), want %v", got, err, want)
			}
			mustRun(t, run, "grant", "remove", sh.Spec.Name, "--user", uname)
			if got := access(uid); got != "(no grant)" {
				t.Errorf("grant remove --user %s: the server holds %s, want no grant", uname, got)
			}
			if got := access(gid); got != "none" {
				t.Errorf("grant remove --user %s: the group's grant is %s, want none, untouched", uname, got)
			}
			// The ID grant list prints addresses the grant with no name.
			mustRun(t, run, "grant", "remove", sh.Spec.Name, "--principal-id", gid)
			if got := access(gid); got != "(no grant)" {
				t.Errorf("grant remove --principal-id %s: the server holds %s, want no grant", gid, got)
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
			if r.status == 0 {
				t.Fatalf("grant set --directory CORP\\bob with no directory: status 0, stderr %q, want non-zero", r.errOut)
			}
			// The operation the server ran, named in the refusal, resolved the
			// name the command was given, and failed.
			m := regexp.MustCompile(`operation ([0-9a-f-]{36}) failed`).FindStringSubmatch(r.errOut)
			if m == nil {
				t.Fatalf("grant set --directory CORP\\bob: stderr %q, want it to name the failed resolution", r.errOut)
			}
			op, err := c.Operation(t.Context(), m[1])
			if err != nil || op.Kind != apiv1.OpResolvePrincipal || op.State != apiv1.OpFailed || op.Error == nil || !strings.Contains(op.Error.Detail, `CORP\bob`) {
				t.Errorf("grant set --directory CORP\\bob: operation %s is %+v (%v), want a failed resolve-principal of CORP\\bob", m[1], op, err)
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
			refusedNaming(t, run, []string{name}, "grant", "set", sh.Spec.Name, "--user", name, "--access", "read")
		}},
	}
}
