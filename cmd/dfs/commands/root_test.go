package commands

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dfs has the server's own commands, init, start and reset-admin, beside
// config, version and completion, and no other; start has no daemon flags.
func TestCommandTree(t *testing.T) {
	var got []string
	for _, c := range rootCmd.Commands() {
		if c.Name() != "help" {
			got = append(got, c.Name())
		}
	}
	slices.Sort(got)
	if want := []string{"completion", "config", "init", "reset-admin", "start", "version"}; !slices.Equal(got, want) {
		t.Errorf("dfs commands %q, want %q", got, want)
	}
	for _, flag := range []string{"foreground", "pid-file", "log-file"} {
		if startCmd.Flags().Lookup(flag) != nil {
			t.Errorf("dfs start has --%s, want no daemon flag", flag)
		}
	}
}

// dfs start fails, naming the step, while the node starts nothing.
func TestStartNamesTheStep(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "logging:\n  output: stderr\nblockstore:\n  journal:\n    path: '" + filepath.Join(t.TempDir(), "blocks") + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rootCmd.SetArgs([]string{"start", "--config", path})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	err := rootCmd.Execute()
	if !errors.Is(err, errors.ErrUnsupported) || !strings.HasPrefix(err.Error(), "starting the node: ") {
		t.Errorf("dfs start: %v, want the node's start step refused", err)
	}
}
