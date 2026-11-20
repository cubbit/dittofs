package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// minimalConfig is a loadable config body, followed by extra.
func minimalConfig(extra string) string {
	return `logging:
  level: "info"
  format: "text"
  output: "stdout"
` + extra
}

// runValidateCapture runs runConfigValidate on the config file at path, with
// the persistent --config flag the root dfs command registers, and returns
// what it printed.
func runValidateCapture(t *testing.T, path string) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("config", "", "")
	if err := cmd.Flags().Set("config", path); err != nil {
		t.Fatalf("set --config flag: %v", err)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runConfigValidate(cmd, nil)
	return out.String(), err
}

// writeConfig writes body to a config.yaml in a new directory.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// The clean path prints a bare "Validation: OK" with no warnings section.
func TestConfigValidate_NoWarningsBareOK(t *testing.T) {
	out, err := runValidateCapture(t, writeConfig(t, minimalConfig("")))
	if err != nil {
		t.Fatalf("runConfigValidate returned error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Validation: OK\n") {
		t.Errorf("expected 'Validation: OK', got:\n%s", out)
	}
	if strings.Contains(out, "Warnings:") {
		t.Errorf("clean config must not print a Warnings section, got:\n%s", out)
	}
}
