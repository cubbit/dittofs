package bootstrap

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// testOnlyEnvNames are names only tests read: tests, scripts and people
// running them set them for go test, and dfs start never reads one, so the
// host keeps refusing each of them while the scan passes them in the
// repository's files. A name leaves the list once no file uses it any more.
var testOnlyEnvNames = []string{
	"DITTOFS_MODEL_SEEDS",
	"DITTOFS_MODEL_SEED_BASE",
	"DITTOFS_SOAK_DURATION",
	"DITTOFS_AD_DC_PAC_OUT",
	"DITTOFS_AD_DC_NAME_PREFIX",
	"DITTOFS_BENCH_SCALE",
	"DITTOFS_MIT_PAC_OUT",
}

// notExportedEnvNames are the allowed names the start, the key provider and
// dfsctl read themselves, so no launcher has to export them.
var notExportedEnvNames = []string{
	"DITTOFS_CONFIG",
	"DITTOFS_ENCRYPTION_PASSPHRASE",
	"DITTOFS_PASSWORD",
}

// driftExemptPrefixes and driftExemptFiles are the paths whose names the
// scan does not check: plans and internals keep the history of the names,
// the operator is its own program, renamed_keys.go holds the table of
// refused names, and dtc matches every name by a pattern.
var (
	driftExemptPrefixes = []string{
		"docs/internals/",
		".planning/",
		"k8s/dittofs-operator/",
	}
	driftExemptFiles = []string{
		"CHANGELOG.md",
		"pkg/bootstrap/renamed_keys.go",
		"test/harness/bin/dtc",
	}
)

