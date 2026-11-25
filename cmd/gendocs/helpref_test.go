package main

// helpref_test.go checks the command references that appear inside the CLI's own
// prose against the command trees they describe.
//
// cli.md is generated, so it cannot drift. Everything else can: a Long
// description, an Example block, a log line or an error message that tells the
// user to run something is a plain string, and nothing stops it from naming a
// subcommand that moved or a flag that was renamed. A user following one of
// those gets "unknown command" and no way to tell whether the feature exists.
//
// The check is deliberately narrow so it never blocks a legitimate string. A
// token is only required to be a subcommand when the command it would attach to
// has subcommands of its own — those group commands take no positional
// arguments, so anything in that slot is either a child or a mistake. At a leaf
// the walk stops and only flags are still checked.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	dfs "github.com/cubbit/dittofs/cmd/dfs/commands"
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	dfsctl "github.com/cubbit/dittofs/cmd/dfsctl/commands"
)

// dfsRoot and dfsctlRoot are one tree of each binary, built once for every
// check to walk.
var (
	dfsRoot    = dfs.NewRoot()
	dfsctlRoot = dfsctl.NewRoot(cmdutil.Env{})
)

// roots maps a binary name to its command tree.
func roots() map[string]*cobra.Command {
	return map[string]*cobra.Command{
		"dfs":    dfsRoot,
		"dfsctl": dfsctlRoot,
	}
}

// placeholderish reports whether tok is standing in for a value the user
// supplies rather than naming a command: an angle/square/brace placeholder, a
// path, a quoted string, an env-style capital, a shell operator. Reaching one
// ends the command-path walk.
func placeholderish(tok string) bool {
	if tok == "" {
		return true
	}
	if strings.ContainsAny(tok, `<>[]{}()/\|&;$"'*=:,%@!?~`) {
		return true
	}
	// Anything outside ASCII is prose or a typographic placeholder ("…"), never
	// a command name.
	for _, r := range tok {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return strings.ToLower(tok) != tok
}

// invocation is one `dfs …` / `dfsctl …` reference found in a string.
type invocation struct {
	bin   string
	toks  []string
	where string // human-readable origin, for the failure message
}

// findInvocations pulls every binary reference out of s that is written as a
// command rather than mentioned in a sentence. "dfsctl is the REST client" and
// "run `dfsctl login`" both contain the binary name; only the second is claiming
// a command exists, and only the second is worth checking. A reference counts
// when it is quoted or backticked, or when it starts its own line — which is how
// Example blocks and copy-pasteable log messages are written.
func findInvocations(s, where string) []invocation {
	// A shell line ending in a backslash continues on the next, so its flags
	// belong to the command the first line starts.
	s = strings.ReplaceAll(s, "\\\n", " ")
	var out []invocation
	for bin := range roots() {
		for i := 0; i+len(bin)+1 < len(s); i++ {
			if !strings.HasPrefix(s[i:], bin+" ") || !isCommandStart(s, i) {
				continue
			}
			// "dfsctl is the REST client" opens the root's own description at
			// offset 0, which looks exactly like a command line. No command is
			// named after an English function word, so one in the first slot means
			// this is a sentence.
			if prosePrefix(s[i+len(bin)+1:]) {
				continue
			}
			rest := s[i+len(bin)+1:]
			if cut := strings.IndexAny(rest, "\n|&;`'\""); cut >= 0 {
				rest = rest[:cut]
			}
			var toks []string
			for _, tok := range strings.Fields(rest) {
				toks = append(toks, strings.TrimRight(tok, ".,:;)"))
			}
			if len(toks) > 0 {
				out = append(out, invocation{bin: bin, toks: toks, where: where})
			}
		}
	}
	return out
}

// proseWords are words that can follow the binary name in a sentence but can
// never be a subcommand.
var proseWords = map[string]bool{
	"is": true, "are": true, "the": true, "a": true, "an": true, "to": true,
	"and": true, "or": true, "will": true, "can": true, "does": true, "has": true,
	"was": true, "from": true, "with": true, "for": true, "in": true, "on": true,
	"talks": true, "uses": true, "needs": true, "requires": true, "commands": true,
	"subcommand": true, "build": true, "instance": true, "itself": true,
	"still": true, "did": true, "survived": true, "exited": true, "process": true,
}

// prosePrefix reports whether the text following a binary name reads as a
// sentence rather than a command line.
func prosePrefix(rest string) bool {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return true
	}
	return proseWords[strings.ToLower(strings.TrimRight(fields[0], ".,:;)"))]
}

