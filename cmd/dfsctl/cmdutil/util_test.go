package cmdutil

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestBoolToYesNo(t *testing.T) {
	tests := []struct {
		input    bool
		expected string
	}{
		{true, "yes"},
		{false, "no"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := BoolToYesNo(tt.input)
			if result != tt.expected {
				t.Errorf("BoolToYesNo(%v) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// testTableRenderer implements output.TableRenderer for testing
type testTableRenderer struct {
	headers []string
	rows    [][]string
}

func (t testTableRenderer) Headers() []string {
	return t.headers
}

func (t testTableRenderer) Rows() [][]string {
	return t.rows
}

// newCmd returns a command whose --output flag says format and whose output
// lands in the returned buffer, as a subcommand of the root sees both.
func newCmd(format string) (*cobra.Command, *bytes.Buffer) {
	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("output", format, "")
	cmd.Flags().Bool("no-color", true, "")
	cmd.SetOut(&buf)
	return cmd, &buf
}

func TestPrintOutput(t *testing.T) {
	renderer := testTableRenderer{headers: []string{"NAME"}, rows: [][]string{{"foo"}, {"bar"}}}
	for _, tt := range []struct {
		format  string
		isEmpty bool
		want    string
	}{
		{"json", false, "[\n  \"foo\",\n  \"bar\"\n]\n"},
		{"yaml", false, "- foo\n- bar\n"},
		{"table", true, "No items found.\n"},
	} {
		cmd, buf := newCmd(tt.format)
		if err := PrintOutput(cmd, []string{"foo", "bar"}, tt.isEmpty, "No items found.", renderer); err != nil {
			t.Fatalf("PrintOutput -o %s: %v", tt.format, err)
		}
		if buf.String() != tt.want {
			t.Errorf("PrintOutput -o %s: got %q, want %q", tt.format, buf.String(), tt.want)
		}
	}

	cmd, buf := newCmd("table")
	if err := PrintOutput(cmd, nil, false, "No items found.", renderer); err != nil {
		t.Fatalf("PrintOutput -o table: %v", err)
	}
	if !strings.Contains(buf.String(), "NAME") || !strings.Contains(buf.String(), "bar") {
		t.Errorf("PrintOutput -o table: got %q, want the header NAME and the row bar", buf.String())
	}

	if err := PrintOutput(func() *cobra.Command { c, _ := newCmd("xml"); return c }(), nil, false, "", renderer); err == nil {
		t.Error("PrintOutput -o xml: got no error, want the format refused")
	}
}

// With no person at Env's reader a removal refuses before prompting, naming
// --force, and --force removes without a prompt either way.
func TestRunDeleteWithConfirmation(t *testing.T) {
	for _, tt := range []struct {
		name        string
		in          string
		interactive bool
		force       bool
		wantDeleted bool
		wantErr     string
	}{
		{name: "terminal, forced", interactive: true, force: true, wantDeleted: true},
		{name: "no terminal", in: "y\n", wantErr: "test: removing Context 'c' needs confirmation; pass --force"},
		{name: "no terminal, forced", force: true, wantDeleted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd, _ := newCmd("table")
			env := Env{In: strings.NewReader(tt.in), Interactive: tt.interactive}
			deleted := false
			err := RunDeleteWithConfirmation(cmd, env, "Context", "c", tt.force, func() error { deleted = true; return nil })
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error: got %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("error: got %v, want none", err)
			}
			if deleted != tt.wantDeleted {
				t.Errorf("deleted: got %v, want %v", deleted, tt.wantDeleted)
			}
		})
	}
}
