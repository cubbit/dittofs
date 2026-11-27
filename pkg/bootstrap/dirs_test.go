package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The environment the caller gives names the directories, each variable's
// value read for its first entry, in the order the loader reads them:
// STATE_DIRECTORY, else XDG_STATE_HOME/dittofs, else HOME's, and
// RUNTIME_DIRECTORY, else XDG_RUNTIME_DIR/dittofs, else a run directory
// under the state one.
func TestStateDirFromEnviron(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		environ    []string
		state      string
		stateVar   string
		runtime    string
		runtimeVar string
	}{
		{
			"the state variable, the home and the config home set",
			[]string{"STATE_DIRECTORY=/s1", "XDG_STATE_HOME=/x", "HOME=/h"},
			"/s1", "STATE_DIRECTORY",
			"/s1/run", "STATE_DIRECTORY",
		},
		{
			"the config home set",
			[]string{"XDG_STATE_HOME=/x", "HOME=/h"},
			"/x/dittofs", "XDG_STATE_HOME",
			"/x/dittofs/run", "XDG_STATE_HOME",
		},
		{
			"the home set",
			[]string{"HOME=/h"},
			"/h/.local/state/dittofs", "HOME",
			"/h/.local/state/dittofs/run", "HOME",
		},
		{
			"a list read for its first entry",
			[]string{"STATE_DIRECTORY=/s1:/s2", "RUNTIME_DIRECTORY=/r1:/r2"},
			"/s1", "STATE_DIRECTORY",
			"/r1", "RUNTIME_DIRECTORY",
		},
		{
			"the runtime variable's shape",
			[]string{"STATE_DIRECTORY=/s1", "RUNTIME_DIRECTORY=/r1"},
			"/s1", "STATE_DIRECTORY",
			"/r1", "RUNTIME_DIRECTORY",
		},
		{
			"the runtime home set",
			[]string{"STATE_DIRECTORY=/s1", "XDG_RUNTIME_DIR=/x"},
			"/s1", "STATE_DIRECTORY",
			"/x/dittofs", "XDG_RUNTIME_DIR",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := lookupDirs(tc.environ)

			if d.state != tc.state || d.stateVar != tc.stateVar {
				t.Errorf("state directory: got %s from %s, want %s from %s",
					d.state, d.stateVar, tc.state, tc.stateVar)
			}

			if d.runtime != tc.runtime || d.runtimeVar != tc.runtimeVar {
				t.Errorf("runtime directory: got %s from %s, want %s from %s",
					d.runtime, d.runtimeVar, tc.runtime, tc.runtimeVar)
			}
		})
	}
}

// The directories come from the environment the caller gives, never from
// the process's: each variable set in the process to another value leaves
// the lookup untouched. Not parallel: it sets the process's variables.
func TestStateDirIgnoresHostVariables(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "/host-state")
	t.Setenv("XDG_STATE_HOME", "/host-xdg")
	t.Setenv("HOME", "/host-home")
	t.Setenv("RUNTIME_DIRECTORY", "/host-run")
	t.Setenv("XDG_RUNTIME_DIR", "/host-xdg-run")

	d := lookupDirs([]string{"STATE_DIRECTORY=/s1", "XDG_STATE_HOME=/x", "HOME=/h"})

	if d.state != "/s1" || d.stateVar != "STATE_DIRECTORY" {
		t.Errorf("state directory: got %s from %s, want /s1 from STATE_DIRECTORY",
			d.state, d.stateVar)
	}

	if d.runtime != "/s1/run" || d.runtimeVar != "STATE_DIRECTORY" {
		t.Errorf("runtime directory: got %s from %s, want /s1/run from STATE_DIRECTORY",
			d.runtime, d.runtimeVar)
	}

	d = lookupDirs([]string{"RUNTIME_DIRECTORY=/r1"})

	if d.runtime != "/r1" || d.runtimeVar != "RUNTIME_DIRECTORY" {
		t.Errorf("runtime directory: got %s from %s, want /r1 from RUNTIME_DIRECTORY",
			d.runtime, d.runtimeVar)
	}
}

