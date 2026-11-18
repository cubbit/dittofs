package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/test/harness/history"
	"github.com/cubbit/dittofs/test/harness/known"
	"github.com/cubbit/dittofs/test/harness/plan"
	"github.com/cubbit/dittofs/test/harness/registry"
	"github.com/cubbit/dittofs/test/harness/report"
)

// lastRuns is how many past runs dt history and dt known prune read unless
// --last says otherwise.
//
// decision: 10 runs, about a day of develop's. Raise it when flake rates or
// prune need a longer window by default.
const lastRuns = 10

// historyFlags are the flags of the commands that read past runs: --ci,
// --last and --branch.
func historyFlags(cmd *cobra.Command, ci *bool, last *int, branch *string) {
	cmd.Flags().BoolVar(ci, "ci", false, "read the last N runs of tests.yml on --branch, from their artifacts through the GitHub API, with the token in GITHUB_TOKEN, instead of this checkout's runs")
	cmd.Flags().IntVar(last, "last", lastRuns, "read the last N runs")
	cmd.Flags().StringVar(branch, "branch", "develop", "with --ci, the branch whose runs of tests.yml are read")
}

// pastRuns are the runs dt history reads, oldest first: this checkout's last
// ones, or with ci those of branch, each with the cells of its jobs' first attempt.
// With suite, only the artifacts of its cells are read, and warn says
// which couldn't be.
func pastRuns(ctx context.Context, suite string, ci bool, last int, branch string, warn io.Writer) ([]report.Run, error) {
	if last < 1 {
		return nil, fmt.Errorf("--last %d: expected 1 or more", last)
	}
	if ci {
		return history.CI(ctx, suite, last, branch, warn)
	}
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	return history.Local(resultsDir(root, ""), last)
}

func newHistory() *cobra.Command {
	var ci bool
	var last int
	var branch string
	cmd := &cobra.Command{
		Use:   "history [SUITE[/TEST]]",
		Short: "Pass, fail, skip and flaky counts over past runs, with p50 and p90 durations: per suite, per test of SUITE, and each result of TEST",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			suite, test, _ := strings.Cut(first(args), "/")
			runs, err := pastRuns(cmd.Context(), suite, ci, last, branch, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			counts, what := history.Suites(runs), "SUITE"
			if suite != "" {
				counts, what = history.Tests(runs, suite, test), "TEST"
			}
			if len(counts) == 0 {
				of := ""
				if len(args) > 0 {
					of = " of " + args[0]
				}
				_, err := fmt.Fprintf(out, "no results%s in the runs read\n", of)
				return err
			}
			rows := [][]string{{what, "RUNS", "PASS", "FAIL", "SKIP", "FLAKY", "P50", "P90"}}
			for _, key := range slices.Sorted(maps.Keys(counts)) {
				n := counts[key]
				rows = append(rows, []string{key, strconv.Itoa(n.Runs), strconv.Itoa(n.Pass), strconv.Itoa(n.Fail), strconv.Itoa(n.Skip), strconv.Itoa(n.Flaky), duration(n.P50()), duration(n.P90())})
			}
			if test != "" {
				// Each result, oldest first, says since when it fails.
				rows = append(rows, nil, []string{"RUN", "CELL", "TEST", "RESULT", "TIME"})
				for _, r := range runs {
					for _, c := range r.Cells {
						for _, t := range c.Tests {
							if history.Suite(c) == suite && t.Name == test {
								rows = append(rows, []string{r.Name, c.Name, t.Title(), result(t), duration(t.Duration)})
							}
						}
					}
				}
			}
			return table(out, rows)
		},
		ValidArgsFunction: completeSuite,
	}
	historyFlags(cmd, &ci, &last, &branch)
	cmd.AddCommand(newHistoryDiff())
	return cmd
}

// estimated is a planned cell with its p50 over the runs read, in minutes,
// null when it has no results there.
type estimated struct {
	plan.Cell
	P50Minutes *float64 `json:"p50_minutes"`
}

