package commands

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dfs has the server's own commands, init, start and reset-admin, beside
// config, version and completion, and no other; start has no daemon flags.
func TestCommandTree(t *testing.T) {
	root := NewRoot()
	var got []string
	for _, c := range root.Commands() {
		if c.Name() != "help" {
			got = append(got, c.Name())
		}
	}
	slices.Sort(got)
	if want := []string{"completion", "config", "init", "reset-admin", "start", "version"}; !slices.Equal(got, want) {
		t.Errorf("dfs commands %q, want %q", got, want)
	}
	start, _, err := root.Find([]string{"start"})
	if err != nil {
		t.Fatalf("dfs start: %v", err)
	}
	for _, flag := range []string{"foreground", "pid-file", "log-file"} {
		if start.Flags().Lookup(flag) != nil {
			t.Errorf("dfs start has --%s, want no daemon flag", flag)
		}
	}
}

// dfs start fails, naming the step: it hands the node no KV yet, so building
// the node refuses.
func TestStartNamesTheStep(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "logging:\n  output: stderr\nblockstore:\n  journal:\n    path: '" + filepath.Join(t.TempDir(), "blocks") + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewRoot()
	root.SetArgs([]string{"start", "--config", path})
	err := root.Execute()
	if err == nil || !strings.HasPrefix(err.Error(), "building the node: ") {
		t.Errorf("dfs start: %v, want the node's build step refused", err)
	}
}

// Each command reads the flags it was run with from its own tree: start's
// --config, and config show's and config schema's --output.
func TestCommandsReadTheirFlags(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := runDfs("start", "--config", missing); err == nil || err.Error() != "configuration file not found: "+missing {
		t.Errorf("dfs start --config %s: %v, want the file named missing", missing, err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("logging:\n  output: stderr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runDfs("config", "show", "--config", path, "-o", "xml"); err == nil || !strings.HasPrefix(err.Error(), "invalid output format: ") {
		t.Errorf("dfs config show -o xml: %v, want the format refused", err)
	}

	schema := filepath.Join(t.TempDir(), "config.schema.json")
	out, err := runDfs("config", "schema", "-o", schema)
	if err != nil {
		t.Fatalf("dfs config schema -o %s: %v", schema, err)
	}
	if want := "JSON schema written to " + schema + "\n"; out != want {
		t.Errorf("dfs config schema -o %s: printed %q, want %q", schema, out, want)
	}
	if _, err := os.Stat(schema); err != nil {
		t.Errorf("dfs config schema -o %s: %v, want the schema written", schema, err)
	}
}

// No package under cmd/dfs keeps state in a package variable, so two roots
// built in one process share nothing; the build's version is the exception.
func TestNoPackageState(t *testing.T) {
	// The variables -ldflags sets, by the directory allowed to declare them:
	// lower case in main, where the linker writes them, and exported in
	// commands, which main copies them into.
	buildVars := map[string]map[string]bool{
		"..":                            {"version": true, "commit": true, "date": true},
		filepath.Join("..", "commands"): {"Version": true, "Commit": true, "Date": true},
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
					if !buildVars[filepath.Dir(path)][name.Name] {
						t.Errorf("%s: package-level var %s, want none but the build's version", fset.Position(name.Pos()), name.Name)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/dfs: %v", err)
	}
	if scanned < 10 {
		t.Fatalf("scanned %d files under cmd/dfs, want the whole tree", scanned)
	}
}
