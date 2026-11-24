package clisuite

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// installationRows are the rows of the node, audit, domain and status
// commands. The domain's actions need a directory, which no server the
// suite starts wires, so their rows are of the refusal; their success is
// the integration suite's.
func installationRows() []row {
	return []row{
		{name: "node list and get name the server's node", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			var want []string
			for n, err := range c.Nodes().List(t.Context(), nil) {
				if err != nil {
					t.Fatalf("list the nodes: %v", err)
				}
				want = append(want, n.ID)
			}
			if s.Harness != nil {
				if id := uuid.UUID(s.Harness.NodeID()).String(); len(want) != 1 || want[0] != id {
					t.Fatalf("the harness's nodes: got %v, want [%s]", want, id)
				}
			}
			out := mustRun(t, run, "node", "list")
			for _, id := range want {
				if !strings.Contains(out, id) {
					t.Errorf("node list: got %q, want a row for %s", out, id)
				}
				var got apiv1.Node
				decodeJSON(t, mustRun(t, run, "node", "get", id, "-o", "json"), &got)
				if got.ID != id {
					t.Errorf("node get %s: got %+v", id, got)
				}
			}
		}},
		{name: "audit list --actor shows the record of a change the actor made", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			g, err := c.Groups().Create(t.Context(), apiv1.GroupSpec{Name: "g-" + uuid.NewString()[:8]})
			if err != nil {
				t.Fatalf("create a group: %v", err)
			}
			var got []apiv1.AuditRecord
			decodeJSON(t, mustRun(t, run, "audit", "list", "--actor", s.User, "-o", "json"), &got)
			found := false
			for _, r := range got {
				if r.ActorName != s.User {
					t.Errorf("audit list --actor %s: got a record of %q", s.User, r.ActorName)
				}
				found = found || r.Target.ID == g.ID
			}
			if !found {
				t.Errorf("audit list --actor %s: got %d records, want one of group %s's create", s.User, len(got), g.ID)
			}
		}},
		{name: "domain actions end failed with no directory wired", run: func(t *testing.T, run Runner, s Server, _ *client.Client) {
			pw := passwordFile(t)
			for _, args := range [][]string{
				{"domain", "resolve", `CORP\bob`},
				{"domain", "test"},
				{"domain", "rotate-machine-password"},
				{"domain", "join", "--user", "administrator@corp.example.com", "--password-file", pw},
				{"domain", "join", "--offline", "--account", "FILESRV", "--password-file", pw},
			} {
				errOut, status := s.waited(t, run, args...)
				if status == 0 || !strings.Contains(errOut, "dfsctl "+strings.Join(args[:2], " ")+": operation ") || !strings.Contains(errOut, " failed") {
					t.Errorf("dfsctl %s --wait: status %d, stderr %q, want non-zero and its operation failed", strings.Join(args, " "), status, errOut)
				}
			}
		}},
		{name: "domain actions send their arguments", run: func(t *testing.T, run Runner, _ Server, _ *client.Client) {
			pw := passwordFile(t)
			// Each argument is one the server refuses before it starts
			// anything, naming the field it arrived in.
			long := strings.Repeat("x", 2048)
			for _, tc := range []struct {
				args  []string
				field string
			}{
				{[]string{"domain", "resolve", long}, "name"},
				{[]string{"domain", "resolve", "--sid", "not-a-sid"}, "sid"},
				{[]string{"domain", "join", "--user", "administrator", "--dns-host-name", "host", "--password-file", pw}, "spec.dns_host_name"},
				{[]string{"domain", "join", "--offline", "--account", long, "--password-file", pw}, "spec.account"},
			} {
				_, errOut, status := run(t.Context(), tc.args...)
				if status == 0 || !strings.Contains(errOut, "400 ") || !strings.Contains(errOut, "(field "+tc.field+")") {
					t.Errorf("dfsctl %s: status %d, stderr %q, want non-zero and the server's 400 naming %s", strings.Join(tc.args[:2], " "), status, errOut, tc.field)
				}
			}
		}},
		{name: "domain join sends no idempotency key, online or offline", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			pw := passwordFile(t)
			for _, args := range [][]string{
				{"domain", "join", "--user", "administrator", "--password-file", pw},
				{"domain", "join", "--offline", "--account", "FILESRV", "--password-file", pw},
				// A rotation does send one, so the audit is seen to hold it.
				{"domain", "rotate-machine-password"},
			} {
				s.waited(t, run, args...)
			}
			keys := map[string]bool{}
			for r, err := range c.Audit(t.Context(), nil) {
				if err != nil {
					t.Fatalf("read the audit: %v", err)
				}
				var req struct {
					Path           string `json:"path"`
					IdempotencyKey string `json:"idempotency_key"`
				}
				if err := json.Unmarshal(r.Request, &req); err != nil {
					t.Fatalf("audit record %s: request %s: %v", r.ID, r.Request, err)
				}
				for _, verb := range []string{"join-domain", "rotate-machine-password"} {
					if strings.HasSuffix(req.Path, ":"+verb) {
						keys[verb] = keys[verb] || req.IdempotencyKey != ""
					}
				}
			}
			if keys["join-domain"] {
				t.Error("domain join: an audited request carried an idempotency key, want none")
			}
			if !keys["rotate-machine-password"] {
				t.Error("domain rotate-machine-password: no audited request carried an idempotency key, want one, as the audit records keys")
			}
		}},
		{name: "status names the node's readiness", run: func(t *testing.T, run Runner, _ Server, c *client.Client) {
			want, err := c.Health(t.Context())
			if err != nil {
				t.Fatalf("read the health: %v", err)
			}
			var got apiv1.Health
			decodeJSON(t, mustRun(t, run, "status", "-o", "json"), &got)
			if got.Ready != want.Ready || got.Ready == "" {
				t.Errorf("status -o json: ready %q, want %q", got.Ready, want.Ready)
			}
			if out := mustRun(t, run, "status"); !strings.Contains(out, want.Ready) {
				t.Errorf("status: got %q, want it to name %q", out, want.Ready)
			}
		}},
	}
}

// passwordFile is a file holding a password, for a --password-file flag.
func passwordFile(t *testing.T) string {
	t.Helper()
	pw := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pw, []byte("a password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return pw
}

// waited runs args with --wait to the end of the operation the command
// starts, which on a harness's node only the clock Await advances ends,
// and returns its stderr and exit status.
func (s Server) waited(t *testing.T, run Runner, args ...string) (string, int) {
	t.Helper()
	type answered struct {
		errOut string
		status int
	}
	done := make(chan answered, 1)
	go func() {
		_, errOut, status := run(t.Context(), append(args, "--wait")...)
		done <- answered{errOut, status}
	}()
	a := Await(t, s, "dfsctl "+strings.Join(args, " ")+" --wait", done)
	return a.errOut, a.status
}
