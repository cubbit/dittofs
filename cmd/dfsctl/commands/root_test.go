package commands

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newTestRoot() (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	return NewRoot(cmdutil.Env{In: strings.NewReader(""), Out: &out, Err: &out}), &out
}

// Every v1 command dfsctl still lists refuses, naming itself, until the CLI is
// rebuilt on the v2 API; the commands that work locally (context, completion,
// version, logout) are left out of the walk.
func TestV1CommandsRefuse(t *testing.T) {
	kept := map[string]bool{"context": true, "completion": true, "version": true, "logout": true, "help": true}
	root, _ := newTestRoot()
	refused := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if c == root && kept[sub.Name()] {
				continue
			}
			if sub.Runnable() {
				refused++
				if sub.RunE == nil {
					t.Errorf("%s: no RunE, want the refusal", sub.CommandPath())
				} else if err := sub.RunE(sub, nil); err == nil || err.Error() != sub.CommandPath()+": not available until the CLI is rebuilt on the v2 API" {
					t.Errorf("%s: got %v, want the refusal naming the command", sub.CommandPath(), err)
				}
			}
			walk(sub)
		}
	}
	walk(root)
	if refused == 0 {
		t.Error("no command refused, want every v1 command")
	}
}

// A v1 command run as a user runs it, through the root with its flags and
// arguments parsed, exits with the refusal.
func TestV1CommandRefusesThroughExecute(t *testing.T) {
	for _, args := range [][]string{
		{"user", "list", "-o", "json"},
		{"share", "create", "--name", "/export", "--metadata", "default", "--block-store", "default"},
	} {
		root, _ := newTestRoot()
		root.SetArgs(args)
		want := "dfsctl " + strings.Join(args[:2], " ") + ": not available until the CLI is rebuilt on the v2 API"
		if err := root.Execute(); err == nil || err.Error() != want {
			t.Errorf("dfsctl %s: got %v, want %q", strings.Join(args, " "), err, want)
		}
	}
}

// The commands no API serves are not in the tree at all: running one is an
// unknown command, not a refusal.
func TestRemovedCommandsAbsent(t *testing.T) {
	root, _ := newTestRoot()
	for _, path := range []string{
		"switch-user",
		"share unmount",
		"share list-mounts",
		"share snapshot",
		"share snapshot-policy",
		"store block",
		"store metadata",
		"store check",
		"settings",
		"idmap",
		"identity-provider",
		"netlogon",
		"trash",
		"system",
	} {
		if c, _, err := root.Find(strings.Fields(path)); err == nil && c.CommandPath() == "dfsctl "+path {
			t.Errorf("dfsctl %s: got the command in the tree, want it absent", path)
		}
	}
}

// No package under cmd/dfsctl keeps state in a package variable, so two roots
// built in one process share nothing; the build's version is the exception.
func TestNoPackageState(t *testing.T) {
	// The variables -ldflags sets: lower case in main, where the linker writes
	// them, and exported in commands, which main copies them into.
	buildVars := map[string]bool{
		"version": true, "commit": true, "date": true,
		"Version": true, "Commit": true, "Date": true,
	}
	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		scanned++
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					if !buildVars[name.Name] {
						t.Errorf("%s: package-level var %s, want none but the build's version", fset.Position(name.Pos()), name.Name)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/dfsctl: %v", err)
	}
	if scanned < 20 {
		t.Fatalf("scanned %d files under cmd/dfsctl, want the whole tree", scanned)
	}
}
