package clisuite

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// uniq is prefix and a fresh suffix, a name no other row uses.
func uniq(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

// namespace writes a namespace through the store, since the API refuses a
// namespace's create until the settings are served, and returns its ID.
func (s Server) namespace(t *testing.T) string {
	t.Helper()
	ns := entity.NamespaceID(uuid.New())
	if _, err := s.Harness.Store().ControlPlane().PutNamespace(t.Context(), entity.Namespace{ID: ns, Store: uuid.New()}, 0); err != nil {
		t.Fatalf("put a namespace: %v", err)
	}
	return uuid.UUID(ns).String()
}

// share creates a share of a name of its own in a namespace written through
// the store, open to one network, and returns it.
func (s Server) share(t *testing.T, c *client.Client) client.Tagged[apiv1.ShareSpec, apiv1.ShareStatus] {
	t.Helper()
	name := uniq("sh")
	sh, err := c.CreateShare(t.Context(), apiv1.NewShare{
		Spec:         apiv1.ShareSpec{Name: name, Path: "/" + name, Namespace: s.namespace(t)},
		ExportPolicy: apiv1.ExportPolicySpec{Flavors: []string{"sys"}, Clients: []apiv1.ClientRule{{CIDR: "10.0.0.0/8", Access: "read-write"}}},
	})
	if err != nil {
		t.Fatalf("create share %s: %v", name, err)
	}
	return sh
}

// decodePrinted reads out, a value printed as format, json or yaml, the YAML
// through JSON as dfsctl prints it.
func decodePrinted[T any](format, out string) (T, error) {
	var v T
	b := []byte(out)
	if format == "yaml" {
		var tree any
		if err := yaml.Unmarshal(b, &tree); err != nil {
			return v, err
		}
		var err error
		if b, err = json.Marshal(tree); err != nil {
			return v, err
		}
	}
	return v, json.Unmarshal(b, &v)
}

// refusedNaming runs args and fails the test unless the command exits non-zero
// with every one of want in its stderr.
func refusedNaming(t *testing.T, run Runner, want []string, args ...string) {
	t.Helper()
	_, errOut, status := run(t.Context(), args...)
	if status == 0 {
		t.Fatalf("dfsctl %s: status 0, want non-zero", strings.Join(args, " "))
	}
	for _, w := range want {
		if !strings.Contains(errOut, w) {
			t.Errorf("dfsctl %s: stderr %q, want it to name %q", strings.Join(args, " "), errOut, w)
		}
	}
}

// shareRows are the rows of the share commands. Each needs a namespace,
// which only the store can hold until the settings are served, so each is
// harness-only.
func shareRows() []row {
	return []row{
		{name: "share create, get and list", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			name, ns := uniq("sh"), s.namespace(t)
			mustRun(t, run, "share", "create", name, "--path", "/"+name, "--namespace", ns, "--case-insensitive",
				"--flavor", "sys", "--client", "10.0.0.0/8=read-write", "--client", "@auditors=read", "--squash", "all")
			var got []apiv1.Share
			for sh, err := range c.Shares().List(t.Context(), map[string][]string{"name": {name}}) {
				if err != nil {
					t.Fatalf("list shares named %s: %v", name, err)
				}
				got = append(got, sh)
			}
			if want := (apiv1.ShareSpec{Name: name, Path: "/" + name, Namespace: ns, CaseSensitivity: "insensitive"}); len(got) != 1 || got[0].Spec != want {
				t.Fatalf("share create %s: the server holds %+v, want one share of %+v", name, got, want)
			}
			p, err := c.ExportPolicies().Get(t.Context(), got[0].ID)
			if err != nil {
				t.Fatalf("read share %s's export policy: %v", name, err)
			}
			if want := []apiv1.ClientRule{{CIDR: "10.0.0.0/8", Access: "read-write"}, {Netgroup: "auditors", Access: "read"}}; !reflect.DeepEqual(p.Spec.Clients, want) ||
				!slices.Equal(p.Spec.Flavors, []string{"sys"}) || p.Spec.Squash != "all" {
				t.Errorf("share create %s: export policy %+v, want flavours [sys], squash all and clients %+v", name, p.Spec, want)
			}
			for _, format := range []string{"json", "yaml"} {
				sh, err := decodePrinted[apiv1.Share](format, mustRun(t, run, "share", "get", name, "-o", format))
				if err != nil || sh.ID != got[0].ID || sh.Spec != got[0].Spec {
					t.Errorf("share get %s -o %s: got %+v (%v), want %+v", name, format, sh, err, got[0])
				}
			}
			out := mustRun(t, run, "share", "list")
			if h := strings.Fields(strings.SplitN(out, "\n", 2)[0]); !reflect.DeepEqual(h, []string{"NAME", "PATH", "STATE", "ID"}) || !strings.Contains(out, got[0].ID) {
				t.Errorf("share list: got %q, want the header NAME PATH STATE ID and a row for %s", out, got[0].ID)
			}
		}},
		{name: "share update renames and moves a share", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			name := uniq("renamed")
			mustRun(t, run, "share", "update", sh.Spec.Name, "--name", name, "--path", "/"+name)
			now, err := c.Shares().Get(t.Context(), sh.ID)
			if err != nil || now.Spec.Name != name || now.Spec.Path != "/"+name {
				t.Errorf("share update: the server holds %+v (%v), want it named %s at /%s", now.Spec, err, name, name)
			}
		}},
		{name: "share quiesce and resume move the share's state", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			for _, tc := range []struct{ verb, state string }{{"quiesce", "quiesced"}, {"resume", "enabled"}} {
				mustRun(t, run, "share", tc.verb, sh.Spec.Name)
				now, err := c.Shares().Get(t.Context(), sh.ID)
				if err != nil || now.Status.State != tc.state {
					t.Errorf("share %s: the server holds state %q (%v), want %s", tc.verb, now.Status.State, err, tc.state)
				}
			}
		}},
		{name: "share delete --wait follows the deletion to its end", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			type ran struct {
				out, errOut string
				status      int
			}
			done := make(chan ran, 1)
			go func() {
				out, errOut, status := run(t.Context(), "share", "delete", sh.Spec.Name, "--force", "--wait", "-o", "json")
				done <- ran{out, errOut, status}
			}()
			r := Await(t, s, "share delete --wait", done)
			if r.status != 0 {
				t.Fatalf("share delete --wait: status %d, stderr %q, want 0", r.status, r.errOut)
			}
			if op, err := decodePrinted[apiv1.Operation]("json", r.out); err != nil || op.State != apiv1.OpSucceeded || op.Kind != apiv1.OpShareDelete {
				t.Errorf("share delete --wait: printed %+v (%v), want the share's deletion succeeded", op, err)
			}
			if _, err := c.Shares().Get(t.Context(), sh.ID); !cmdutil.HasStatus(err, http.StatusNotFound) {
				t.Errorf("share delete --wait: reading the share back answered %v, want 404", err)
			}
		}},
		{name: "share delete without --wait reports the removal as started, naming its operation", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			out := mustRun(t, run, "share", "delete", sh.Spec.Name, "--force")
			// The operation's ID is the first field of the table's one row.
			lines := strings.Split(out, "\n")
			if len(lines) < 2 || len(strings.Fields(lines[1])) == 0 {
				t.Fatalf("share delete: printed %q, want the operation's table", out)
			}
			id := strings.Fields(lines[1])[0]
			op, err := c.Operation(t.Context(), id)
			if err != nil || op.Kind != apiv1.OpShareDelete {
				t.Fatalf("share delete: operation %s read back as %+v (%v), want the share's deletion", id, op, err)
			}
			want := "removal of share '" + sh.Spec.Name + "' started as operation " + id + "; 'dfsctl operation wait " + id + "' waits for it"
			if !strings.Contains(out, want) || strings.Contains(out, "removed successfully") {
				t.Errorf("share delete: printed %q, want %q and no claim it was removed", out, want)
			}
			s.Settle(t, c, op)
		}},
		{name: "share delete --wait reports the removal once its operation succeeded", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			type ran struct {
				out, errOut string
				status      int
			}
			done := make(chan ran, 1)
			go func() {
				out, errOut, status := run(t.Context(), "share", "delete", sh.Spec.Name, "--force", "--wait")
				done <- ran{out, errOut, status}
			}()
			r := Await(t, s, "share delete --wait", done)
			if r.status != 0 {
				t.Fatalf("share delete --wait: status %d, stderr %q, want 0", r.status, r.errOut)
			}
			ended, removed := strings.Index(r.out, string(apiv1.OpSucceeded)), strings.Index(r.out, "share '"+sh.Spec.Name+"' removed successfully")
			if ended < 0 || removed < ended || strings.Contains(r.out, "started as operation") {
				t.Errorf("share delete --wait: printed %q, want the operation succeeded and then the share removed, and no claim it only started", r.out)
			}
		}},
		{name: "share export-policy update changes only its flags' fields and set writes the whole policy", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			policy := func() apiv1.ExportPolicySpec {
				p, err := c.ExportPolicies().Get(t.Context(), sh.ID)
				if err != nil {
					t.Fatalf("read share %s's export policy: %v", sh.Spec.Name, err)
				}
				return p.Spec
			}
			want := policy()
			mustRun(t, run, "share", "export-policy", "update", sh.Spec.Name, "--read-only", "--smb-encrypt", "--manage-gids")
			want.ReadOnly, want.SMB.Encrypt, want.ManageGIDs = true, true, true
			if got := policy(); !reflect.DeepEqual(got, want) {
				t.Errorf("share export-policy update: the server holds %+v, want %+v", got, want)
			}
			// set takes every field its flags do not name back to its default,
			// read-only, SMB encryption and manage-gids among them.
			mustRun(t, run, "share", "export-policy", "set", sh.Spec.Name, "--flavor", "sys", "--client", "192.168.0.0/16=read", "--squash", "all")
			want = apiv1.ExportPolicySpec{Flavors: []string{"sys"}, Clients: []apiv1.ClientRule{{CIDR: "192.168.0.0/16", Access: "read"}},
				MinProof: "none", MinProtection: "none", Squash: "all"}
			if got := policy(); !reflect.DeepEqual(got, want) {
				t.Errorf("share export-policy set: the server holds %+v, want %+v", got, want)
			}
			p, err := decodePrinted[apiv1.ExportPolicy]("json", mustRun(t, run, "share", "export-policy", "get", sh.Spec.Name, "-o", "json"))
			if err != nil || !reflect.DeepEqual(p.Spec, want) {
				t.Errorf("share export-policy get: printed %+v (%v), want %+v", p.Spec, err, want)
			}
		}},
		{name: "share mount builds its line with the adapter record's port", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			// The adapter stays disabled, so the node binds no port for the write.
			mustRun(t, run, "adapter", "set", "smb", "--setting", "port=1445")
			out := mustRun(t, run, "share", "mount", sh.Spec.Name, "/mnt/x", "--protocol", "smb", "-u", "alice", "--dry-run")
			// The port as each platform's line spells it: Linux's port=,
			// macOS's URL, Windows' /TCPPORT:.
			port := strings.Contains(out, "port=1445,") || strings.Contains(out, ":1445/") || strings.Contains(out, "/TCPPORT:1445")
			if !port || !strings.Contains(out, sh.Spec.Name) {
				t.Errorf("share mount --protocol smb: printed %q, want the share's name and the port 1445", out)
			}
			// dfsctl runs as the caller and the mount command, which needs
			// root, through sudo.
			if runtime.GOOS != "windows" && os.Geteuid() != 0 {
				out := mustRun(t, run, "share", "mount", sh.Spec.Name, "/mnt/x", "--protocol", "nfs", "--dry-run")
				if !strings.HasPrefix(out, "sudo mount ") {
					t.Errorf("share mount --protocol nfs --dry-run as uid %d: printed %q, want the mount command through sudo", os.Geteuid(), out)
				}
			}
		}},
		{name: "share get of a name no share holds is refused naming it", run: func(t *testing.T, run Runner, _ Server, _ *client.Client) {
			name := uniq("absent")
			refusedNaming(t, run, []string{name}, "share", "get", name)
		}},
	}
}
