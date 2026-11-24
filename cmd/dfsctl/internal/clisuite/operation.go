package clisuite

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// operationRows are the rows of the operation commands, each over an
// operation that has ended, which no later read can see change.
func operationRows() []row {
	return []row{
		{name: "operation get -o json and -o yaml decode to the operation", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			op := s.Settle(t, c, startOperation(t, c))
			want, err := c.Operation(t.Context(), op.ID)
			if err != nil {
				t.Fatalf("read operation %s: %v", op.ID, err)
			}
			for _, format := range []string{"json", "yaml"} {
				out := mustRun(t, run, "operation", "get", op.ID, "-o", format)
				got, err := decode(format, out)
				if err != nil {
					t.Fatalf("operation get -o %s: decoding %q: %v", format, out, err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("operation get -o %s: got %+v, want %+v as the client reads it", format, got, want)
				}
			}
		}},
		{name: "operation list -o table heads its declared columns", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			op := s.Settle(t, c, startOperation(t, c))
			out := mustRun(t, run, "operation", "list")
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if got, want := strings.Fields(lines[0]), (cmdutil.Operations{}).Headers(); !reflect.DeepEqual(got, want) {
				t.Errorf("operation list header: got %q, want %q", got, want)
			}
			if !strings.Contains(out, op.ID) {
				t.Errorf("operation list: got %q, want a row for %s", out, op.ID)
			}
		}},
		{name: "operation wait of an ended operation prints it and exits 0", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			op := s.Settle(t, c, startOperation(t, c))
			out := mustRun(t, run, "operation", "wait", op.ID, "-o", "json")
			got, err := decode("json", out)
			if err != nil || got.ID != op.ID || got.State != apiv1.OpSucceeded {
				t.Errorf("operation wait %s: got %+v (%v), want it succeeded", op.ID, got, err)
			}
		}},
		{name: "operation cancel of a kind with no cancel is refused", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			op := s.Settle(t, c, startOperation(t, c))
			_, errOut, status := run(t.Context(), "operation", "cancel", op.ID)
			if status == 0 || !strings.Contains(errOut, string(apiv1.CodeNotCancellable)) {
				t.Errorf("operation cancel %s: status %d, stderr %q, want non-zero and %s", op.ID, status, errOut, apiv1.CodeNotCancellable)
			}
		}},
		{name: "operation wait of an operation that ended failed or cancelled exits non-zero", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, _ *client.Client) {
			for _, tc := range []struct {
				state entity.OpState
				want  string
			}{{entity.OpFailed, "failed"}, {entity.OpCancelled, "was cancelled"}} {
				op := s.ended(t, tc.state)
				out, errOut, status := run(t.Context(), "operation", "wait", op.ID, "-o", "json")
				if status == 0 || !strings.Contains(errOut, "operation "+op.ID+" "+tc.want) {
					t.Errorf("operation wait of an operation ended %s: status %d, stderr %q, want non-zero naming it %s", tc.want, status, errOut, tc.want)
				}
				if got, err := decode("json", out); err != nil || got.ID != op.ID {
					t.Errorf("operation wait of an operation ended %s: printed %q (%v), want the operation", tc.want, out, err)
				}
			}
		}},
		{name: "operation wait refused by the server mid-wait gives no resume hint", harnessOnly: true, run: func(t *testing.T, _ Runner, s Server, c *client.Client) {
			// An operation of a kind the node has no step for stays pending,
			// so its poll ends only when its wait does.
			id := entity.OperationID(uuid.New())
			if _, err := s.Harness.Store().ControlPlane().PutOperation(t.Context(), entity.Operation{ID: id, Kind: "clisuite.never",
				Target: entity.ResourceRef{Kind: entity.ResourceGroup, ID: id[:]}, Account: entity.NewUser(), State: entity.OpPending}, 0); err != nil {
				t.Fatalf("store an operation: %v", err)
			}
			watcher := "w-" + uuid.NewString()[:8]
			s.Harness.Account(t, watcher, "the watcher's password", entity.RoleViewer)
			w := s
			w.User, w.Password = watcher, "the watcher's password"
			run := s.newRunner(t)
			w.Login(t, run)
			base := s.Harness.Clock().Pending()
			type answered struct {
				errOut string
				status int
			}
			done := make(chan answered, 1)
			go func() {
				_, errOut, status := run(t.Context(), "operation", "wait", uuid.UUID(id).String())
				done <- answered{errOut, status}
			}()
			// Polling until the wait's poll has parked on the node's clock.
			for end := time.Now().Add(time.Minute); s.Harness.Clock().Pending() <= base; time.Sleep(time.Millisecond) {
				if time.Now().After(end) {
					t.Fatal("the wait's poll did not park within a minute")
				}
			}
			s.demote(t, watcher)
			a := Await(t, s, "the wait refused once its account lost its role", done)
			if a.status == 0 || strings.Contains(a.errOut, "resumes the wait") || !strings.Contains(a.errOut, "waiting for operation") {
				t.Errorf("operation wait refused mid-wait: status %d, stderr %q, want non-zero, the refusal and no resume hint", a.status, a.errOut)
			}
		}},
		{name: "operation get of an unknown ID is refused naming it", run: func(t *testing.T, run Runner, _ Server, _ *client.Client) {
			id := uuid.NewString()
			_, errOut, status := run(t.Context(), "operation", "get", id)
			if status == 0 || !strings.Contains(errOut, id) {
				t.Errorf("operation get %s: status %d, stderr %q, want non-zero naming the ID", id, status, errOut)
			}
		}},
	}
}

// mustRun is run failing the test unless it exits 0.
func mustRun(t *testing.T, run Runner, args ...string) string {
	t.Helper()
	out, errOut, status := run(t.Context(), args...)
	if status != 0 {
		t.Fatalf("dfsctl %s: status %d, stderr %q, want 0", strings.Join(args, " "), status, errOut)
	}
	return out
}

// decode reads out, an operation printed as format, json or yaml, the YAML
// through JSON as dfsctl printed it.
func decode(format, out string) (apiv1.Operation, error) {
	var op apiv1.Operation
	b := []byte(out)
	if format == "yaml" {
		var tree any
		if err := yaml.Unmarshal(b, &tree); err != nil {
			return op, err
		}
		var err error
		if b, err = json.Marshal(tree); err != nil {
			return op, err
		}
	}
	return op, json.Unmarshal(b, &op)
}

// ended stores an operation already ended in state, as only a runner's
// failure or a cancel would end one, and returns it as the client reads it.
func (s Server) ended(t *testing.T, state entity.OpState) apiv1.Operation {
	t.Helper()
	id := entity.OperationID(uuid.New())
	op := entity.Operation{ID: id, Kind: string(apiv1.OpGroupDelete), Target: entity.ResourceRef{Kind: entity.ResourceGroup, ID: id[:]},
		Account: entity.NewUser(), State: state}
	if state == entity.OpFailed {
		op.Error = []byte(`{"status":500,"title":"ended by the test","retryable":false,"request_id":"clisuite"}`)
	}
	if _, err := s.Harness.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("store an operation ended %v: %v", state, err)
	}
	return apiv1.Operation{ID: uuid.UUID(id).String()}
}

// demote takes every role from the account named name.
func (s Server) demote(t *testing.T, name string) {
	t.Helper()
	cp := s.Harness.Store().ControlPlane()
	u, _, err := cp.UserNamed(t.Context(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	u.Role = entity.RoleNone
	if _, err := cp.PutUser(t.Context(), u, u.Generation, entity.Alloc{}); err != nil {
		t.Fatalf("demote %s: %v", name, err)
	}
}
