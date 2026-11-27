//go:build unix

package bootstrap

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// dirOf makes a directory of the given mode, chmod'ed so the umask does not
// change it.
func dirOf(t *testing.T, mode os.FileMode) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "state")

	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("make directory: %v", err)
	}

	if err := os.Chmod(dir, mode); err != nil {
		t.Fatalf("chmod directory: %v", err)
	}

	return dir
}

// validateWith validates a hand-built bootstrap with environ, as it is, and
// returns the report as text.
func validateWith(t *testing.T, environ []string) string {
	t.Helper()

	return validateAs(t, environ, processUser())
}

// validateAs is validateWith for a node whose user id is node.
func validateAs(t *testing.T, environ []string, node uint32) string {
	t.Helper()

	err := factsErr(handBuilt(), environ, node)
	if err == nil {
		return ""
	}

	return err.Error()
}

// The state directory that exists is checked: a link is followed by nobody,
// a world write bit is refused, a group write bit is not. A missing one
// passes, and nothing is made.
func TestStateDirChecks(t *testing.T) {
	t.Parallel()

	existing := dirOf(t, 0o755)
	link := filepath.Join(t.TempDir(), "link")

	if err := os.Symlink(existing, link); err != nil {
		t.Fatalf("link: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "missing")

	for _, tc := range []struct {
		name string
		dir  string
		want string
	}{
		{"a link to a directory", link, "environment: STATE_DIRECTORY: is a symlink"},
		{
			"writable by others", dirOf(t, 0o757),
			"environment: STATE_DIRECTORY: is writable by others",
		},
		{"writable by the group", dirOf(t, 0o775), ""},
		{"0755", dirOf(t, 0o755), ""},
		{"missing", missing, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := validateWith(t, []string{"STATE_DIRECTORY=" + tc.dir}); got != tc.want {
				t.Errorf("state directory %s:\ngot:\n%s\nwant:\n%s", tc.name, got, tc.want)
			}
		})
	}

	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Errorf("Validate made the missing state directory: %v", err)
	}
}