// isCommandStart reports whether the binary name at s[i:] is being written as a
// command line: immediately after a quote or backtick, or at the start of a line
// with nothing before it but indentation, a shell prompt, or a relative path.
func isCommandStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	switch s[i-1] {
	case '`', '\'', '"':
		return true
	}
	lineStart := strings.LastIndexByte(s[:i], '\n') + 1
	prefix := strings.TrimLeft(s[lineStart:i], " \t")
	prefix = strings.TrimPrefix(prefix, "$ ")
	prefix = strings.TrimLeft(prefix, " \t")
	return prefix == "" || prefix == "./" || prefix == "sudo " || prefix == "sudo ./"
}

// checkInvocation walks inv against its command tree and returns one message per
// problem found.
func checkInvocation(inv invocation) []string {
	cmd := roots()[inv.bin]
	path := []string{inv.bin}
	var problems []string
	var flags []string

	skipValue := false
	for _, tok := range inv.toks {
		if skipValue {
			// The previous flag takes a value and it was written as its own
			// token: `-o json`, `--server https://…`. Consuming it here keeps
			// "json" from being read as a subcommand.
			skipValue = false
			continue
		}
		switch {
		case strings.HasPrefix(tok, "-"):
			flags = append(flags, tok)
			skipValue = takesSeparateValue(cmd, tok)
			continue
		case placeholderish(tok):
			// An argument, a placeholder or a value — the command path ends here.
			return append(problems, checkFlags(cmd, flags, path, inv.where)...)
		case !cmd.HasSubCommands():
			// A leaf's positional argument.
			return append(problems, checkFlags(cmd, flags, path, inv.where)...)
		}
		child := findChild(cmd, tok)
		if child == nil {
			problems = append(problems, inv.where+": `"+strings.Join(path, " ")+" "+tok+
				"` — "+strings.Join(path, " ")+" has no subcommand \""+tok+"\" (has: "+childNames(cmd)+")")
			return problems
		}
		cmd = child
		path = append(path, tok)
	}
	return append(problems, checkFlags(cmd, flags, path, inv.where)...)
}

// takesSeparateValue reports whether raw is a flag whose value follows as its
// own token. A `--flag=value` form carries its value already, and a boolean
// takes none; anything else consumes the token after it. A flag we cannot
// resolve is assumed to take a value — it is already reported as unknown, and
// guessing the other way would pile a bogus "no subcommand" on top of it.
func takesSeparateValue(cmd *cobra.Command, raw string) bool {
	name := strings.TrimLeft(raw, "-")
	if strings.Contains(name, "=") || name == "" {
		return false
	}
	// Short flags can be bundled ("-abc"); those are boolean by definition.
	if !strings.HasPrefix(raw, "--") && len(name) > 1 {
		return false
	}
	f := lookupFlag(cmd, raw, name)
	if f == nil {
		return true
	}
	// pflag gives a bool flag a NoOptDefVal so it can stand alone.
	return f.Value.Type() != "bool" && f.NoOptDefVal == ""
}

// lookupFlag resolves a flag against the command, the flags it inherits, and the
// root's persistent flags — a flag may be written before the subcommand it
// belongs to, when the walk has not descended yet.
func lookupFlag(cmd *cobra.Command, raw, name string) *pflag.Flag {
	sets := []*pflag.FlagSet{cmd.Flags(), cmd.InheritedFlags(), cmd.PersistentFlags(), cmd.Root().PersistentFlags()}
	for _, set := range sets {
		if strings.HasPrefix(raw, "--") {
			if f := set.Lookup(name); f != nil {
				return f
			}
			continue
		}
		if len(name) == 1 {
			if f := set.ShorthandLookup(name); f != nil {
				return f
			}
		}
	}
	return nil
}

