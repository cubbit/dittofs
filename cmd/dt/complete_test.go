package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// completions are the words dt completes the last of args with, sorted; the
// directive must keep the shell from completing file names.
func completions(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := dt(t, append([]string{"__complete"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	words := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if directive := words[len(words)-1]; directive != ":4" {
		t.Errorf("dt %s: directive %s, want :4, no file names", strings.Join(args, " "), directive)
	}
	return slices.Sorted(slices.Values(words[:len(words)-1]))
}

// dt completes run's target with the groups, the suites, and each suite's
// cells by the first segments of their names, but not an option's cells,
// which its flag runs; then a suite's options, as flags; --only and --skip
// with the group's suites, after those listed; --tier and --in with their
// values. report completes its runs, those of --results, and its formats;
// history a suite; known prune a suite with a known file, of its own or an
// option's; known baseline smbtorture's cells, its options' included, then a
// run.
func TestComplete(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "test", "suites.yaml"), "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }\ngroups: { g: [m, plain] }\nsuites:\n"+
		"  m: { cmd: \"true {x} {y}\", matrix: { x: [a, b], y: [\"1\"] }, options: { o: { cmd: \"true\", matrix: {}, known: o.yaml } } }\n"+
		"  plain: { cmd: \"true\" }\n"+
		"  smbtorture: { cmd: \"true {v}\", matrix: { v: [a] }, options: { k: { cmd: \"true\", matrix: {} } } }\n")
	writeFile(t, filepath.Join(root, "o.yaml"), "")
	for _, dir := range []string{"test/harness/results/run-1", "test/harness/results/run-2", "elsewhere/run-3"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "test", "harness", "results", "notes.txt"), "")
	t.Chdir(root)
	tiers := []string{"manual", "merge", "nightly", "pr"}
	for args, want := range map[string][]string{
		"run|":                         {"g", "m", "m/a", "m/a/1", "m/b", "m/b/1", "plain", "smbtorture", "smbtorture/a"},
		"run|m|":                       {"--o"},
		"run|plain|":                   nil,
		"run|g|--only|m,":              {"m,plain"},
		"run|g|--only|m,plain,":        nil,
		"run|smbtorture/a|":            {"--k"},
		"run|g|--skip|":                {"m", "plain"},
		"run|--tier|":                  tiers,
		"run|--in|":                    {"auto", "container", "host"},
		"list|--tier|":                 tiers,
		"plan|--tier|":                 tiers,
		"report|":                      {"run-1", "run-2"},
		"report|--results|elsewhere|":  {"run-3"},
		"report|--format|":             {"json", "md", "text"},
		"history|":                     {"diff\t" + newHistoryDiff().Short, "m", "plain", "smbtorture"}, // the subcommand, with its summary, then the suites
		"history|m|":                   nil,
		"known|prune|":                 {"m"},
		"known|baseline|":              {"smbtorture/a", "smbtorture/k"},
		"known|baseline|smbtorture/a|": {"run-1", "run-2"},
	} {
		if got := completions(t, strings.Split(args, "|")...); !slices.Equal(got, want) {
			t.Errorf("dt %s<TAB>: %q, want %q", strings.ReplaceAll(args, "|", " "), got, want)
		}
	}
}