// A regular file where a state or runtime directory belongs is refused, by
// Validate and by MakeRuntimeDir, with its own phrase.
func TestFileInPlaceOfDirectory(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "file")

	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	for _, tc := range []struct {
		name     string
		environ  []string
		variable string
	}{
		{"the state directory", []string{"STATE_DIRECTORY=" + file}, "STATE_DIRECTORY"},
		{"the runtime directory", []string{"RUNTIME_DIRECTORY=" + file}, "RUNTIME_DIRECTORY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want := "environment: " + tc.variable + ": is not a directory"

			if got := validateWith(t, tc.environ); got != want {
				t.Errorf("Validate:\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}

	// MakeRuntimeDir reads $XDG_RUNTIME_DIR/dittofs, so the file sits there.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "dittofs"), nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	environ := []string{"STATE_DIRECTORY=" + t.TempDir(), "XDG_RUNTIME_DIR=" + home}

	_, err := MakeRuntimeDir(environ)

	const want = "environment: XDG_RUNTIME_DIR: is not a directory"
	if err == nil || err.Error() != want {
		t.Errorf("MakeRuntimeDir: got %v, want %q", err, want)
	}
}

// A runtime directory that exists is refused when group or others can write
// it, by Validate and by MakeRuntimeDir; one the node's user owns at 0700
// passes both, and a runtime directory of another user is refused (RFC 13
// Appendix C).
func TestRuntimeDirOwnerAndMode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mode os.FileMode
		want string
	}{
		{"0700", 0o700, ""},
		{"0755", 0o755, ""},
		{"0775", 0o775, "environment: XDG_RUNTIME_DIR: is writable by group or others"},
		{"0777", 0o777, "environment: XDG_RUNTIME_DIR: is writable by group or others"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			dir := filepath.Join(home, "dittofs")

			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatalf("make directory: %v", err)
			}

			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatalf("chmod directory: %v", err)
			}

			environ := []string{"STATE_DIRECTORY=" + t.TempDir(), "XDG_RUNTIME_DIR=" + home}

			if got := validateWith(t, environ); got != tc.want {
				t.Errorf("Validate:\ngot:\n%s\nwant:\n%s", got, tc.want)
			}

			_, err := MakeRuntimeDir(environ)

			if got := errText(err); got != tc.want {
				t.Errorf("MakeRuntimeDir:\ngot:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}

	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "dittofs"), 0o700); err != nil {
		t.Fatalf("make directory: %v", err)
	}

	// Root's state directory is let in whoever the node's user is.
	environ := []string{"STATE_DIRECTORY=/", "XDG_RUNTIME_DIR=" + home}

	const want = "environment: XDG_RUNTIME_DIR: is owned by another user"

	if got := validateAs(t, environ, processUser()+1); got != want {
		t.Errorf("another user's runtime directory:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// errText is an error's text, empty for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

// dirOwner reads the owner, not the group, of the facts a unix host gives.
func TestDirOwnerReadsUid(t *testing.T) {
	t.Parallel()

	owner, ok := dirOwner(fakeInfo{sys: &syscall.Stat_t{Uid: 1000, Gid: 2000}})
	if !ok || owner != 1000 {
		t.Errorf("dirOwner: got %d, %t, want 1000, true", owner, ok)
	}
}

// Validate reads the owner of the real state directory against the node's
// user it is given: another user's directory is refused, root's is not, and
// the root of the file system, which root owns, loads on any host.
func TestStateDirOwner(t *testing.T) {
	t.Parallel()

	dir := dirOf(t, 0o755)

	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	owner, ok := dirOwner(info)
	if !ok {
		t.Fatal("the host reports no owner of a directory")
	}

	want := "environment: STATE_DIRECTORY: is owned by another user"
	if owner == 0 {
		want = ""
	}

	environ := []string{"STATE_DIRECTORY=" + dir}

	if got := validateAs(t, environ, owner+1); got != want {
		t.Errorf("a directory of user %d, node user %d:\ngot:\n%s\nwant:\n%s",
			owner, owner+1, got, want)
	}

	if got := validateAs(t, environ, owner); got != "" {
		t.Errorf("the node user's own directory:\ngot:\n%s\nwant no refusal", got)
	}

	if got := validateAs(t, []string{"STATE_DIRECTORY=/", "RUNTIME_DIRECTORY=/no/such/run"}, owner+1); got != "" {
		t.Errorf("the root of the file system:\ngot:\n%s\nwant no refusal", got)
	}
}

// The runtime directory is refused for a link, the one the service manager
// names and the one under the state directory alike, and for a write bit of
// group or others; Validate makes nothing.
func TestRuntimeDirChecks(t *testing.T) {
	t.Parallel()

	state := dirOf(t, 0o700)
	target := dirOf(t, 0o700)

	managedLink := filepath.Join(t.TempDir(), "managed")
	if err := os.Symlink(target, managedLink); err != nil {
		t.Fatalf("link: %v", err)
	}

	plantedState := dirOf(t, 0o700)
	if err := os.Symlink(target, filepath.Join(plantedState, "run")); err != nil {
		t.Fatalf("plant the link: %v", err)
	}

	plain := dirOf(t, 0o700)

	groupWritable := dirOf(t, 0o775)

	for _, tc := range []struct {
		name    string
		environ []string
		want    string
	}{
		{
			"a link named by the service manager",
			[]string{"STATE_DIRECTORY=" + state, "RUNTIME_DIRECTORY=" + managedLink},
			"environment: RUNTIME_DIRECTORY: is a symlink",
		},
		{
			"a link planted under the state directory",
			[]string{"STATE_DIRECTORY=" + plantedState},
			"environment: STATE_DIRECTORY: is a symlink",
		},
		{
			"group writable",
			[]string{"STATE_DIRECTORY=" + state, "RUNTIME_DIRECTORY=" + groupWritable},
			"environment: RUNTIME_DIRECTORY: is writable by group or others",
		},
		{"a plain one", []string{"STATE_DIRECTORY=" + state, "RUNTIME_DIRECTORY=" + plain}, ""},
		{"a missing one", []string{"STATE_DIRECTORY=" + state}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := validateWith(t, tc.environ); got != tc.want {
				t.Errorf("runtime directory %s:\ngot:\n%s\nwant:\n%s", tc.name, got, tc.want)
			}
		})
	}

	before := listTree(t, state)

	validateWith(t, []string{"STATE_DIRECTORY=" + state})

	if after := listTree(t, state); !slices.Equal(before, after) {
		t.Errorf("Validate changed the state tree:\nbefore %q\nafter  %q", before, after)
	}
}

// listTree returns the names under dir, sorted.
func listTree(t *testing.T, dir string) []string {
	t.Helper()

	var names []string

	err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		names = append(names, path)

		return err
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	return names
}

// MakeRuntimeDir refuses a state directory no variable names and no home
// gives, as Validate does. Not parallel: it clears the process's HOME.
func TestMakeRuntimeDirRefusesUnnamedStateDir(t *testing.T) {
	t.Setenv("HOME", "")

	_, err := MakeRuntimeDir(nil)

	const want = "environment: HOME: is not set"
	if err == nil || err.Error() != want {
		t.Errorf("no variable names the state directory: got %v, want %q", err, want)
	}
}
