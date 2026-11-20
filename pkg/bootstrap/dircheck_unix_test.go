//go:build unix

package bootstrap

import (
	"context"
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

	err := handBuilt().validate(context.Background(), environ, distinctKeys(), node)
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

	if got := validateAs(t, []string{"STATE_DIRECTORY=/"}, owner+1); got != "" {
		t.Errorf("the root of the file system:\ngot:\n%s\nwant no refusal", got)
	}
}

// The runtime directory is refused for a link only, the one the service
// manager names and the one under the state directory alike; its owner and
// writers are not read here, and Validate makes nothing.
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
			"group writable, as the service manager makes it",
			[]string{"STATE_DIRECTORY=" + state, "RUNTIME_DIRECTORY=" + groupWritable},
			"",
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
