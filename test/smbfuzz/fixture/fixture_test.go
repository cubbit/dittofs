//go:build linux

package fixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both mounts get the options DESIGN.md §3.4 requires; the profile and the
// authentication change only their own options.
func TestMountOptions(t *testing.T) {
	c := Config{Auth: "ntlm", Profile: "sd"}
	o := c.mountOpts(445)
	for _, want := range []string{"vers=3.1.1", "multiuser", "sec=ntlmssp", "noperm", "cache=none", "actimeo=0",
		"nolease", "nohandlecache", "rsize=1048576", "wsize=1048576", "serverino", "soft"} {
		if !strings.Contains(","+o+",", ","+want+",") {
			t.Errorf("missing %s in %s", want, o)
		}
	}
	if strings.Contains(o, "cifsacl") || strings.Contains(o, "nobrl") || strings.Contains(o, "port=") {
		t.Errorf("unexpected option in %s", o)
	}
	c = Config{Auth: "krb5", Profile: "posix"}
	o = c.mountOpts(smbPort)
	for _, want := range []string{"sec=krb5", "cifsacl", "port=12445"} {
		if !strings.Contains(o, want) {
			t.Errorf("missing %s in %s", want, o)
		}
	}
}

// CleanRef refuses, by itself, a name that is not one round directory: any
// other error, such as docker finding no reference container, is not the
// refusal under test.
func TestCleanRefRefusesEscapes(t *testing.T) {
	h := Hooks{}
	for _, d := range []string{"", "../x", "a/b", ".", ".."} {
		if err := h.CleanRef(d); err == nil || !strings.HasPrefix(err.Error(), "refusing to remove") {
			t.Errorf("CleanRef(%q) = %v, want a refusal", d, err)
		}
	}
}

// The state directory is made private, and one already there is used only
// when it is a real directory nobody else can write to, in a parent where
// nobody else can replace it.
func TestStateDirIsPrivate(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "state")
	if err := stateDir(dir, true); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(dir); err != nil || fi.Mode().Perm()&0o022 != 0 {
		t.Fatalf("made %v, %v", fi.Mode(), err)
	}
	if err := stateDir(dir, false); err != nil {
		t.Fatalf("its own directory: %v", err)
	}

	link := filepath.Join(parent, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := stateDir(link, true); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("a symbolic link: %v, want it refused as not a directory", err)
	}

	open := filepath.Join(parent, "open")
	if err := os.Mkdir(open, 0o700); err != nil || os.Chmod(open, 0o777) != nil {
		t.Fatal(err)
	}
	if err := stateDir(open, true); err == nil {
		t.Error("used a directory others can write to")
	}

	// Inside a directory others can write to and that is not sticky, anyone
	// could replace the state directory.
	if err := stateDir(filepath.Join(open, "state"), true); err == nil {
		t.Error("used a directory in a parent others can write to")
	}
	if err := os.Chmod(open, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := stateDir(filepath.Join(open, "sticky"), true); err != nil {
		t.Errorf("in a sticky parent: %v", err)
	}
}
