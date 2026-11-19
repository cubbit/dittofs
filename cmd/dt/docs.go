package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cubbit/dittofs/test/harness/registry"
)

// The testing guide, suitesDoc, relative to the repository root, holds two
// blocks dt writes: the suite table between the suites markers, its links
// relative to the guide, and dt's command reference between the commands
// markers.
const (
	suitesDoc     = "docs/internals/testing.md"
	suitesBegin   = "<!-- conformance-suites:begin -->"
	suitesEnd     = "<!-- conformance-suites:end -->"
	commandsBegin = "<!-- dt-commands:begin -->"
	commandsEnd   = "<!-- dt-commands:end -->"
)

func newDocs() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "docs [--check]",
		Short: "Write the testing guide's suite table from the registry and its command reference from dt's commands, or check them; check the RFCs' suites paragraphs; exit 1 on a difference",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, r, err := load()
			if err != nil {
				return err
			}
			path := filepath.Join(root, suitesDoc)
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			doc := string(data)
			// dt can't write an RFC's suites paragraph, so a problem with one
			// fails the write as well as the check.
			problems, err := rfcProblems(root, r)
			if err != nil {
				return err
			}
			updated := doc
			var stale []string
			for _, b := range []struct{ begin, end, block, what, from string }{
				{suitesBegin, suitesEnd, suitesTable(r), "the suite table", registry.Path},
				{commandsBegin, commandsEnd, commandsReference(cmd.Root()), "the command reference", "dt's commands"},
			} {
				before, rest, begun := strings.Cut(updated, b.begin)
				_, after, ended := strings.Cut(rest, b.end)
				if !begun || !ended {
					return fmt.Errorf("%s: no %s followed by %s", suitesDoc, b.begin, b.end)
				}
				if next := before + b.block + after; next != updated {
					stale = append(stale, fmt.Sprintf("%s: %s differs from %s; run test/harness/bin/dt docs and commit the result", suitesDoc, b.what, b.from))
					updated = next
				}
			}
			switch {
			case check:
				problems = append(problems, stale...)
			case updated != doc:
				if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
					return err
				}
			}
			for _, p := range problems {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), p)
			}
			if len(problems) > 0 {
				return exitStatus(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only check the suite table, the command reference and the RFCs' suites paragraphs, and exit 1 on a difference")
	return cmd
}

// Each RFC under rfcDir names the suites that cover its component in one
// paragraph that starts with rfcSuites, so whoever implements it knows which
// suite to extend.
const (
	rfcDir    = "docs/internals/rfcs"
	rfcSuites = "**Suites.**"
)

var (
	rfcFile  = regexp.MustCompile(`^rfc-\d+-.+\.md$`)
	codeSpan = regexp.MustCompile("`([^`]+)`")
)

// rfcProblems lists each RFC without its suites paragraph or with more than
// one, and each code span in one that isn't a suite, a group or an option of
// the registry, so a renamed suite can't leave an RFC naming the old one.
func rfcProblems(root string, r *registry.Registry) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, rfcDir))
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for g := range r.Groups {
		names[g] = true
	}
	for _, s := range r.Suites {
		for _, u := range r.Units(s) {
			names[u.Name] = true
		}
	}
	var problems []string
	for _, e := range entries {
		if !rfcFile.MatchString(e.Name()) {
			continue
		}
		path := rfcDir + "/" + e.Name()
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(data), "\n")
		found := 0
		for i, line := range lines {
			if !strings.HasPrefix(line, rfcSuites) {
				continue
			}
			found++
			// The paragraph runs to the next blank line.
			for j := i; j < len(lines) && strings.TrimSpace(lines[j]) != ""; j++ {
				for _, m := range codeSpan.FindAllStringSubmatch(lines[j], -1) {
					if !names[m[1]] {
						problems = append(problems, fmt.Sprintf("%s:%d: `%s` is no suite, group or option of %s", path, j+1, m[1], registry.Path))
					}
				}
			}
		}
		switch {
		case found == 0:
			problems = append(problems, fmt.Sprintf("%s: no paragraph starting %s names the suites that cover it", path, rfcSuites))
		case found > 1:
			problems = append(problems, fmt.Sprintf("%s: %d paragraphs start %s; one names them all", path, found, rfcSuites))
		}
	}
	return problems, nil
}

