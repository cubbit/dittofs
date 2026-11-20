package config

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/cli/output"
	"github.com/cubbit/dittofs/pkg/bootstrap"
)

// validate runs validate as dfs config validate does, with the --config
// value flag and environ, and returns what it printed and its error.
func validate(t *testing.T, flag string, environ []string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	err := runValidate(context.Background(), flag, environ, &out)

	return out.String(), err
}

// A file the start refuses returns the start's refusal as it is, its four
// lines, and validate prints nothing before or after it.
func TestValidateFourRefusals(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, fourRefusals)

	out, err := validate(t, path, hostEnviron(t))
	if err == nil {
		t.Fatal("validate: got no error, want the four refusals")
	}

	if want := lines(path, fourRefusalLines); err.Error() != want {
		t.Errorf("validate's error:\ngot  %q\nwant %q", err.Error(), want)
	}

	if out != "" {
		t.Errorf("validate printed %q beside a refusal, want nothing", out)
	}
}

// A file Load accepts and Validate refuses returns Validate's lines: one
// role, no journal, no key.
func TestValidateRunsValidate(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "node:\n  roles: [storage]\n")
	environ := hostEnviron(t)

	b, err := bootstrap.Load(path, environ)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := b.Validate(context.Background(), environ)
	if want == nil {
		t.Fatal("Validate accepted a file with one role and no journal")
	}

	out, err := validate(t, path, environ)
	if err == nil || err.Error() != want.Error() {
		t.Errorf("validate's error:\ngot  %v\nwant %q", err, want.Error())
	}

	if out != "" {
		t.Errorf("validate printed %q beside a refusal, want nothing", out)
	}
}

// A file both Load and Validate would refuse returns Load's lines only, as
// a start stops at the load.
func TestValidateLoadRefusalFirst(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "loging:\n  level: debug\nnode:\n  roles: [storage]\n")

	_, err := validate(t, path, hostEnviron(t))

	want := path + ": loging.level: unknown key; did you mean logging.level?"
	if err == nil || err.Error() != want {
		t.Errorf("validate's error:\ngot  %v\nwant %q", err, want)
	}
}

// validate starts nothing: the runtime directory a start would make is
// still missing afterwards, the XDG one and the fallback under a state
// directory that does not exist yet.
func TestValidateLeavesNoDirectory(t *testing.T) {
	t.Parallel()

	keys := keyFiles(t, passphrase)
	path := writeConfig(t, goodBody(keys))

	for _, tc := range []struct {
		name    string
		environ func(tmp string) []string
		absent  []string
	}{
		{
			name: "the XDG runtime directory",
			environ: func(tmp string) []string {
				return []string{
					"STATE_DIRECTORY=" + t.TempDir(),
					"XDG_RUNTIME_DIR=" + filepath.Join(tmp, "xdg"),
				}
			},
			absent: []string{"xdg"},
		},
		{
			name: "the fallback under a missing state directory",
			environ: func(tmp string) []string {
				return []string{"STATE_DIRECTORY=" + filepath.Join(tmp, "state")}
			},
			absent: []string{"state", filepath.Join("state", "run")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tmp := t.TempDir()
			environ := append(tc.environ(tmp), "DITTOFS_ENCRYPTION_PASSPHRASE="+passphrase)

			if out, err := validate(t, path, environ); err != nil {
				t.Fatalf("validate: %v\noutput:\n%s", err, out)
			}

			for _, a := range tc.absent {
				if _, err := os.Lstat(filepath.Join(tmp, a)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s after validate: Lstat error %v, want it absent", a, err)
				}
			}
		})
	}
}