// driftNamePattern finds a start's name that is not the tail of another
// identifier.
var driftNamePattern = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(DITTOFS_[A-Za-z0-9_]*)`)

// driftTextProbe is how many leading bytes decide that a file is text: a
// NUL byte among them marks a binary.
const driftTextProbe = 8000

// driftLists are the lists whose names the scan checks against the files:
// provisional names need a Go reader, exported names a file that exports
// them, test-only names a file that sets them and a test that reads them.
type driftLists struct {
	provisional []string
	exported    []string
	testOnly    []string
}

// driftResult is what a scan found: one line per finding, and how many
// names each file it read holds.
type driftResult struct {
	findings []string
	names    map[string]int
}

// driftUses is where the scan saw each name outside the exempt paths and
// pkg/bootstrap.
type driftUses struct {
	named       map[string]bool
	prodLiteral map[string]bool
	testLiteral map[string]bool
}

// scanDrift reads each listed file under root and reports every name the
// start refuses, with its file, line and rule, then every listed name whose
// users are gone.
func scanDrift(root string, files []string, lists driftLists) (driftResult, error) {
	result := driftResult{
		names: map[string]int{},
	}
	uses := driftUses{
		named:       map[string]bool{},
		prodLiteral: map[string]bool{},
		testLiteral: map[string]bool{},
	}

	for _, file := range files {
		if err := scanDriftFile(root, file, lists.testOnly, &result, uses); err != nil {
			return driftResult{}, err
		}
	}

	for _, name := range lists.provisional {
		if !uses.prodLiteral[name] {
			result.findings = append(result.findings,
				name+": provisional name with no reader left; remove it from the allowed names")
		}
	}

	for _, name := range lists.exported {
		if !uses.named[name] {
			result.findings = append(result.findings,
				name+": allowed name no tracked file exports; remove it from the allowed names")
		}
	}

	for _, name := range lists.testOnly {
		if !uses.named[name] || !uses.testLiteral[name] {
			result.findings = append(result.findings,
				name+": test-only name with no user left; remove it from the test-only list")
		}
	}

	return result, nil
}

// scanDriftFile reads one file and records its findings, its names and
// where it uses them. A file that is not a regular one, or a binary, is not
// read.
func scanDriftFile(
	root, file string,
	testOnly []string,
	result *driftResult,
	uses driftUses,
) error {
	path := filepath.Join(root, filepath.FromSlash(file))

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	content, err := readDriftText(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}

	if content == nil || !bytes.Contains(content, []byte(envPrefix)) {
		return nil
	}

	exempt := driftExempt(file)
	isTest := strings.HasSuffix(file, "_test.go")
	counted := !exempt && !strings.HasPrefix(file, "pkg/bootstrap/")

	for number, line := range bytes.Split(content, []byte("\n")) {
		for _, name := range driftNames(string(line)) {
			result.names[file]++

			if exempt || isTest {
				continue
			}

			if counted {
				uses.named[name] = true
			}

			if rule, refused := driftRule(name, testOnly); refused {
				result.findings = append(result.findings,
					fmt.Sprintf("%s:%d: %s: %s", file, number+1, name, rule))
			}
		}
	}

	if counted && strings.HasSuffix(file, ".go") {
		literals := uses.prodLiteral
		if isTest {
			literals = uses.testLiteral
		}

		for _, name := range goLiteralNames(content) {
			literals[name] = true
		}
	}

	return nil
}

// readDriftText reads a whole file, or returns nil for a binary one.
func readDriftText(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = f.Close()
	}()

	reader := bufio.NewReaderSize(f, driftTextProbe)

	head, err := reader.Peek(driftTextProbe)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	if bytes.IndexByte(head, 0) >= 0 {
		return nil, nil
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	if content == nil {
		content = []byte{}
	}

	return content, nil
}

// driftExempt reports whether the scan leaves the file's names unchecked.
func driftExempt(file string) bool {
	if slices.Contains(driftExemptFiles, file) {
		return true
	}

	return slices.ContainsFunc(driftExemptPrefixes, func(prefix string) bool {
		return strings.HasPrefix(file, prefix)
	})
}

// driftNames are the start's names on one line, in order, repeats kept.
func driftNames(line string) []string {
	matches := driftNamePattern.FindAllStringSubmatch(line, -1)
	names := make([]string, 0, len(matches))

	for _, match := range matches {
		names = append(names, match[1])
	}

	return names
}

// driftRule is the rule the scan reports for a name, and whether it
// refuses it: the bare prefix and a test-only name pass, the rest as the
// start classifies them.
func driftRule(name string, testOnly []string) (string, bool) {
	if name == envPrefix || slices.Contains(testOnly, name) {
		return "", false
	}

	class, rule := classifyEnv(name, "", declaredEnv)

	switch {
	case class < envSetting:
		return "", false
	case class == envUnknown:
		return rule + " If only tests and scripts read it, add it to testOnlyEnvNames " +
			"in pkg/bootstrap/drift_test.go", true
	default:
		return rule, true
	}
}

// goLiteralNames are the start's names inside the Go source's string
// literals; comments are not read.
func goLiteralNames(src []byte) []string {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))

	var s scanner.Scanner

	s.Init(file, src, nil, 0)

	var names []string

	for {
		_, tok, literal := s.Scan()
		if tok == token.EOF {
			return names
		}

		if tok != token.STRING {
			continue
		}

		if value, err := strconv.Unquote(literal); err == nil {
			literal = value
		}

		names = append(names, driftNames(literal)...)
	}
}

// launcherEnvNames are the allowed names a launcher exports.
func launcherEnvNames() []string {
	var names []string

	for _, name := range allowedEnvNames {
		if !slices.Contains(notExportedEnvNames, name) {
			names = append(names, name)
		}
	}

	return names
}

// plantDrift writes the files under a new directory and returns it with
// the files' paths, sorted.
func plantDrift(t *testing.T, files map[string]string) (string, []string) {
	t.Helper()

	root := t.TempDir()
	paths := make([]string, 0, len(files))

	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}

		paths = append(paths, name)
	}

	slices.Sort(paths)

	return root, paths
}

// scanPlanted scans the planted files with the given lists and fails the
// test on an error.
func scanPlanted(t *testing.T, files map[string]string, lists driftLists) []string {
	t.Helper()

	root, paths := plantDrift(t, files)

	result, err := scanDrift(root, paths, lists)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	return result.findings
}

// wantFindings fails the test unless the findings are exactly want.
func wantFindings(t *testing.T, got, want []string) {
	t.Helper()

	if !slices.Equal(got, want) {
		t.Errorf("findings:\n got %q\nwant %q", got, want)
	}
}

func TestScanRefusesANameInAShellFileWithoutExtension(t *testing.T) {
	t.Parallel()

	got := scanPlanted(t, map[string]string{
		"scripts/launch": "#!/bin/sh\nset -e\nDITTOFS_CONTROLPLANE_SECRET=x dfs start\n",
	}, driftLists{})

	wantFindings(t, got, []string{
		"scripts/launch:3: DITTOFS_CONTROLPLANE_SECRET: no longer read; remove it",
	})
}

func TestScanPassesEachExemption(t *testing.T) {
	t.Parallel()

	for _, file := range []string{
		"docs/internals/x.md",
		".planning/x.md",
		"CHANGELOG.md",
		"pkg/bootstrap/renamed_keys.go",
		"k8s/dittofs-operator/x.go",
		"test/harness/bin/dtc",
		"cmd/x/x_test.go",
	} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			got := scanPlanted(t, map[string]string{
				file: "DITTOFS_CONTROLPLANE_SECRET=x\n",
			}, driftLists{})

			wantFindings(t, got, nil)
		})
	}

	t.Run("bare prefix", func(t *testing.T) {
		t.Parallel()

		got := scanPlanted(t, map[string]string{
			"docs/guide/x.md": "Every DITTOFS_* name, and DITTOFS_ alone.\n",
		}, driftLists{})

		wantFindings(t, got, nil)
	})
}

func TestScanRefusesTheSameNameOutsideAnExemption(t *testing.T) {
	t.Parallel()

	for _, file := range []string{
		"docs/internals-other/x.md",
		"docs/CHANGELOG.md",
		"docs/.planning/x.md",
		".planning-other/x.md",
		"other/renamed_keys.go",
		"other/dtc",
		"x_test.go.md",
		"k8s/dittofs-operator-x/x.go",
	} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			got := scanPlanted(t, map[string]string{
				file: "DITTOFS_CONTROLPLANE_SECRET=x\n",
			}, driftLists{})

			wantFindings(t, got, []string{
				file + ":1: DITTOFS_CONTROLPLANE_SECRET: no longer read; remove it",
			})
		})
	}

	t.Run("prefix literal", func(t *testing.T) {
		t.Parallel()

		got := scanPlanted(t, map[string]string{
			"docs/guide/x.md": "Names under DITTOFS_LDAP_ are gone.\n",
		}, driftLists{})

		wantFindings(t, got, []string{
			"docs/guide/x.md:1: DITTOFS_LDAP_: no longer read; a record or a setting, " +
				"set it through the management API",
		})
	})

	t.Run("a setting's variable", func(t *testing.T) {
		t.Parallel()

		got := scanPlanted(t, map[string]string{
			"scripts/x.sh": "DITTOFS_ADAPTERS_NFS_PORT=2049\n",
		}, driftLists{})

		wantFindings(t, got, []string{
			"scripts/x.sh:1: DITTOFS_ADAPTERS_NFS_PORT: a setting, not a host fact; " +
				"set it through the management API",
		})
	})
}

func TestScanPassesKeysAllowedNamesAndPrefixes(t *testing.T) {
	t.Parallel()

	got := scanPlanted(t, map[string]string{
		"scripts/x.sh": "DITTOFS_LOGGING_LEVEL=debug DITTOFS_CONFIG=c\n" +
			"DITTOFS_SKIP_LINT=1 DITTOFS_TEST_X=1\n" +
			"DITTOFS_SKIP_TLS_VERIFY=1\n",
	}, driftLists{})

	wantFindings(t, got, []string{
		"scripts/x.sh:3: DITTOFS_SKIP_TLS_VERIFY: unknown name; did you mean " +
			closestEnvName("DITTOFS_SKIP_TLS_VERIFY") + "? If only tests and scripts read it, " +
			"add it to testOnlyEnvNames in pkg/bootstrap/drift_test.go",
	})
}

func TestScanIgnoresABinaryFile(t *testing.T) {
	t.Parallel()

	got := scanPlanted(t, map[string]string{
		"bin/dfs": "\x7fELF\x00\x00DITTOFS_CONTROLPLANE_SECRET\n",
	}, driftLists{})

	wantFindings(t, got, nil)
}

func TestScanFindsEachNameOnALineOnce(t *testing.T) {
	t.Parallel()

	got := scanPlanted(t, map[string]string{
		"scripts/x.sh": "x MY_DITTOFS_X=1\n" +
			"DITTOFS_CONTROLPLANE_SECRET=a DITTOFS_ADMIN_INITIAL_PASSWORD=b\n",
	}, driftLists{})

	wantFindings(t, got, []string{
		"scripts/x.sh:2: DITTOFS_CONTROLPLANE_SECRET: no longer read; remove it",
		"scripts/x.sh:2: DITTOFS_ADMIN_INITIAL_PASSWORD: no longer read; " +
			"pass dfs init --admin-password-env <name> or --admin-password-file",
	})
}

func TestScanReadsALongLineAndALastLineWithoutANewline(t *testing.T) {
	t.Parallel()

	got := scanPlanted(t, map[string]string{
		"docs/guide/long.md": strings.Repeat("x", 100<<10) + "\nDITTOFS_CONTROLPLANE_SECRET\n",
		"docs/guide/tail.md": "first\nDITTOFS_CONTROLPLANE_SECRET",
	}, driftLists{})

	wantFindings(t, got, []string{
		"docs/guide/long.md:2: DITTOFS_CONTROLPLANE_SECRET: no longer read; remove it",
		"docs/guide/tail.md:2: DITTOFS_CONTROLPLANE_SECRET: no longer read; remove it",
	})
}

func TestScanSkipsWhatIsNotARegularFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := scanDrift(root, []string{"dir"}, driftLists{})
	if err != nil || len(result.findings) != 0 {
		t.Errorf("directory: got %q, %v; want no finding and no error", result.findings, err)
	}

	_, err = scanDrift(root, []string{"gone.sh"}, driftLists{})
	if err == nil || !strings.Contains(err.Error(), "gone.sh") {
		t.Errorf("missing file: got error %v, want one naming gone.sh", err)
	}
}

func TestScanFlagsAProvisionalNameWithoutAReader(t *testing.T) {
	t.Parallel()

	const name = "DITTOFS_OTLP_ENDPOINT"

	lists := driftLists{
		provisional: []string{name},
	}
	noReader := []string{
		name + ": provisional name with no reader left; remove it from the allowed names",
	}
	prose := map[string]string{
		"docs/guide/x.md": "Set " + name + " to trace.\n",
		"cmd/y/y.go":      "package y\n\n// " + name + " is read elsewhere.\n",
	}

	cases := []struct {
		label string
		files map[string]string
		want  []string
	}{
		{"prose and a comment only", prose, noReader},
		{
			"a literal in a test only",
			map[string]string{
				"cmd/x/x_test.go": "package x\n\nconst e = \"" + name + "\"\n",
			},
			noReader,
		},
		{
			"a literal in pkg/bootstrap only",
			map[string]string{
				"pkg/bootstrap/x.go": "package bootstrap\n\nconst e = \"" + name + "\"\n",
			},
			noReader,
		},
		{
			"a literal in exempt paths only",
			map[string]string{
				"k8s/dittofs-operator/x.go": "package x\n\nconst e = \"" + name + "\"\n",
				"docs/internals/x.go":       "package x\n\nconst e = \"" + name + "\"\n",
			},
			noReader,
		},
		{
			"a reader",
			map[string]string{
				"docs/guide/x.md": prose["docs/guide/x.md"],
				"cmd/y/y.go":      prose["cmd/y/y.go"],
				"cmd/x/x.go":      "package x\n\nconst e = \"" + name + "\"\n",
			},
			nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			wantFindings(t, scanPlanted(t, tc.files, lists), tc.want)
		})
	}
}

func TestScanFlagsAnAllowedNameNobodyExports(t *testing.T) {
	t.Parallel()

	const name = "DITTOFS_REPO"

	lists := driftLists{
		exported: []string{name},
	}
	noExporter := []string{
		name + ": allowed name no tracked file exports; remove it from the allowed names",
	}

	cases := []struct {
		label string
		files map[string]string
		want  []string
	}{
		{"named nowhere", map[string]string{"scripts/y.sh": "true\n"}, noExporter},
		{
			"in a test only",
			map[string]string{"cmd/x/x_test.go": "package x\n\nconst e = \"" + name + "\"\n"},
			noExporter,
		},
		{
			"in pkg/bootstrap only",
			map[string]string{"pkg/bootstrap/x.go": "package bootstrap\n\n// " + name + "\n"},
			noExporter,
		},
		{
			"in exempt paths only",
			map[string]string{
				".planning/x.md":      name + "\n",
				"docs/internals/x.md": name + "\n",
			},
			noExporter,
		},
		{"an exporter", map[string]string{"scripts/x.sh": "export " + name + "=.\n"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			wantFindings(t, scanPlanted(t, tc.files, lists), tc.want)
		})
	}
}

func TestScanPassesATestOnlyName(t *testing.T) {
	t.Parallel()

	const name = "DITTOFS_ONLYTESTS_X"

	got := scanPlanted(t, map[string]string{
		"scripts/x.sh":    name + "=1 go test ./cmd/x\n",
		"cmd/x/x_test.go": "package x\n\nconst e = \"" + name + "\"\n",
		"scripts/y.sh":    name + "_Y=1\n",
	}, driftLists{
		testOnly: []string{name},
	})

	wantFindings(t, got, []string{
		"scripts/y.sh:1: DITTOFS_ONLYTESTS_X_Y: unknown name; did you mean " +
			closestEnvName("DITTOFS_ONLYTESTS_X_Y") + "? If only tests and scripts read it, " +
			"add it to testOnlyEnvNames in pkg/bootstrap/drift_test.go",
	})

	for _, listed := range testOnlyEnvNames {
		if class, _ := classifyEnv(listed, "", declaredEnv); class != envUnknown {
			t.Errorf("classifyEnv(%s): got class %d, want unknown (%d): the host must "+
				"keep refusing a test-only name", listed, class, envUnknown)
		}
	}
}

func TestScanFlagsATestOnlyNameNobodyUses(t *testing.T) {
	t.Parallel()

	const name = "DITTOFS_ONLYTESTS_X"

	lists := driftLists{
		testOnly: []string{name},
	}
	noUser := []string{
		name + ": test-only name with no user left; remove it from the test-only list",
	}
	script := name + "=1 go test ./cmd/x\n"
	reader := "package x\n\nconst e = \"" + name + "\"\n"

	cases := []struct {
		label string
		files map[string]string
		want  []string
	}{
		{
			"a script and a test reader",
			map[string]string{"scripts/x.sh": script, "cmd/x/x_test.go": reader},
			nil,
		},
		{
			"named only in pkg/bootstrap",
			map[string]string{"pkg/bootstrap/x.md": script, "cmd/x/x_test.go": reader},
			noUser,
		},
		{
			"read only in pkg/bootstrap",
			map[string]string{"scripts/x.sh": script, "pkg/bootstrap/x_test.go": reader},
			noUser,
		},
		{
			"named only in an exempt path",
			map[string]string{".planning/x.md": script, "cmd/x/x_test.go": reader},
			noUser,
		},
		{
			"read only in an exempt path",
			map[string]string{"scripts/x.sh": script, "k8s/dittofs-operator/x_test.go": reader},
			noUser,
		},
		{
			"named by no scanned file",
			map[string]string{"cmd/x/x_test.go": reader},
			noUser,
		},
		{
			"read only in a comment",
			map[string]string{
				"scripts/x.sh":    script,
				"cmd/x/x_test.go": "package x\n\n// " + name + " is read elsewhere.\n",
			},
			noUser,
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			wantFindings(t, scanPlanted(t, tc.files, lists), tc.want)
		})
	}
}

// driftFloor is how many names files that keep names hold, so a scan that
// reads nothing cannot pass.
var driftFloor = map[string]int{
	".githooks/pre-commit":      16,
	".githooks/commit-msg":      4,
	".githooks/pre-push":        5,
	"cmd/dfs/commands/start.go": 1,
}

func TestRepositoryNamesNoRefusedVariable(t *testing.T) {
	t.Parallel()

	root := driftRepositoryRoot(t)

	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	files := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")

	result, err := scanDrift(root, files, driftLists{
		provisional: provisionalEnvNames,
		exported:    launcherEnvNames(),
		testOnly:    testOnlyEnvNames,
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	for _, finding := range result.findings {
		t.Error(finding)
	}

	for file, want := range driftFloor {
		if got := result.names[file]; got < want {
			t.Errorf("%s: got %d names, want at least %d", file, got, want)
		}
	}
}

// driftRepositoryRoot is the directory holding go.mod, above the test's.
func driftRepositoryRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}

		dir = parent
	}
}