// estimate gives each cell its p50 over runs, by its name, so each shard
// counts as the runner it takes, and the tier's runner-minutes: the sum of
// those p50s, with how many cells have none.
//
// decision: a cell costs its p50. Runner-minutes are a cost, summed over
// many runs, so the typical run measures it; the p90 bounds one run, as the
// budgets do.
func estimate(planned []plan.Cell, runs []report.Run) (out []estimated, total float64, missing int) {
	counts := history.Cells(runs)
	var sum time.Duration
	for _, c := range planned {
		e := estimated{Cell: c}
		if n := counts[c.Name]; n != nil {
			e.P50Minutes, sum = new(minutes(n.P50())), sum+n.P50()
		} else {
			missing++
		}
		out = append(out, e)
	}
	return out, minutes(sum), missing
}

// minutes is d in minutes, to a tenth.
func minutes(d time.Duration) float64 { return math.Round(d.Minutes()*10) / 10 }

// result is a result's outcome, with its grade or that it was flaky.
func result(t report.Test) string {
	switch {
	case t.Flaky:
		return history.Outcome(t) + ", flaky"
	case t.Grade != "":
		return history.Outcome(t) + " (" + t.Grade + ")"
	}
	return history.Outcome(t)
}

func duration(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}

func newKnownPrune() *cobra.Command {
	var ci bool
	var last int
	var branch string
	cmd := &cobra.Command{
		Use:   "prune SUITE",
		Short: "Remove the known entries that matched no failing or skipping test in the runs dt history reads",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, r, err := load()
			if err != nil {
				return err
			}
			s := r.Suite(args[0])
			if s == nil {
				return fmt.Errorf("no suite %q", args[0])
			}
			runs, err := pastRuns(cmd.Context(), s.Name, ci, last, branch, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			// Each cell the suite and its options plan, by name, with its
			// unit and where it runs, but for the tier and OS of the run it
			// ran in. A cell no longer planned tells nothing.
			type planned struct {
				unit *registry.Suite
				at   map[string]string
			}
			cells := map[string]planned{}
			var files []string
			for _, u := range r.Units(s) {
				if u.Known != "" && !slices.Contains(files, u.Known) {
					files = append(files, u.Known)
				}
				list, err := plan.UnitCells(root, u, "manual", true)
				if err != nil {
					return err
				}
				for _, c := range list {
					at := map[string]string{"option": c.Option}
					maps.Copy(at, c.Matrix)
					cells[c.Name] = planned{u, at}
				}
			}
			if len(files) == 0 {
				return fmt.Errorf("%s has no known file", s.Name)
			}
			// Each known file's results: those of the cells it grades, each
			// where its cell ran, as dt run grades them: at the run's tier,
			// or the unit's own when wider, and on the run's OS, named as
			// the platform needs name it.
			results := map[string][]known.Result{}
			for _, run := range runs {
				for _, c := range run.Cells {
					p, ok := cells[c.Name]
					if !ok || p.unit.Known == "" {
						continue
					}
					at := maps.Clone(p.at)
					at["tier"], at["os"] = run.Tier, run.OS
					if !plan.RunsIn(p.unit.Tier, run.Tier) {
						at["tier"] = p.unit.Tier
					}
					if run.OS == "darwin" {
						at["os"] = "macos"
					}
					for _, t := range c.Tests {
						results[p.unit.Known] = append(results[p.unit.Known], known.Result{Name: t.Name, Outcome: history.Outcome(t), At: at})
					}
				}
			}
			out := cmd.OutOrStdout()
			for _, file := range files {
				if len(results[file]) == 0 {
					_, _ = fmt.Fprintf(out, "%s: left as is: no cell it grades has results in the runs read\n", file)
					continue
				}
				removed, err := known.Prune(filepath.Join(root, file), results[file])
				if err != nil {
					return err
				}
				if len(removed) == 0 {
					_, _ = fmt.Fprintf(out, "%s: nothing to remove\n", file)
				}
				for _, e := range removed {
					_, _ = fmt.Fprintf(out, "%s:%d: removed %s, a %s entry\n", file, e.Line, e.Test, e.Status)
				}
			}
			return nil
		},
		ValidArgsFunction: completeKnownSuite,
	}
	historyFlags(cmd, &ci, &last, &branch)
	return cmd
}

// diffFloor and diffTop are what dt history diff keeps unless --floor and
// --top say otherwise.
//
// decision: 100 ms, ten times the 10 ms a Go test's time is rounded to, so
// rounding and run-to-run noise don't fill the list; lower it when a suite of
// short tests is what's being searched.
const (
	diffFloor = 100 * time.Millisecond
	diffTop   = 30
)