// A load with no file gives every key its default, and the three defaults
// that follow the directories follow the environment the caller gave:
// metadata.badger.path the state directory's metadata, logging.output its
// log, api.socket the runtime directory's socket, all sourced as defaults.
func TestDefaultsFollowDirectories(t *testing.T) {
	t.Parallel()

	environ := []string{"STATE_DIRECTORY=/s1", "RUNTIME_DIRECTORY=/s1/run2"}

	loaded, err := LoadWithSources("", environ)
	if err != nil {
		t.Fatalf("Load with no file: %v", err)
	}

	for _, key := range []string{"metadata.badger.path", "logging.output", "api.socket"} {
		if loaded.Source[key] != sourceDefault {
			t.Errorf("%s's source: got %q, want %q", key, loaded.Source[key], sourceDefault)
		}
	}

	if loaded.Metadata.Badger.Path != "/s1/metadata" {
		t.Errorf("metadata.badger.path: got %s, want /s1/metadata", loaded.Metadata.Badger.Path)
	}

	if loaded.Logging.Output != "/s1/dittofs.log" {
		t.Errorf("logging.output: got %s, want /s1/dittofs.log", loaded.Logging.Output)
	}

	if loaded.API.Socket != "/s1/run2/dfs.sock" {
		t.Errorf("api.socket: got %s, want /s1/run2/dfs.sock", loaded.API.Socket)
	}
}

// A variable that names a relative state directory refuses, naming the one
// that chose it, and an absolute HOME loads. The state and the runtime
// directory stay empty behind a refusal.
func TestLoadRefusesRelativeDirectory(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, variable string
	}{
		{"the state variable", "STATE_DIRECTORY"},
		{"the config home", "XDG_STATE_HOME"},
		{"the home", "HOME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want := "environment: " + tc.variable + ": must be an absolute path"

			_, err := Load("", []string{tc.variable + "=rel"})

			if err == nil || err.Error() != want {
				t.Errorf("Load with %s=rel: got %v, want %q", tc.variable, err, want)
			}

			d := lookupDirs([]string{tc.variable + "=rel"})

			if !d.stateRefused || d.stateVar != tc.variable || d.runtime != "" {
				t.Errorf("lookupDirs with %s=rel: got %s from %s, refused %t, runtime %s",
					tc.variable, d.state, d.stateVar, d.stateRefused, d.runtime)
			}
		})
	}

	t.Run("an absolute home loads", func(t *testing.T) {
		t.Parallel()

		if _, err := Load("", []string{"HOME=/h"}); err != nil {
			t.Errorf("Load with HOME=/h: %v", err)
		}
	})
}

// A variable that names a relative runtime directory refuses, naming the one
// that chose it, in Load, in Validate and in MakeRuntimeDir, and an absolute
// one is accepted. A variable a unit sets as a list is read for its first
// entry, so a relative first entry refuses and a relative later one does not.
// XDG_RUNTIME_DIR is one path: a colon is part of it.
func TestRefusesRelativeRuntimeDirectory(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, variable, value string
		refused               bool
	}{
		{"the runtime variable, absolute", "RUNTIME_DIRECTORY", "", false},
		{"the runtime variable, relative", "RUNTIME_DIRECTORY", "rel", true},
		{"the runtime variable, relative first entry", "RUNTIME_DIRECTORY", "rel:/r", true},
		{"the runtime variable, relative second entry", "RUNTIME_DIRECTORY", "/r:rel", false},
		{"the runtime home, absolute", "XDG_RUNTIME_DIR", "", false},
		{"the runtime home, relative", "XDG_RUNTIME_DIR", "rel", true},
		{"the runtime home, relative with a colon", "XDG_RUNTIME_DIR", "rel:/x", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A row with no value takes a directory of its own, so no row
			// creates anything outside the test's temp directory.
			value, wantDir := tc.value, tc.value
			if value == "" {
				value = t.TempDir()
				wantDir = value
			}

			if tc.variable == "XDG_RUNTIME_DIR" {
				wantDir = filepath.Join(value, "dittofs")
			}

			if tc.variable == "RUNTIME_DIRECTORY" {
				wantDir = firstEntry(value)
			}

			state := t.TempDir()
			environ := []string{"STATE_DIRECTORY=" + state, tc.variable + "=" + value}
			want := "environment: " + tc.variable + ": must be an absolute path"

			d := lookupDirs(environ)

			if d.runtimeRefused != tc.refused || d.stateRefused {
				t.Errorf("lookupDirs with %s=%s: runtime refused %t, want %t",
					tc.variable, value, d.runtimeRefused, tc.refused)
			}

			_, loadErr := Load("", environ)
			dir, makeErr := MakeRuntimeDir(environ)
			valErr := factsErr(handBuilt(), environ, processUser())

			if !tc.refused {
				if loadErr != nil {
					t.Errorf("Load with %s=%s: %v", tc.variable, value, loadErr)
				}

				if makeErr != nil || dir != wantDir {
					t.Errorf("MakeRuntimeDir with %s=%s: got %s and %v, want %s and no error",
						tc.variable, value, dir, makeErr, wantDir)
				}

				if valErr != nil && valErr.Error() == want {
					t.Errorf("Validate with %s=%s: refused: %v", tc.variable, value, valErr)
				}

				return
			}

			for _, c := range []struct {
				name string
				err  error
			}{
				{"Load", loadErr},
				{"MakeRuntimeDir", makeErr},
				{"Validate", valErr},
			} {
				if c.err == nil || c.err.Error() != want {
					t.Errorf(
						"%s with %s=%s: got %v, want %q",
						c.name,
						tc.variable,
						value,
						c.err,
						want,
					)
				}
			}
		})
	}
}

