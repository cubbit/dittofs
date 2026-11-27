package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/keyprovider"
)

// passphrase is the passphrase of the key files goodFile names.
const passphrase = "s3cr3t-pass"

// fourRefusals is the file whose load refuses four keys, with the four
// lines, "P" standing for the file's path.
const fourRefusals = "loging:\n  level: debug\nmetrics:\n  listen_address: x\n" +
	"adapters:\n  smb:\n    port: 445\njournals:\n  - path: data/j1\n"

var fourRefusalLines = []string{
	"P: adapters.smb.port: a setting, not a host fact; set it through the management API",
	"P: journals: data/j1 is not an absolute path",
	"P: loging.level: unknown key; did you mean logging.level?",
	"P: metrics.listen_address: unknown key; did you mean metrics.listen?",
}

// lines renders want's lines with "P" replaced by path, as a load's error
// prints them.
func lines(path string, want []string) string {
	out := make([]string, len(want))
	for i, l := range want {
		out[i] = path + strings.TrimPrefix(l, "P")
	}

	return strings.Join(out, "\n")
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

// keyFiles returns two key files sealed with pass, each with its own key.
func keyFiles(t *testing.T, pass string) [2]string {
	t.Helper()

	var files [2]string

	dir := t.TempDir()

	for i := range files {
		raw, err := keyprovider.GenerateKeyFile(pass)
		if err != nil {
			t.Fatalf("GenerateKeyFile: %v", err)
		}

		files[i] = filepath.Join(dir, "k"+strconv.Itoa(i)+".key")

		if err := os.WriteFile(files[i], raw, 0o600); err != nil {
			t.Fatalf("write key file: %v", err)
		}
	}

	return files
}

// goodBody is a file a start accepts: a journal and a local key file for
// each role.
func goodBody(keys [2]string) string {
	return "journals:\n  - path: /j/1\n" +
		"keys:\n" +
		"  protocol:\n    kind: local\n    local:\n      file: " + keys[0] + "\n" +
		"  storage:\n    kind: local\n    local:\n      file: " + keys[1] + "\n"
}

// hostEnviron is an environment naming a state directory of the test's
// own and the key files' passphrase, after extra: the first entry of a name
// wins, so extra may name its own.
func hostEnviron(t *testing.T, extra ...string) []string {
	t.Helper()

	return append(
		extra,
		"STATE_DIRECTORY="+t.TempDir(),
		"DITTOFS_ENCRYPTION_PASSPHRASE="+passphrase,
	)
}
