package clisuite

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// operationRows are the rows of the operation commands, each over an
// operation that has ended, which no later read can see change.
func operationRows() []row {
	return []row{
		{name: "operation get -o json and -o yaml decode to the operation", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			op := s.Settle(t, c, Operation(t, c))
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
			op := s.Settle(t, c, Operation(t, c))
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
			op := s.Settle(t, c, Operation(t, c))
			out := mustRun(t, run, "operation", "wait", op.ID, "-o", "json")
			got, err := decode("json", out)
			if err != nil || got.ID != op.ID || got.State != apiv1.OpSucceeded {
				t.Errorf("operation wait %s: got %+v (%v), want it succeeded", op.ID, got, err)
			}
		}},
		{name: "operation cancel of a kind with no cancel is refused", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			op := s.Settle(t, c, Operation(t, c))
			_, errOut, status := run(t.Context(), "operation", "cancel", op.ID)
			if status == 0 || !strings.Contains(errOut, string(apiv1.CodeNotCancellable)) {
				t.Errorf("operation cancel %s: status %d, stderr %q, want non-zero and %s", op.ID, status, errOut, apiv1.CodeNotCancellable)
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