// A good bootstrap prints the file read and OK, and "none" when no file was
// read and the variables give the journals and the keys.
func TestValidateOK(t *testing.T) {
	t.Parallel()

	keys := keyFiles(t, passphrase)
	path := writeConfig(t, goodBody(keys))

	for _, tc := range []struct {
		name    string
		flag    string
		environ []string
		want    string
	}{
		{
			name:    "a file",
			flag:    path,
			environ: hostEnviron(t),
			want:    "Configuration file: " + path + "\nValidation: OK\n",
		},
		{
			name: "no file",
			environ: hostEnviron(t,
				"DITTOFS_JOURNALS=/j/1",
				"DITTOFS_KEYS_PROTOCOL_KIND=local",
				"DITTOFS_KEYS_PROTOCOL_LOCAL_FILE="+keys[0],
				"DITTOFS_KEYS_STORAGE_KIND=local",
				"DITTOFS_KEYS_STORAGE_LOCAL_FILE="+keys[1],
			),
			want: "Configuration file: none\nValidation: OK\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, err := validate(t, tc.flag, tc.environ)
			if err != nil {
				t.Fatalf("validate: %v", err)
			}

			if out != tc.want {
				t.Errorf("validate printed %q, want %q", out, tc.want)
			}
		})
	}
}

// With no --config, validate reads, names and refuses the file
// DITTOFS_CONFIG names, not the default one.
func TestValidateFollowsDittofsConfig(t *testing.T) {
	t.Parallel()

	keys := keyFiles(t, passphrase)

	home := t.TempDir()
	def := filepath.Join(home, ".config", "dittofs", "config.yaml")

	if err := os.MkdirAll(filepath.Dir(def), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(def, []byte(goodBody(keys)), 0o600); err != nil {
		t.Fatal(err)
	}

	good := writeConfig(t, goodBody(keys))

	out, err := validate(t, "", hostEnviron(t, "DITTOFS_CONFIG="+good, "HOME="+home))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	if want := "Configuration file: " + good + "\nValidation: OK\n"; out != want {
		t.Errorf("validate printed %q, want %q", out, want)
	}

	bad := writeConfig(t, "loging:\n  level: debug\n")

	_, err = validate(t, "", hostEnviron(t, "DITTOFS_CONFIG="+bad, "HOME="+home))

	want := bad + ": loging.level: unknown key; did you mean logging.level?"
	if err == nil || err.Error() != want {
		t.Errorf("validate of a bad DITTOFS_CONFIG file:\ngot  %v\nwant %q", err, want)
	}
}

// No refusal validate returns holds the value of an environment variable:
// a wrong passphrase, a relative path, a relative runtime directory.
func TestValidatePrintsNoEnvironmentValue(t *testing.T) {
	t.Parallel()

	keys := keyFiles(t, "another-pass")
	path := writeConfig(t, goodBody(keys))

	for _, tc := range []struct {
		name    string
		environ []string
		want    string
		value   string
	}{
		{
			name:    "a passphrase that does not unwrap",
			environ: hostEnviron(t),
			want: path + ": keys.protocol.local.file: the key file does not unwrap\n" +
				path + ": keys.storage.local.file: the key file does not unwrap",
			value: "s3cr3t",
		},
		{
			name:    "a relative path",
			environ: hostEnviron(t, "DITTOFS_METADATA_BADGER_PATH=data/badger"),
			want:    "environment: DITTOFS_METADATA_BADGER_PATH: not an absolute path",
			value:   "data/badger",
		},
		{
			name:    "a relative runtime directory",
			environ: hostEnviron(t, "XDG_RUNTIME_DIR=rel/run"),
			want:    "environment: XDG_RUNTIME_DIR: must be an absolute path",
			value:   "rel/run",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := validate(t, path, tc.environ)
			if err == nil {
				t.Fatalf("validate: got no error, want %q", tc.want)
			}

			if err.Error() != tc.want {
				t.Errorf("validate's error:\ngot  %q\nwant %q", err.Error(), tc.want)
			}

			if strings.Contains(err.Error(), tc.value) {
				t.Errorf("validate's error %q holds the variable's value %q", err, tc.value)
			}
		})
	}
}

// show stops at the same refusal of a relative runtime directory, and
// prints nothing, neither the value.
func TestShowRefusesRelativeRuntimeDirectory(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "logging:\n  level: warn\n")

	var out bytes.Buffer

	err := runShow(path, hostEnviron(t, "XDG_RUNTIME_DIR=rel/run"), output.FormatYAML, &out)

	want := "environment: XDG_RUNTIME_DIR: must be an absolute path"
	if err == nil || err.Error() != want {
		t.Errorf("show's error:\ngot  %v\nwant %q", err, want)
	}

	if out.Len() != 0 {
		t.Errorf("show printed %q beside a refusal, want nothing", out.String())
	}
}
