package adaptertest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/cubbit/dittofs/"

// outside is what an adapter may import from outside the module and the
// standard library, in any file. An entry is added by the change that needs
// it, with its reason.
var outside = []string{
	"go.opentelemetry.io/otel/trace", // the tracing API Deps.Tracer names
}

// outsideTests is what an adapter's test files may import besides.
var outsideTests = []string{
	"github.com/pmezard/go-difflib/difflib", // the frozen surface's test prints a diff against its golden files
}

// fileIO is the standard library a wire package may not import, each with the
// packages below it, but for net/netip, which is values only: a codec is a
// pure function of bytes.
var fileIO = []string{"os", "io/ioutil", "net", "syscall", "plugin"}

// retired is the paths the first adapters used and these do not: no package
// may exist at one, so nothing can import it either.
var retired = []string{
	"pkg/controlplane/runtime/adapters", // the runtime's adapter half
	"internal/adapter/common",           // shared helpers, now core's or a protocol's own
	"internal/adapter/pool",             // the buffer pool, now pkg/resources
	"internal/adapter/testruntime",      // the test runtime, now adaptertest
	"internal/adapter/auth",             // sign-in, now the service's
	"internal/adapter/nfs/rpc",          // the RPC layer, now the protocol's stages
	"internal/adapter/smb/session",      // sessions, now the protocol's own state
	"internal/adapter/smb/handlers",     // handlers, now rows of the protocol's tables
}

// under reports whether p is root or a package below it.
func under(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }

// standard reports whether an import path is the standard library's: its
// first element has no dot.
func standard(imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	return !strings.Contains(first, ".")
}

// adapterOf names the adapter the package at dir belongs to: "core", "nfs",
// "smb", "adaptertest" or another directory of internal/adapter, or the
// protocol a constructor package builds; "" for a package that is no
// adapter's.
func adapterOf(dir string) string {
	if rest, ok := strings.CutPrefix(dir, "internal/adapter/"); ok {
		name, _, _ := strings.Cut(rest, "/")
		return name
	}
	if dir == "pkg/adapter/nfs" || dir == "pkg/adapter/smb" {
		return path.Base(dir)
	}

	return ""
}

// forbidden says why the file in dir, a test file or not, may not import imp,
// or "" when it may.
func forbidden(dir string, test bool, imp string) string {
	p, local := strings.CutPrefix(imp, module)
	if local && p == dir {
		return "" // a package's external tests import it
	}
	if local && !test && under(p, "internal/adapter/adaptertest") && !under(dir, "internal/adapter/adaptertest") {
		return "adaptertest and its test protocol are imported by test files only"
	}
	if name := adapterOf(dir); name != "" {
		return forbiddenToAnAdapter(dir, name, test, imp)
	}
	switch {
	case test:
		// A test file outside the adapters is held only to the pkg/ rule below.
		//
		// decision: it may import an implementation from outside pkg/, though
		// RFC 20 §2.5 names no test files, since it never reaches a binary's
		// build. Withdraw it if a package of test helpers outside
		// internal/adapter imports an implementation and non-test code
		// imports that package.
	case dir == "pkg/adapter/settings":
		if !standard(imp) {
			return "pkg/adapter/settings is a leaf: it imports the standard library only"
		}
	case dir == "pkg/adapter":
		ok := standard(imp) || slices.Contains(outside, imp) ||
			local && (p == "pkg/vfs" || p == "pkg/metrics" || p == "pkg/resources" || p == "pkg/adapter/settings")
		if !ok {
			return "pkg/adapter imports only the standard library, pkg/vfs, pkg/metrics, pkg/resources, pkg/adapter/settings and the tracing API"
		}
	}
	if local && under(dir, "pkg") && under(p, "internal/adapter") {
		return "no package under pkg/ but the two constructors imports an adapter's implementation"
	}
	// RFC 20 §2.5 and §3.5: outside pkg/ too, only the constructors import an
	// implementation, but for a sidecar that speaks a protocol's encoding,
	// which imports that protocol's wire package.
	sidecarWire := under(dir, "internal/sidecar") && path.Base(p) == "wire"
	if local && !test && under(p, "internal/adapter") && !sidecarWire {
		return "only the two constructors import an adapter's implementation; a sidecar may import a protocol's wire package"
	}

	return ""
}