func newHistoryDiff() *cobra.Command {
	var last, top int
	var branch string
	var floor time.Duration
	cmd := &cobra.Command{
		Use:   "diff [A B]",
		Short: "Which cells and tests took longer or shorter between two runs of tests.yml, by workflow run ID, or between the oldest and the newest of the last N on --branch",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 && len(args) != 2 {
				return fmt.Errorf("expected two workflow run IDs, A and B, or none with --last; got %d", len(args))
			}
			if len(args) == 2 && cmd.Flags().Changed("last") {
				return errors.New("two workflow run IDs and --last both say which runs to compare; give one")
			}
			for _, a := range args {
				if _, err := strconv.ParseInt(a, 10, 64); err != nil {
					return fmt.Errorf("%q is not a workflow run ID", a)
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var a, b []report.Run
			var err error
			if len(args) == 2 {
				ids := [2]int64{}
				for i, arg := range args {
					ids[i], _ = strconv.ParseInt(arg, 10, 64)
				}
				if a, err = history.Workflow(cmd.Context(), "", ids[0], cmd.ErrOrStderr()); err == nil {
					b, err = history.Workflow(cmd.Context(), "", ids[1], cmd.ErrOrStderr())
				}
			} else {
				var runs []report.Run
				if runs, err = history.CI(cmd.Context(), "", last, branch, cmd.ErrOrStderr()); err == nil {
					a, b, err = ends(runs)
				}
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "A: %s\nB: %s\n\n", runName(a), runName(b)); err != nil {
				return err
			}
			return writeDiff(out, history.Diff(a, b, floor, top))
		},
	}
	cmd.Flags().IntVar(&last, "last", lastRuns, "without run IDs, compare the oldest and the newest of the last N runs")
	cmd.Flags().StringVar(&branch, "branch", "develop", "without run IDs, the branch whose runs of tests.yml are compared")
	cmd.Flags().DurationVar(&floor, "floor", diffFloor, "leave out tests whose time changed by less than this")
	cmd.Flags().IntVar(&top, "top", diffTop, "show at most N tests, the ones that took longer first; 0 shows all")
	return cmd
}

// runName is the name of the workflow run runs belong to, its first job's.
func runName(runs []report.Run) string {
	if len(runs) == 0 {
		return "(no results)"
	}
	return runs[0].Name
}

// ends splits runs, oldest first, which are each workflow run's jobs one
// after another, into the first workflow run's and the last's, by name. Fewer
// than two workflow runs have nothing to compare, which is an error.
func ends(runs []report.Run) (first, last []report.Run, err error) {
	var names []string
	for _, r := range runs {
		if len(names) == 0 || names[len(names)-1] != r.Name {
			names = append(names, r.Name)
		}
	}
	if len(names) < 2 {
		return nil, nil, fmt.Errorf("found %d workflow runs, and comparing needs 2; raise --last or check --branch", len(names))
	}
	for _, r := range runs {
		switch r.Name {
		case names[0]:
			first = append(first, r)
		case names[len(names)-1]:
			last = append(last, r)
		}
	}
	return first, last, nil
}

// writeDiff writes the cells and then the tests of d: each with its time in A
// and in B, and how much longer it took, and which side it's missing from.
func writeDiff(out io.Writer, d history.Delta) error {
	rows := [][]string{{"CELL", "A", "B", "DELTA"}}
	for _, c := range d.Cells {
		rows = append(rows, diffRow(c, c.Cell))
	}
	rows = append(rows, nil, []string{"TEST", "CELL", "A", "B", "DELTA"})
	for _, c := range d.Tests {
		rows = append(rows, append([]string{c.Test}, diffRow(c, c.Cell)...))
	}
	return table(out, rows)
}

// diffRow is a change's cell, its times and its delta, marked new or gone
// when it's in one run only.
func diffRow(c history.Change, first string) []string {
	at := func(d time.Duration, in bool) string {
		if !in {
			return "-"
		}
		return duration(d)
	}
	delta := duration(c.Delta())
	if c.Delta() >= 0 {
		delta = "+" + delta
	}
	switch {
	case !c.InA:
		delta += " (new)"
	case !c.InB:
		delta += " (gone)"
	}
	return []string{first, at(c.A, c.InA), at(c.B, c.InB), delta}
}
