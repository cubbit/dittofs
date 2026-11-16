package main

import (
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/test/harness/plan"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// The completion functions below give cobra's scripts, which dt completion
// bash and zsh print, the words that fit an argument or a flag's value, from
// the registry or the results; the shell keeps those that begin with what is
// typed. A registry that doesn't load completes nothing, and none of them
// completes file names.

// fixed completes with words.
func fixed(words ...string) cobra.CompletionFunc {
	return cobra.FixedCompletions(words, cobra.ShellCompDirectiveNoFileComp)
}

// fromRegistry completes with what words gives from the registry.
func fromRegistry(words func(root string, r *registry.Registry, cmd *cobra.Command, args []string, toComplete string) []string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		root, r, err := load()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return words(root, r, cmd, args, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeRun completes dt run's target, then the options of the suite it
// names, as flags. A target is a group, a suite, or a suite's cells by the
// first segments of their names, at any tier and with every matrix value; an
// option's cells are run by its flag, so they aren't one.
var completeRun = fromRegistry(func(root string, r *registry.Registry, cmd *cobra.Command, args []string, _ string) []string {
	if cmd.Flags().Changed("place") {
		return nil
	}
	if len(args) > 0 {
		name, _, _ := strings.Cut(args[0], "/") // a cell or a suite's first segments name its suite
		var flags []string
		if s := r.Suite(name); s != nil {
			for _, o := range s.Options {
				flags = append(flags, "--"+o.Name)
			}
		}
		return flags
	}
	targets := slices.Collect(maps.Keys(r.Groups))
	for _, s := range r.Suites {
		targets = append(targets, s.Name)
		for _, c := range cellNames(root, r.Units(s)[:1]) {
			for i := range c {
				if c[i] == '/' {
					targets = append(targets, c[:i])
				}
			}
			targets = append(targets, c)
		}
	}
	slices.Sort(targets)
	return slices.Compact(targets)
})

// completeGroupSuites completes --only and --skip: the suites of the group
// dt run names, each after the ones already listed.
var completeGroupSuites = fromRegistry(func(_ string, r *registry.Registry, _ *cobra.Command, args []string, toComplete string) []string {
	if len(args) == 0 {
		return nil
	}
	listed := toComplete[:strings.LastIndex(toComplete, ",")+1]
	done := strings.Split(strings.TrimSuffix(listed, ","), ",")
	var suites []string
	for _, s := range r.Groups[args[0]] {
		if slices.Contains(done, s) {
			continue
		}
		suites = append(suites, listed+s)
	}
	return suites
})

// completeSuite completes a command's one suite: dt history's.
var completeSuite = fromRegistry(func(_ string, r *registry.Registry, _ *cobra.Command, args []string, _ string) []string {
	if len(args) > 0 {
		return nil
	}
	var suites []string
	for _, s := range r.Suites {
		suites = append(suites, s.Name)
	}
	return suites
})

// completeKnownSuite completes dt known prune's suite: one with a known file,
// of its own or of an option.
var completeKnownSuite = fromRegistry(func(_ string, r *registry.Registry, _ *cobra.Command, args []string, _ string) []string {
	if len(args) > 0 {
		return nil
	}
	var suites []string
	for _, s := range r.Suites {
		if slices.ContainsFunc(r.Units(s), func(u *registry.Suite) bool { return u.Known != "" }) {
			suites = append(suites, s.Name)
		}
	}
	return suites
})

// completeBaseline completes dt known baseline's cell, one of smbtorture's
// and its options', then the run.
var completeBaseline = fromRegistry(func(root string, r *registry.Registry, cmd *cobra.Command, args []string, _ string) []string {
	switch s := r.Suite("smbtorture"); {
	case len(args) == 1:
		return runNames(cmd)
	case len(args) == 0 && s != nil:
		return cellNames(root, r.Units(s))
	}
	return nil
})

// completeRuns completes dt report's runs.
func completeRuns(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return runNames(cmd), cobra.ShellCompDirectiveNoFileComp
}

// cellNames are the names of the cells units have at any tier, with every
// matrix value.
func cellNames(root string, units []*registry.Suite) []string {
	var names []string
	for _, u := range units {
		cells, _ := plan.UnitCells(root, u, "manual", true)
		for _, c := range cells {
			names = append(names, c.Name)
		}
	}
	return names
}

// runNames are the runs in the results directory: the command's --results,
// or the default when it has none.
func runNames(cmd *cobra.Command) []string {
	root, err := repoRoot()
	if err != nil {
		return nil
	}
	results, _ := cmd.Flags().GetString("results")
	entries, _ := os.ReadDir(resultsDir(root, results))
	var runs []string
	for _, e := range entries {
		if e.IsDir() {
			runs = append(runs, e.Name())
		}
	}
	return runs
}