// suitesTable renders, markers included, a row for each suite graded against
// a known file and for each of its options, in the registry's order.
//
// decision: those suites are today's conformance suites, cthon04 and nfstest
// included, which the conformance group leaves out. Pick the rows by group
// once a suite of another kind gets a known file.
func suitesTable(r *registry.Registry) string {
	var b strings.Builder
	b.WriteString(suitesBegin + "\n<!-- Generated from " + registry.Path + " by test/harness/bin/dt docs. Do not edit by hand. -->\n\n")
	b.WriteString("| Suite | Protocol | Profiles | Variants | Tier | Known failures and skips |\n|---|---|---|---|---|---|\n")
	for _, s := range r.Suites {
		for _, u := range r.Units(s) {
			if u.Known == "" {
				continue
			}
			var variants []string
			for _, a := range u.Matrix {
				if a.Name != "profile" {
					variants = append(variants, a.Name+": "+code(a.Values...))
				}
			}
			_, _ = fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | [`%s`](../../%[6]s) |\n",
				u.Name, protocols(u.Fixture), code(profiles(u)...), none(strings.Join(variants, "; ")), code(u.Tier), u.Known)
		}
	}
	b.WriteString("\nFrom [`" + registry.Path + "`](../../" + registry.Path + "): its suites graded against a known file, and their options.\n")
	b.WriteString("Each runs in its tier and the wider ones, `pr` then `merge` then `nightly`; a `manual` one only when asked.\n")
	b.WriteString(suitesEnd)
	return b.String()
}

// commandsReference renders, markers included, each available command of dt
// under root, in cobra's order, with its usage line and flags: what commands
// dt has and what each takes. A suite's options are flags of dt run, so the
// registry changes it too.
func commandsReference(root *cobra.Command) string {
	var b strings.Builder
	b.WriteString(commandsBegin + "\n<!-- Generated from dt's commands by test/harness/bin/dt docs. Do not edit by hand. -->\n")
	var write func(c *cobra.Command)
	write = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if !sub.IsAvailableCommand() {
				continue
			}
			_, _ = fmt.Fprintf(&b, "\n### `%s`\n\n%s\n\n```text\n%s\n```\n", sub.CommandPath(), sub.Short, sub.UseLine())
			// Leave out --help, which every command has, but which cobra adds
			// only to the command it runs.
			flags := pflag.NewFlagSet(sub.Name(), pflag.ContinueOnError)
			sub.NonInheritedFlags().VisitAll(func(f *pflag.Flag) {
				if f.Name != "help" {
					flags.AddFlag(f)
				}
			})
			if usages := flags.FlagUsages(); usages != "" {
				_, _ = fmt.Fprintf(&b, "\nFlags:\n\n```text\n%s```\n", usages)
			}
			write(sub)
		}
	}
	write(root)
	b.WriteString("\n" + commandsEnd)
	return b.String()
}

// protocols names what the fixture serves.
func protocols(f *registry.Fixture) string {
	var p []string
	if f != nil && f.NFS.Serves() {
		p = append(p, "NFS")
	}
	if f != nil && f.SMB.Enabled {
		p = append(p, "SMB")
	}
	return none(strings.Join(p, ", "))
}

// profiles returns the unit's profile axis, or its fixture's one profile.
func profiles(u *registry.Suite) []string {
	for _, a := range u.Matrix {
		if a.Name == "profile" {
			return a.Values
		}
	}
	if u.Fixture != nil && u.Fixture.Profile != "" {
		return []string{u.Fixture.Profile}
	}
	return nil
}

// code joins values, each as code, and says none for no value.
func code(values ...string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "`" + v + "`"
	}
	return none(strings.Join(quoted, ", "))
}

func none(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