// Validate reads nothing at a relative runtime path: one that exists from
// where the process started, as a symlink, still gives the one refusal of
// its not being absolute, and no line about what sits at it. It runs in a
// temp directory it changes into, so it is not parallel.
func TestValidateReadsNothingAtRelativeRuntimeDir(t *testing.T) {
	for _, tc := range []struct {
		name, variable, value, link string
	}{
		{"the runtime variable", "RUNTIME_DIRECTORY", "rel", "rel"},
		{"the runtime home", "XDG_RUNTIME_DIR", "rel", "rel/dittofs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())

			if err := os.MkdirAll(filepath.Dir(tc.link), 0o700); err != nil {
				t.Fatal(err)
			}

			if err := os.Symlink(t.TempDir(), tc.link); err != nil {
				t.Fatal(err)
			}

			environ := []string{"STATE_DIRECTORY=" + t.TempDir(), tc.variable + "=" + tc.value}
			err := factsErr(handBuilt(), environ, processUser())

			want := "environment: " + tc.variable + ": must be an absolute path"
			if err == nil || err.Error() != want {
				t.Errorf("Validate with %s=%s: got %v, want %q", tc.variable, tc.value, err, want)
			}
		})
	}
}

// MakeRuntimeDir makes the fallback and the runtime home's dittofs at 0700,
// returns the service manager's directory untouched, and refuses a symlink
// on the path and a state directory it cannot resolve.
func TestMakeRuntimeDir(t *testing.T) {
	t.Parallel()

	state := t.TempDir()

	dir, err := MakeRuntimeDir([]string{"STATE_DIRECTORY=" + state})
	if err != nil {
		t.Fatalf("MakeRuntimeDir of the fallback: %v", err)
	}

	want := filepath.Join(state, "run")

	if dir != want {
		t.Errorf("fallback: got %s, want %s", dir, want)
	}

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat of the fallback: %v", err)
	}

	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("fallback mode: got %o, want 700", got)
	}

	again, err := MakeRuntimeDir([]string{"STATE_DIRECTORY=" + state})
	if err != nil || again != want {
		t.Errorf("second call: got %s and %v, want %s and no error", again, err, want)
	}

	xdg := t.TempDir()

	dir, err = MakeRuntimeDir([]string{"XDG_RUNTIME_DIR=" + xdg})
	if err != nil {
		t.Fatalf("MakeRuntimeDir of the runtime home: %v", err)
	}

	if want := filepath.Join(xdg, "dittofs"); dir != want {
		t.Errorf("runtime home: got %s, want %s", dir, want)
	}

	fi, err = os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat of the runtime home's dittofs: %v", err)
	}

	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("runtime home mode: got %o, want 700", got)
	}

	managed := filepath.Join(t.TempDir(), "never")

	dir, err = MakeRuntimeDir([]string{"RUNTIME_DIRECTORY=" + managed})
	if err != nil {
		t.Fatalf("MakeRuntimeDir of the managed directory: %v", err)
	}

	if dir != managed {
		t.Errorf("managed directory: got %s, want %s", dir, managed)
	}

	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Errorf("managed directory: it exists, want nothing created")
	}

	other := t.TempDir()

	if err := os.Symlink(other, filepath.Join(other, "run")); err != nil {
		t.Fatal(err)
	}

	_, err = MakeRuntimeDir([]string{"STATE_DIRECTORY=" + other})
	if err == nil || err.Error() != "environment: STATE_DIRECTORY: is a symlink" {
		t.Errorf(
			"a symlink on the fallback's path: got %v, want a refusal naming the variable",
			err,
		)
	}

	home := t.TempDir()

	if err := os.Symlink(home, filepath.Join(home, "dittofs")); err != nil {
		t.Fatal(err)
	}

	_, err = MakeRuntimeDir([]string{"XDG_RUNTIME_DIR=" + home})
	if err == nil || err.Error() != "environment: XDG_RUNTIME_DIR: is a symlink" {
		t.Errorf(
			"a symlink on the runtime home's path: got %v, want a refusal naming the variable",
			err,
		)
	}

	file := filepath.Join(t.TempDir(), "file")

	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = MakeRuntimeDir([]string{"XDG_RUNTIME_DIR=" + file})
	if err == nil || err.Error() != "environment: XDG_RUNTIME_DIR: cannot be read" {
		t.Errorf("a runtime home under a file: got %v, want a refusal naming the variable", err)
	}

	if os.Geteuid() != 0 {
		locked := t.TempDir()

		if err := os.Chmod(locked, 0o500); err != nil {
			t.Fatal(err)
		}

		_, err = MakeRuntimeDir([]string{"XDG_RUNTIME_DIR=" + locked})
		if err == nil || err.Error() != "environment: XDG_RUNTIME_DIR: is not writable" {
			t.Errorf(
				"a runtime home in a locked directory: got %v, want a refusal naming the variable",
				err,
			)
		}
	}

	_, err = MakeRuntimeDir([]string{"STATE_DIRECTORY=rel"})
	if err == nil ||
		err.Error() != "environment: STATE_DIRECTORY: must be an absolute path" {
		t.Errorf("a relative state directory: got %v, want the lookup's refusal", err)
	}
}

