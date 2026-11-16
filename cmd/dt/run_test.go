package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/cubbit/dittofs/test/harness/registry"
	"github.com/cubbit/dittofs/test/harness/report"
	"github.com/cubbit/dittofs/test/harness/run"
)

// The registry refuses an option named like any of dt run's own flags: the
// option is a flag of its own, and with a flag of that name already there it
// would get none.
func TestOptionsCannotTakeRunFlagNames(t *testing.T) {
	const head = "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\n"
	newRun().Flags().VisitAll(func(f *pflag.Flag) {
		if strings.HasPrefix(f.Usage, "run the suite's ") {
			return // a suite option's own flag
		}
		yaml := head + "suites:\n  a: { cmd: true, options: { " + f.Name + ": { cmd: false } } }\n"
		if _, err := registry.Parse(t.TempDir(), []byte(yaml)); err == nil || !strings.Contains(err.Error(), "dt run has a flag of that name") {
			t.Errorf("an option named %s: %v", f.Name, err)
		}
	})
}

// resultsCheckout moves the test into a checkout whose group g has a cell of
// each result: two pass, one fails, one is skipped for a need this machine
// doesn't meet, and one doesn't run, since dt can't set up its fixture yet.
func resultsCheckout(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the suites run true and false")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "test", "suites.yaml"), "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }\ngroups: { g: [ok, ok2, bad, away, fixed] }\nsuites:\n"+
		"  ok: { cmd: \"true\" }\n"+
		"  ok2: { cmd: \"true\" }\n"+
		"  bad: { cmd: \"false\" }\n"+
		"  away: { cmd: \"true\", needs: [windows] }\n"+
		"  fixed: { cmd: \"true\", fixture: { profile: memory, nfs: \"4.1\", smb: { mount: true } } }\n")
	t.Chdir(root)
}

// dt run ends with where the results went, then a line counting the cells by
// result, and all of them, with the run's time.
func TestRunSummaryLine(t *testing.T) {
	resultsCheckout(t)
	out, _ := dt(t, "run", "g", "--results", t.TempDir())
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if n := len(lines); n < 2 || !strings.HasPrefix(lines[n-2], "results: ") ||
		!regexp.MustCompile(`^cells: 1 failed, 1 not run, 2 passed, 1 skipped, 5 total in (\d+m)?\d+s$`).MatchString(lines[n-1]) {
		t.Errorf("dt run g ends:\n%s", out)
	}
}

// dt run whose output isn't a terminal's colors nothing, whatever NO_COLOR
// and --no-color say.
func TestRunPlainOffATerminal(t *testing.T) {
	resultsCheckout(t)
	t.Setenv("NO_COLOR", "")
	if out, _ := dt(t, "run", "g", "--results", t.TempDir()); strings.Contains(out, "\x1b") {
		t.Errorf("off a terminal, colors:\n%q", out)
	}
}

// The results are colored on a terminal, unless --no-color is given or
// NO_COLOR is set to anything but empty.
func TestColorOn(t *testing.T) {
	for _, c := range []struct {
		noColor    bool
		noColorEnv string
		onTerminal bool
		want       bool
	}{
		{false, "", true, true},
		{true, "", true, false},
		{false, "1", true, false},
		{false, "", false, false},
	} {
		if got := colorOn(c.noColor, c.noColorEnv, c.onTerminal); got != c.want {
			t.Errorf("colorOn(%v, %q, %v) = %v, want %v", c.noColor, c.noColorEnv, c.onTerminal, got, c.want)
		}
	}
}

// How a run went, plain as dt run printed it before colors, then a line
// counting the cells by result, and all of them, with the run's time; and
// colored: passed green, failed red, not run yellow, the rest in the
// terminal's default color, whose escapes are as long, so the columns line
// up as in the plain table.
func TestPrintRun(t *testing.T) {
	rep := run.Report{Dir: "/results/run", Outcomes: []report.Cell{
		{Name: "ok", Result: report.Passed, Took: time.Second},
		{Name: "ok2", Result: report.Passed},
		{Name: "bad", Result: report.Failed, Took: 72 * time.Second, Note: "`false` exited 1"},
		{Name: "away", Result: report.Skipped, Note: "windows: this machine runs darwin"},
		{Name: "fixed", Result: report.NotRun, Note: "no fixture yet"},
	}}
	const plain = "" +
		"CELL   RESULT   TIME   NOTE\n" +
		"ok     passed   1s     \n" +
		"ok2    passed   0s     \n" +
		"bad    failed   1m12s  `false` exited 1\n" +
		"away   skipped  0s     windows: this machine runs darwin\n" +
		"fixed  not run  0s     no fixture yet\n" +
		"results: /results/run\n" +
		"cells: 1 failed, 1 not run, 2 passed, 1 skipped, 5 total in 1m15s\n"
	const colored = "" +
		"CELL   \x1b[39mRESULT\x1b[0m   TIME   NOTE\n" +
		"ok     \x1b[32mpassed\x1b[0m   1s     \n" +
		"ok2    \x1b[32mpassed\x1b[0m   0s     \n" +
		"bad    \x1b[31mfailed\x1b[0m   1m12s  `false` exited 1\n" +
		"away   \x1b[39mskipped\x1b[0m  0s     windows: this machine runs darwin\n" +
		"fixed  \x1b[33mnot run\x1b[0m  0s     no fixture yet\n" +
		"results: /results/run\n" +
		"cells: \x1b[31m1 failed\x1b[0m, \x1b[33m1 not run\x1b[0m, \x1b[32m2 passed\x1b[0m, \x1b[39m1 skipped\x1b[0m, 5 total in 1m15s\n"
	for color, want := range map[painter]string{false: plain, true: colored} {
		var out bytes.Buffer
		if err := printRun(&out, rep, 75*time.Second+400*time.Millisecond, color); err != nil || out.String() != want {
			t.Errorf("colors %v: %v\n%q\nwant:\n%q", color, err, out.String(), want)
		}
	}
}

// dtStreams is dt with its stdout and its stderr apart.
func dtStreams(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := newRoot(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// dt run --json prints on stdout what dt report --format json prints of the
// run, and the rest on stderr; its exit status is the run's.
func TestRunJSON(t *testing.T) {
	resultsCheckout(t)
	results := t.TempDir()
	stdout, stderr, err := dtStreams(t, "run", "g", "--json", "--results", results)
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("dt run g --json: %v, want exit status 1", err)
	}
	want, err := dt(t, "report", "--format", "json", "--results", results)
	if err != nil || stdout != want {
		t.Errorf("dt run g --json printed:\n%s\ndt report --format json: %v\n%s", stdout, err, want)
	}
	if !strings.Contains(stderr, "[dt] bad: failed") || !strings.Contains(stderr, "\nbad    failed") || !strings.Contains(stderr, "\ncells: 1 failed") {
		t.Errorf("stderr:\n%s", stderr)
	}
}
