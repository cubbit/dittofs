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

// asNobody starts a child as nobody, with no supplementary group.
var asNobody = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobody, Gid: nobody}}

// Every row of the CLI suite passes from a dfsctl child run as a second OS
// user, nobody, who can reach neither the node's data directory, its key
// files among it, nor the bootstrap at the child's default path, each
// checked first: a command that opened the node's files or read the
// bootstrap fails. The
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
	// dt builds dfsctl as the user and names its directory, so root never
	// runs go build; a run by hand builds it here.
	if dir := os.Getenv("DITTOFS_E2E_BIN"); dir != "" {
		b, err := os.ReadFile(filepath.Join(dir, "dfsctl"))
		if err != nil {
			t.Fatalf("dfsctl from DITTOFS_E2E_BIN: %v", err)
		}
		if err := os.WriteFile(bin, b, 0o755); err != nil {
			t.Fatal(err)
		}
	} else if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
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
			clisuite.Run(t, func(t *testing.T) clisuite.Runner { return secondUser(t, bin, data, s.Harness) }, s, noun)
		})
	}
}

// secondUser is a child runner whose child runs as nobody in a home of its
// own, the bootstrap's default path there a file only root may read, and
// no file under data handed to it.
func secondUser(t *testing.T, bin, data string, h *nodetest.Harness) clisuite.Runner {
	home := poisoned(t, sharedDir(t, 0o700))
	for _, d := range []string{home, filepath.Join(home, ".config")} {
		if err := os.Chown(d, nobody, nobody); err != nil {
			t.Fatal(err)
		}
	}
	unreadable(t, "cat", filepath.Join(home, ".config", "dittofs", "config.yaml"))
	return child(bin, home, h, func(cmd *exec.Cmd) error {
		cmd.SysProcAttr = asNobody
		return handOver(cmd.Args, home, data)
	})
}

// handOver replaces each file a "-file" flag names in args with a copy in
// home owned by nobody: a row writes its secrets under its own temporary
// directory, which only root may read. A file under data, the node's, is
// refused rather than handed over, so no row can pass the node's files to
// the child.
//
// ponytail: only a file given as the argument after a flag ending in -file
// is handed over, the form every row uses; a --flag=path argument stays
// root's and its row fails on permission denied. Upgrade to a directory the
// runner gives the rows to write their files in when a row needs another form.
func handOver(args []string, home, data string) error {
	for i := 1; i < len(args); i++ {
		if !strings.HasSuffix(args[i-1], "-file") {
			continue
		}
		if strings.HasPrefix(args[i], data+string(filepath.Separator)) {
			return fmt.Errorf("%s is the node's, not a row's to hand the child", args[i])
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

// sharedDir is a directory of mode perm, removed when the test ends.
func sharedDir(t *testing.T, perm os.FileMode) string {
	t.Helper()
	// decision: made outside t.TempDir(), against the rule that a test writes
	// only under it, because its root is 0700 and root's, closed to nobody,
	// who must reach the binary and its home. Withdraw it if the test runs as
	// a user whose temporary directories nobody may enter.
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
	cmd.SysProcAttr, cmd.Env = asNobody, []string{"LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Permission denied") {
		t.Fatalf("%s %s as uid %d: %v, %q, want Permission denied", tool, path, nobody, err, out)
	}
}
