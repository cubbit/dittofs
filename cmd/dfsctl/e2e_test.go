//go:build e2e && unix

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/cmd/dfsctl/internal/clisuite"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/internal/prof"
)

// nobody is the second user's uid and gid, a user no file of the test's
// belongs to, so it reads only what the test hands it.
const nobody = 65534

// Every row of the CLI suite passes from a dfsctl child run as a second OS
// user, nobody, to whom the node's data directory, its key files and the
// bootstrap at the child's default path are unreadable, each checked first:
// a command that opened the node's files or read the bootstrap fails. The
// rows' own setup and reads run in this process, as root, as they do in
// TestSuiteFromAChildProcess. The child owns its home and nothing else of the
// test's; the API's socket is handed to it, as an operator grants a user the
// API, and a file a row names on the command line is copied into its home.
func TestSecondUser(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("second user: needs Linux and root, to start dfsctl as uid and gid 65534; dt runs e2e so")
	}
	prof.Watch(t, prof.Options{})
	bin := filepath.Join(sharedDir(t, 0o755), "dfsctl")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build dfsctl: %v\n%s", err, out)
	}
	for _, noun := range clisuite.Nouns() {
		t.Run(noun, func(t *testing.T) {
			s := clisuite.Start(t, nodetest.StartAt(time.Now().Add(-10*time.Minute)))
			if err := os.Chmod(filepath.Dir(s.Addr), 0o711); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(s.Addr, nobody, nobody); err != nil {
				t.Fatal(err)
			}
			data := filepath.Dir(s.Harness.StorageKeyFile())
			unreadable(t, "ls", data)
			unreadable(t, "cat", s.Harness.StorageKeyFile())
			clisuite.Run(t, func(t *testing.T) clisuite.Runner { return secondUser(t, bin, s.Harness) }, s, noun)
		})
	}
}

// secondUser is a child runner whose child runs as nobody in a home of its
// own, the bootstrap's default path there a file only root may read.
func secondUser(t *testing.T, bin string, h *nodetest.Harness) clisuite.Runner {
	home := poisoned(t, sharedDir(t, 0o700))
	for _, d := range []string{home, filepath.Join(home, ".config")} {
		if err := os.Chown(d, nobody, nobody); err != nil {
			t.Fatal(err)
		}
	}
	bootstrap := filepath.Join(home, ".config", "dittofs", "config.yaml")
	if err := os.Chmod(bootstrap, 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable(t, "cat", bootstrap)
	return child(bin, home, h, func(cmd *exec.Cmd) error {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobody, Gid: nobody, Groups: []uint32{}}}
		return handOver(cmd.Args, home)
	})
}

// handOver replaces each file a "-file" flag names in args with a copy in
// home owned by nobody: a row writes its secrets under its own temporary
// directory, which only root may read.
func handOver(args []string, home string) error {
	for i := 1; i < len(args); i++ {
		if !strings.HasSuffix(args[i-1], "-file") {
			continue
		}
		b, err := os.ReadFile(args[i])
		if err != nil {
			continue // not a file: the command reports it as the row expects
		}
		f, err := os.CreateTemp(home, "arg")
		if err != nil {
			return err
		}
		_, werr := f.Write(b)
		if err := errors.Join(werr, f.Close(), os.Chown(f.Name(), nobody, nobody)); err != nil {
			return fmt.Errorf("hand %s to the child: %w", args[i], err)
		}
		args[i] = f.Name()
	}
	return nil
}

// sharedDir is a directory of mode perm outside t.TempDir(), whose parents
// only root may enter, removed when the test ends.
func sharedDir(t *testing.T, perm os.FileMode) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "second-user")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, perm); err != nil {
		t.Fatal(err)
	}
	return dir
}

// unreadable fails t unless tool, run on path as nobody, is refused it.
func unreadable(t *testing.T, tool, path string) {
	t.Helper()
	cmd := exec.Command(tool, path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobody, Gid: nobody, Groups: []uint32{}}}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Permission denied") {
		t.Fatalf("%s %s as uid %d: %v, %q, want Permission denied", tool, path, nobody, err, out)
	}
}