// checkFlags validates the long and short flags named in an invocation against
// the command it resolved to, including flags inherited from its parents.
func checkFlags(cmd *cobra.Command, flags, path []string, where string) []string {
	var problems []string
	for _, raw := range flags {
		name := strings.TrimLeft(raw, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if name == "" || placeholderish(name) {
			continue
		}
		if !strings.HasPrefix(raw, "--") && len(name) != 1 {
			// A bundle of short flags; not worth unpicking.
			continue
		}
		if lookupFlag(cmd, raw, name) == nil {
			problems = append(problems, where+": `"+strings.Join(path, " ")+
				"` has no flag "+strings.SplitN(raw, "=", 2)[0])
		}
	}
	return problems
}

func findChild(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name {
			return c
		}
		for _, alias := range c.Aliases {
			if alias == name {
				return c
			}
		}
	}
	return nil
}

func childNames(cmd *cobra.Command) string {
	var names []string
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// TestHelpTextCommandReferences checks every Short, Long and Example string in
// both command trees. This is the text users read at `--help`, which cli.md is
// generated from but which no generator validates.
func TestHelpTextCommandReferences(t *testing.T) {
	var problems []string
	for bin, root := range roots() {
		walkCommands(root, bin, func(cmd *cobra.Command, path string) {
			for field, text := range map[string]string{
				"Short":   cmd.Short,
				"Long":    cmd.Long,
				"Example": cmd.Example,
			} {
				for _, inv := range findInvocations(text, path+" ("+field+")") {
					problems = append(problems, checkInvocation(inv)...)
				}
			}
		})
	}
	reportProblems(t, problems)
}

// sourceRoot is the repository root relative to this package's directory, which
// is where the test binary runs. filepath.Clean gives it the host separator so
// the walk root compares and trims equal to the paths WalkDir reports.
var sourceRoot = filepath.Clean("../..")

// TestSourceStringCommandReferences checks the command references in log lines,
// error messages and any other string literal in the tree. A message that tells
// an operator to run a command that does not exist is the same drift as a stale
// Long description, and it reaches them at the worst possible moment.
func TestSourceStringCommandReferences(t *testing.T) {
	var problems []string
	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir(sourceRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable dir is not this test's business
		}
		if d.IsDir() {
			// The walk root is named ".." — skipping it would skip everything and
			// leave the check passing vacuously.
			if path == sourceRoot {
				return nil
			}
			// Skip dot-directories wholesale, not just .git. A checked-out tree
			// can hold nested git worktrees under .claude/, each a full copy of
			// the repo at some other commit — scanning those reports drift
			// against branches that are not this one, in files that may not even
			// exist here any more. Named skips would miss the next such
			// directory; the rule is that the source under test is the visible
			// tree.
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			switch d.Name() {
			case "vendor", "node_modules", "testdata", "docs":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil //nolint:nilerr // a file that does not parse fails the build, not this test
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			where := filepath.ToSlash(strings.TrimPrefix(path, sourceRoot+string(filepath.Separator))) + ":" +
				strconv.Itoa(fset.Position(lit.Pos()).Line)
			for _, inv := range findInvocations(text, where) {
				problems = append(problems, checkInvocation(inv)...)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk source tree: %v", err)
	}
	// A directory filter that accidentally matches the walk root skips the whole
	// tree, and a check that scans nothing passes. Assert it actually looked.
	if scanned < 500 {
		t.Fatalf("scanned only %d source files from %s — the walk is being skipped, "+
			"so this check is passing without looking at anything", scanned, sourceRoot)
	}
	reportProblems(t, problems)
}

// TestDocsCommandReferences checks the hand-written guides. cli.md is generated
// and skipped; everything around it is prose an author has to keep in step by
// hand, which is how a whole page came to document a flag layout the CLI had
// stopped using. RFCs and plans are records of decisions, which name commands
// a change removes or has yet to build, so they are not held to today's tree.
func TestDocsCommandReferences(t *testing.T) {
	var problems []string
	err := filepath.WalkDir("../../docs", func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && (d.Name() == "rfcs" || d.Name() == "planning") {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil //nolint:nilerr // an unreadable doc is not this test's business
		}
		if filepath.Base(path) == "cli.md" {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil //nolint:nilerr // ditto
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, "../../"))
		lines := strings.Split(string(body), "\n")
		for i := 0; i < len(lines); i++ {
			where := rel + ":" + strconv.Itoa(i+1)
			// A line ending in a backslash is checked with the lines it continues on.
			text := lines[i]
			for strings.HasSuffix(text, "\\") && i+1 < len(lines) {
				i++
				text += "\n" + lines[i]
			}
			for _, inv := range findInvocations(text, where) {
				problems = append(problems, checkInvocation(inv)...)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	reportProblems(t, problems)
}

// TestCheckerAcceptsAndRejects pins the checker's own behaviour. A checker that
// cries wolf gets deleted by the next person it blocks, so the accept cases
// matter as much as the reject ones — particularly a flag whose value is written
// as a separate token, where the value must not be read as a subcommand.
func TestCheckerAcceptsAndRejects(t *testing.T) {
	accept := []string{
		"`dfsctl quota set /archive --scope user --id 1000`",
		"`dfsctl -o json share list`",
		"`dfsctl --output json share list`",
		"`dfs start`",
		"`dfsctl share get <share>`",
		"dfsctl is the REST client for a running dfs instance",
		"run `dfsctl login` first",
	}
	for _, s := range accept {
		var problems []string
		for _, inv := range findInvocations(s, "case") {
			problems = append(problems, checkInvocation(inv)...)
		}
		if len(problems) > 0 {
			t.Errorf("false positive on %q:\n  %s", s, strings.Join(problems, "\n  "))
		}
	}

	reject := []string{
		"`dfsctl user edit alice`",                     // it is `user update`
		"`dfs config init`",                            // it is `dfs init`
		"`dfsctl quota list --kind remote`",            // no such flag
		"`dfs init --admin`",                           // no such flag
		"dfs start --config c.yaml \\\n  --pid-file p", // a flag on a continued line
	}
	for _, s := range reject {
		var problems []string
		for _, inv := range findInvocations(s, "case") {
			problems = append(problems, checkInvocation(inv)...)
		}
		if len(problems) == 0 {
			t.Errorf("false negative: %q should have been reported", s)
		}
	}
}

func reportProblems(t *testing.T, problems []string) {
	t.Helper()
	if len(problems) == 0 {
		return
	}
	sort.Strings(problems)
	t.Errorf("%d stale command reference(s) — the command moved or the flag was renamed:\n  %s",
		len(problems), strings.Join(problems, "\n  "))
}

// walkCommands visits cmd and every descendant, passing the full command path.
func walkCommands(cmd *cobra.Command, path string, fn func(*cobra.Command, string)) {
	fn(cmd, path)
	for _, c := range cmd.Commands() {
		walkCommands(c, path+" "+c.Name(), fn)
	}
}

// deletedPaths are the paths of v1's control plane, its API client, its
// identity providers and its test drivers that the v2 node replaced. A page
// naming one describes code that no longer exists, so a reader following it
// finds nothing. v1's removed dfs commands are not paths:
// TestDocsCommandReferences refuses a page that runs one.
var deletedPaths = []string{
	"pkg/controlplane",
	"internal/controlplane",
	"pkg/apiclient",
	"pkg/snapshot",
	"pkg/config",
	"pkg/identity/ldap",
	"pkg/identity/kerberos",
	"internal/sysinfo",
	"pkg/auth/sid/mapper.go",
	"pkg/health/share.go",
	"pkg/health/cache.go",
	"test/e2e/helpers",
	"test/e2e/snapshot",
	"test/e2e/main_test.go",
	"test/harness/fixture/api.go",
	"test/harness/fixture/shares.go",
	"test/integration/ad-dc/ad_dc_ldap_test.go",
	"test/integration/ad-dc/netlogon_smbclient_test.go",
	"docs/guide/metadata-store-migration.md",
}

// deletedPathsIn returns each deleted path line names, a deleted page also by
// its name alone, as a page beside it links it. A path
// counts only as a whole name, so pkg/snapshot matches neither
// pkg/snapshotter, pkg/snapshot.go nor mypkg/snapshot.
//
// decision: a path inside a URL is not reported, since a link to a tag or
// another branch names a file that exists there. A path written with
// backslashes is not reported either; the pages write paths with slashes.
func deletedPathsIn(line string) []string {
	var found []string
	for _, path := range deletedPaths {
		for _, p := range []string{path, filepath.Base(path)} {
			if p != path && !strings.HasSuffix(p, ".md") {
				continue // only a page is linked by its name alone; share.go names a live file too
			}
			if namedIn(line, p) {
				found = append(found, path)
				break
			}
		}
	}
	return found
}

// namedIn reports whether line names p as a whole name outside a URL.
func namedIn(line, p string) bool {
	for from := 0; ; {
		i := strings.Index(line[from:], p)
		if i < 0 {
			return false
		}
		i += from
		from = i + 1
		if i > 0 && (isNameChar(line[i-1]) || line[i-1] == '.') {
			continue
		}
		if !nameEnds(line[i+len(p):]) {
			continue
		}
		start := strings.LastIndexAny(line[:i], " \t(<[`\"'") + 1
		if !strings.Contains(line[start:i], "://") {
			return true
		}
	}
}

// nameEnds reports whether a path's last element ends where rest starts: at
// the line's end, a slash, a full stop ending the sentence, or any character
// a name does not hold.
func nameEnds(rest string) bool {
	switch {
	case rest == "" || rest[0] == '/':
		return true
	case rest[0] == '.':
		return len(rest) == 1 || !isNameChar(rest[1])
	}
	return !isNameChar(rest[0])
}

func isNameChar(c byte) bool {
	return c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// TestPagesNameNoDeletedPath checks README.md, CLAUDE.md and every page under
// docs/ but the RFCs and plans, which are design history, for a deleted path.
func TestPagesNameNoDeletedPath(t *testing.T) {
	for _, p := range deletedPaths {
		if _, err := os.Stat(filepath.Join(sourceRoot, p)); err == nil {
			t.Errorf("%s exists again: drop it from deletedPaths", p)
		}
	}

	pages := []string{filepath.Join(sourceRoot, "README.md"), filepath.Join(sourceRoot, "CLAUDE.md")}
	err := filepath.WalkDir(filepath.Join(sourceRoot, "docs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "rfcs" || d.Name() == "planning") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			pages = append(pages, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	// A walk that finds nothing passes; the guides alone are more than this.
	if len(pages) < 20 {
		t.Fatalf("found only %d pages under %s: the walk is not reaching the docs", len(pages), sourceRoot)
	}

	var problems []string
	for _, path := range pages {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, _ := filepath.Rel(sourceRoot, path)
		for i, line := range strings.Split(string(body), "\n") {
			for _, p := range deletedPathsIn(line) {
				problems = append(problems, fmt.Sprintf("%s:%d: names %s, which v2 deleted", filepath.ToSlash(rel), i+1, p))
			}
		}
	}
	if len(problems) > 0 {
		t.Errorf("%d page line(s) name a deleted path:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}

// TestDeletedPathsIn pins the match: a deleted path is found wherever its
// name ends, and a longer name that starts with it is not.
func TestDeletedPathsIn(t *testing.T) {
	for _, c := range []struct {
		line string
		want string
	}{
		{"The Runtime (`pkg/controlplane/runtime/`) routes", "pkg/controlplane"},
		{"see pkg/apiclient.", "pkg/apiclient"},
		{"[the guide](metadata-store-migration.md)", "docs/guide/metadata-store-migration.md"},
		{"see docs/guide/metadata-store-migration.md.", "docs/guide/metadata-store-migration.md"},
		{"`pkg/apiclient/`", "pkg/apiclient"},
		{"see pkg/node/apiv1/share.go", ""},
		{"mypkg/snapshot and old-metadata-store-migration.md", ""},
		{"https://github.com/cubbit/dittofs/blob/v0.35.0/pkg/controlplane/x.go", ""},
		{"`pkg/snapshot`", "pkg/snapshot"},
		{"pkg/snapshotter, pkg/snapshot.go and pkg/controlplanes", ""},
		{"internal/node builds the API", ""},
	} {
		got := strings.Join(deletedPathsIn(c.line), ",")
		if got != c.want {
			t.Errorf("deletedPathsIn(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}