// forbiddenToAnAdapter is forbidden for a package of the adapter name.
func forbiddenToAnAdapter(dir, name string, test bool, imp string) string {
	p, local := strings.CutPrefix(imp, module)
	wire := slices.Contains(strings.Split(dir, "/"), "wire")
	switch {
	// decision: a wire package's test files may import fileIO, since a codec's
	// test reads its vectors from files and the rule keeps the codec pure, not
	// its test. Withdraw it if a wire test opens a connection.
	case wire && !test && imp != "net/netip" && slices.ContainsFunc(fileIO, func(r string) bool { return under(imp, r) }):
		return "a wire package does no file or network I/O"
	case standard(imp):
		return ""
	case wire && test && local && (under(p, "pkg/vfs/vfstest") || under(p, "internal/prof") || under(p, "internal/adapter/adaptertest")):
		return "" // what every adapter test may add; a wire test reads its vectors from files
	case wire && (!local || !under(p, "pkg/resources")):
		return "a wire package imports only the standard library and pkg/resources"
	case !local:
		if slices.Contains(outside, imp) || test && slices.Contains(outsideTests, imp) {
			return ""
		}
		return "an adapter imports, from outside the module, only what its list names"
	case under(p, "pkg/vfs/vfstest"), under(p, "internal/prof"):
		if test || name == "adaptertest" {
			return ""
		}
		return "the service's test components and the profiler reach an adapter through its tests and adaptertest only"
	case under(p, "internal/adapter/adaptertest"):
		return "" // forbidden has refused it to a non-test file outside adaptertest
	case under(p, "internal/adapter/"+name), p == "internal/adapter/core" && !under(dir, "pkg"):
		return ""
	case under(p, "internal/adapter"):
		if name == "core" {
			return "core imports no protocol"
		}
		if under(dir, "pkg") {
			return "a constructor imports only its own protocol's implementation"
		}
		return "an adapter never imports another adapter"
	case p == "pkg/adapter/nfs", p == "pkg/adapter/smb":
		if under(dir, "pkg") {
			return "an adapter never imports another adapter"
		}
		return "an implementation package imports no constructor"
	case under(p, "pkg/vfs"), p == "pkg/adapter", p == "pkg/adapter/settings",
		under(p, "pkg/resources"), under(p, "pkg/metrics"), under(p, "internal/tlsconfig"):
		return ""
	}

	return "an adapter imports, of the module, only pkg/vfs, pkg/adapter, pkg/adapter/settings, pkg/resources, pkg/metrics, internal/tlsconfig, core and its own packages"
}

// violations walks every Go file under root, test files and files behind a
// build tag included, and names each import the adapters' architecture
// forbids and each package at a retired path. It leaves out what go builds no
// package of this module from: a directory below root named testdata or
// beginning with . or _, and another module, a directory holding a go.mod.
//
// ponytail: a package's path is read from its directory under root, so an
// import a replace directive or a vendored copy redirects would be missed.
// Load the packages, inside this test alone, if the module ever has either.
func violations(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, file)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if file == root {
				return nil
			}
			if n := d.Name(); n == "testdata" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(file, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		dir := path.Dir(rel)
		for _, old := range retired {
			if under(dir, old) {
				found = append(found, rel+": a package at "+old+", a path the first adapters used")
			}
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			found = append(found, rel+": its imports cannot be read: "+err.Error())
			return nil
		}
		for _, spec := range parsed.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if why := forbidden(dir, strings.HasSuffix(rel, "_test.go"), imp); why != "" {
				found = append(found, rel+": imports "+imp+": "+why)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)

	return found
}

// No file of the repository imports what the adapters' architecture forbids
// (RFC 20 §2.3, §2.5, §7.1), and nothing is exempt.
func TestImports(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the walk's root is not the module's: %v", err)
	}
	for _, v := range violations(t, root) {
		t.Error(v)
	}
}