// FuzzDirLookup never panics over any environment, and a variable a unit
// sets as a colon-separated list gives its first entry, both directories
// and every level of their lookup. The fuzz input is the environment as
// the process writes it, one NAME=value entry per line.
func FuzzDirLookup(f *testing.F) {
	seeds := []string{
		"",
		"STATE_DIRECTORY=/s1:/s2\nRUNTIME_DIRECTORY=/r1:/r2\n",
		"STATE_DIRECTORY=/s1\nXDG_STATE_HOME=/x\nHOME=/h\n",
		"XDG_STATE_HOME=/x\nXDG_RUNTIME_DIR=/xrd\nHOME=\n",
		"RUNTIME_DIRECTORY=/r1\nSTATE_DIRECTORY=\nXDG_STATE_HOME=rel\nHOME=rel\n",
		":=:\n=\nSTATE_DIRECTORY=:=:\na=b=c\n",
		"XDG_RUNTIME_DIR=:\nSTATE_DIRECTORY=/s1\n",
	}

	for _, environ := range seeds {
		f.Add([]byte(environ))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		environ := strings.Split(string(raw), "\n")
		d := lookupDirs(environ)
		env := environMap(environ)

		if v := env["STATE_DIRECTORY"]; v != "" && d.stateVar == "STATE_DIRECTORY" {
			first, _, _ := strings.Cut(v, ":")

			if d.state != first {
				t.Errorf("state directory: got %s, want the first entry %s", d.state, first)
			}
		}

		if v := env["RUNTIME_DIRECTORY"]; v != "" && d.runtimeVar == "RUNTIME_DIRECTORY" {
			first, _, _ := strings.Cut(v, ":")

			if d.runtime != first {
				t.Errorf("runtime directory: got %s, want the first entry %s", d.runtime, first)
			}
		}

		// XDG_RUNTIME_DIR is one path, not a unit's list: a colon is part of it.
		if v := env["XDG_RUNTIME_DIR"]; v != "" && d.runtimeVar == "XDG_RUNTIME_DIR" {
			if d.runtime != filepath.Join(v, "dittofs") {
				t.Errorf("runtime directory: got %s, want %s", d.runtime,
					filepath.Join(v, "dittofs"))
			}
		}
	})
}

// The store directory's default is the badger store's: with another
// backend named the key stays unset, so a bootstrap read back from a file
// that writes down what Load returned still loads.
func TestDefaultBadgerPathOnlyForBadger(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		backend string
		extra   string
		want    string
	}{
		{"badger", "badger", "", "/s1/metadata"},
		{"foundationdb", "foundationdb", "  foundationdb:\n    cluster_file: /c\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			loaded, err := LoadWithSources(
				writeFile(t, "metadata:\n  backend: "+tc.backend+"\n"+tc.extra),
				[]string{"STATE_DIRECTORY=/s1"},
			)
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			if got := loaded.Metadata.Badger.Path; got != tc.want {
				t.Errorf("metadata.badger.path with backend %s: got %q, want %q",
					tc.backend, got, tc.want)
			}
		})
	}
}
