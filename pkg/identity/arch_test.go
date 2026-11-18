package identity_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/cubbit/dittofs/"

// leaf is what pkg/identity imports of the module: the store's value types,
// the SID codec and the filesystem's error values. Anything else outside the
// standard library is refused.
var leaf = []string{module + "pkg/metadata/entity", module + "pkg/auth/sid", module + "pkg/vfs/vfserr"}

const (
	server = "internal/identity"
	ldap   = "github.com/go-ldap/ldap/v3"
)

// under reports whether p is root or a package below it.
func under(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }

// standard reports whether an import path is the standard library's: its
// first element has no dot.
func standard(imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	return !strings.Contains(first, ".")
}

// forbidden says why a non-test file of the package at dir, relative to the
// module's root, may not import imp, or "" when it may.
func forbidden(dir, imp string) string {
	switch {
	case dir == "pkg/identity" && !standard(imp) && !slices.Contains(leaf, imp):
		return "pkg/identity is a leaf: it imports only the standard library, entity, pkg/auth/sid and vfserr"
	case dir == "pkg/vfs/vfstest/memory" && imp == module+server+"/identitymem":
		return ""
	// identitytest hands tests identity's directory and NETLOGON clients, so
	// it imports those two and never the server.
	case dir == "pkg/identity/identitytest" &&
		(under(imp, module+server+"/directory") || under(imp, module+server+"/netlogon")):
		return ""
	case under(imp, module+server) && dir != "internal/node" && !under(dir, server):
		return "only internal/node and identity's own packages import the identity server"
	// decision: the online domain join in internal/auth/netlogon still speaks
	// LDAP itself; it is exempt until the package moves under
	// internal/identity, where its LDAP calls go through the directory client,
	// and the exemption goes with the move. It names one package, so no new
	// importer hides behind it.
	case under(imp, ldap) && dir != server+"/directory" && dir != "internal/auth/netlogon":
		return "only internal/identity/directory and internal/auth/netlogon import LDAP"
	}
	return ""
}

// violations parses every non-test Go file under root, leaving out what go
// builds no package of this module from (a directory named testdata or
// beginning with . or _, and a nested module), and names each import
// identity's layout forbids and each file whose imports cannot be read.
func violations(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		// decision: test files are exempt from all three rules: a test may
		// build a real server or talk to a directory, and the rules govern
		// what ships. Withdraw it if a non-test package starts importing a
		// test helper that imports the server or LDAP.
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			found = append(found, rel+": its imports cannot be read: "+err.Error())
			return nil
		}
		for _, imp := range f.Imports {
			ip, _ := strconv.Unquote(imp.Path.Value)
			if why := forbidden(path.Dir(rel), ip); why != "" {
				found = append(found, rel+": imports "+ip+": "+why)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	return found
}

// pkg/identity is a leaf, only the node and identity's own packages import
// the server, and only the directory client imports LDAP.
func TestImports(t *testing.T) {
	for _, v := range violations(t, filepath.Join("..", "..")) {
		t.Error(v)
	}
}

// The check reports each of its seeded violations, once, and nothing else.
func TestImportCheckReportsEachSeededViolation(t *testing.T) {
	got := violations(t, filepath.Join("testdata", "arch"))
	want := []string{
		"internal/adapter/smb/directory.go: imports " + module + "internal/identity/directory: only internal/node and identity's own packages import the identity server",
		"internal/adapter/smb/ldap.go: imports " + ldap + ": only internal/identity/directory and internal/auth/netlogon import LDAP",
		"internal/node/nodetest/nodetest.go: imports " + module + "internal/identity/directory: only internal/node and identity's own packages import the identity server",
		"internal/vfs/server.go: imports " + module + "internal/identity: only internal/node and identity's own packages import the identity server",
		"pkg/identity/identitytest/srv.go: imports " + module + "internal/identity: only internal/node and identity's own packages import the identity server",
		"pkg/identity/store.go: imports " + module + "pkg/metadata/kvstore: pkg/identity is a leaf: it imports only the standard library, entity, pkg/auth/sid and vfserr",
		"pkg/identity/uuid.go: imports github.com/google/uuid: pkg/identity is a leaf: it imports only the standard library, entity, pkg/auth/sid and vfserr",
		"pkg/identity/vfs.go: imports " + module + "pkg/vfs: pkg/identity is a leaf: it imports only the standard library, entity, pkg/auth/sid and vfserr",
		"pkg/vfs/vfstest/memory/memory.go: imports " + module + "internal/identity: only internal/node and identity's own packages import the identity server",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the check reported\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A file whose imports do not parse is reported, not skipped. It is written at
// run time because gofmt would refuse it as a seed under testdata.
func TestImportCheckReportsAnUnreadableFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "broken.go"), []byte("package broken\n\nimport (\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := violations(t, root)
	if len(got) != 1 || !strings.HasPrefix(got[0], "broken.go: its imports cannot be read: ") {
		t.Errorf("the check reported %q, want one line saying broken.go's imports cannot be read", got)
	}
}