// Over its seeds the check reports each violation once and no allowance: a
// rule that matches nothing, or one written so wide that it refuses an
// allowed import, fails here.
func TestImportCheckReportsEachSeededViolation(t *testing.T) {
	want := []string{
		"internal/adapter/auth/doc.go: a package at internal/adapter/auth, a path the first adapters used",
		"internal/adapter/common/doc.go: a package at internal/adapter/common, a path the first adapters used",
		"internal/adapter/core/metadata.go: imports github.com/cubbit/dittofs/pkg/metadata: an adapter imports, of the module, only pkg/vfs, pkg/adapter, pkg/adapter/settings, pkg/resources, pkg/metrics, internal/tlsconfig, core and its own packages",
		"internal/adapter/core/protocol.go: imports github.com/cubbit/dittofs/internal/adapter/nfs: core imports no protocol",
		"internal/adapter/core/protocol_test.go: imports github.com/cubbit/dittofs/internal/adapter/nfs: core imports no protocol",
		"internal/adapter/nfs/constructor.go: imports github.com/cubbit/dittofs/pkg/adapter/nfs: an implementation package imports no constructor",
		"internal/adapter/nfs/difflib.go: imports github.com/pmezard/go-difflib/difflib: an adapter imports, from outside the module, only what its list names",
		"internal/adapter/nfs/errgroup.go: imports golang.org/x/sync/errgroup: an adapter imports, from outside the module, only what its list names",
		"internal/adapter/nfs/identity.go: imports github.com/cubbit/dittofs/pkg/identity: an adapter imports, of the module, only pkg/vfs, pkg/adapter, pkg/adapter/settings, pkg/resources, pkg/metrics, internal/tlsconfig, core and its own packages",
		"internal/adapter/nfs/prof.go: imports github.com/cubbit/dittofs/internal/prof: the service's test components and the profiler reach an adapter through its tests and adaptertest only",
		"internal/adapter/nfs/rpc/doc.go: a package at internal/adapter/nfs/rpc, a path the first adapters used",
		"internal/adapter/nfs/service.go: imports github.com/cubbit/dittofs/pkg/vfs/vfstest: the service's test components and the profiler reach an adapter through its tests and adaptertest only",
		"internal/adapter/nfs/vfs_test.go: imports github.com/cubbit/dittofs/internal/vfs: an adapter imports, of the module, only pkg/vfs, pkg/adapter, pkg/adapter/settings, pkg/resources, pkg/metrics, internal/tlsconfig, core and its own packages",
		"internal/adapter/nfs/vfsx.go: imports github.com/cubbit/dittofs/pkg/vfsx: an adapter imports, of the module, only pkg/vfs, pkg/adapter, pkg/adapter/settings, pkg/resources, pkg/metrics, internal/tlsconfig, core and its own packages",
		"internal/adapter/nfs/wire/errgroup.go: imports golang.org/x/sync/errgroup: a wire package imports only the standard library and pkg/resources",
		"internal/adapter/nfs/wire/file.go: imports os: a wire package does no file or network I/O",
		"internal/adapter/nfs/wire/http.go: imports net/http: a wire package does no file or network I/O",
		"internal/adapter/nfs/wire/net.go: imports net: a wire package does no file or network I/O",
		"internal/adapter/nfs/wire/service.go: imports github.com/cubbit/dittofs/pkg/vfs: a wire package imports only the standard library and pkg/resources",
		"internal/adapter/nfs/wire/service_test.go: imports github.com/cubbit/dittofs/pkg/vfs: a wire package imports only the standard library and pkg/resources",
		"internal/adapter/pool/doc.go: a package at internal/adapter/pool, a path the first adapters used",
		"internal/adapter/smb/handlers/doc.go: a package at internal/adapter/smb/handlers, a path the first adapters used",
		"internal/adapter/smb/other.go: imports github.com/cubbit/dittofs/internal/adapter/nfs: an adapter never imports another adapter",
		"internal/adapter/smb/session/sub/doc.go: a package at internal/adapter/smb/session, a path the first adapters used",
		"internal/adapter/testruntime/doc.go: a package at internal/adapter/testruntime, a path the first adapters used",
		"internal/node/impl.go: imports github.com/cubbit/dittofs/internal/adapter/nfs: only the two constructors import an adapter's implementation; a sidecar may import a protocol's wire package",
		"internal/node/proto.go: imports github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto: adaptertest and its test protocol are imported by test files only",
		"internal/sidecar/portmap/impl.go: imports github.com/cubbit/dittofs/internal/adapter/nfs: only the two constructors import an adapter's implementation; a sidecar may import a protocol's wire package",
		"pkg/adapter/core.go: imports github.com/cubbit/dittofs/internal/adapter/core: pkg/adapter imports only the standard library, pkg/vfs, pkg/metrics, pkg/resources, pkg/adapter/settings and the tracing API",
		"pkg/adapter/errgroup.go: imports golang.org/x/sync/errgroup: pkg/adapter imports only the standard library, pkg/vfs, pkg/metrics, pkg/resources, pkg/adapter/settings and the tracing API",
		"pkg/adapter/nfs/core.go: imports github.com/cubbit/dittofs/internal/adapter/core: a constructor imports only its own protocol's implementation",
		"pkg/adapter/nfs/other.go: imports github.com/cubbit/dittofs/internal/adapter/smb: a constructor imports only its own protocol's implementation",
		"pkg/adapter/nfs/smb.go: imports github.com/cubbit/dittofs/pkg/adapter/smb: an adapter never imports another adapter",
		"pkg/adapter/settings/service.go: imports github.com/cubbit/dittofs/pkg/vfs: pkg/adapter/settings is a leaf: it imports the standard library only",
		"pkg/adapter/vfstest.go: imports github.com/cubbit/dittofs/pkg/vfs/vfstest: pkg/adapter imports only the standard library, pkg/vfs, pkg/metrics, pkg/resources, pkg/adapter/settings and the tracing API",
		"pkg/controlplane/runtime/adapters/doc.go: a package at pkg/controlplane/runtime/adapters, a path the first adapters used",
		"pkg/other/core.go: imports github.com/cubbit/dittofs/internal/adapter/core: no package under pkg/ but the two constructors imports an adapter's implementation",
		"pkg/other/core_test.go: imports github.com/cubbit/dittofs/internal/adapter/core: no package under pkg/ but the two constructors imports an adapter's implementation",
	}
	got := violations(t, filepath.Join("testdata", "imports"))
	for _, g := range got {
		if !slices.Contains(want, g) {
			t.Errorf("reported, and not a seeded violation:\n  %s", g)
		}
	}
	for _, w := range want {
		if n := len(slices.DeleteFunc(slices.Clone(got), func(g string) bool { return g != w })); n != 1 {
			t.Errorf("reported %d times, want once:\n  %s", n, w)
		}
	}
}
