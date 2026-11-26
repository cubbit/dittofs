package benchdir

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scratchRepo is a git checkout in a temporary directory with one commit.
func scratchRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()

		cmd := exec.Command("git", append([]string{
			"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false",
		}, args...)...)
		cmd.Dir = dir

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	git("init", "-q")
	git("add", "a.go")
	git("commit", "-q", "-m", "first")

	return dir
}

func TestGitCommitDirty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		touch func(dir string) error
		dirty bool
	}{
		{"clean", func(string) error {
			return nil
		}, false},
		{"untracked", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "b_test.go"), []byte("package a\n"), 0o600)
		}, true},
		{"modified", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "a.go"), []byte("package b\n"), 0o600)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := scratchRepo(t)
			if err := tc.touch(dir); err != nil {
				t.Fatal(err)
			}

			got := commitIn(dir)
			if tc.dirty != strings.HasSuffix(got, "-dirty") {
				t.Fatalf("commitIn = %q, want dirty %v", got, tc.dirty)
			}
		})
	}

	// The ceiling keeps git from finding a checkout above the temporary
	// directory, when TMPDIR is inside one.
	outside := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(outside))

	if got := commitIn(outside); got != "unknown" {
		t.Fatalf("commitIn outside a checkout = %q, want %q", got, "unknown")
	}
}
