package commands

import (
	"bytes"
	"strings"
	"testing"
)

// runDfs runs dfs with args on a root of its own and returns what it printed.
func runDfs(args ...string) (string, error) {
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// TestVersionCmd_OutputPrefix verifies that "dfs version" prints the binary
// name returned by cmd.Root().Name(), not the hardcoded string "dittofs".
func TestVersionCmd_OutputPrefix(t *testing.T) {
	out, err := runDfs("version")
	if err != nil {
		t.Fatalf("dfs version: %v", err)
	}
	if !strings.HasPrefix(out, "dfs ") {
		t.Errorf("version output should start with %q, got: %q", "dfs ", out)
	}
	if strings.HasPrefix(out, "dittofs ") {
		t.Errorf("version output must not start with hardcoded %q, got: %q", "dittofs ", out)
	}
}

// TestVersionCmd_ShortFlag verifies --short prints only the version token with
// no binary-name prefix (unchanged behaviour).
func TestVersionCmd_ShortFlag(t *testing.T) {
	Version = "v1.2.3"
	t.Cleanup(func() { Version = "dev" })

	out, err := runDfs("version", "--short")
	if err != nil {
		t.Fatalf("dfs version --short: %v", err)
	}
	if out = strings.TrimSpace(out); out != "v1.2.3" {
		t.Errorf("--short output = %q, want %q", out, "v1.2.3")
	}
}
