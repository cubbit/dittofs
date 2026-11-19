package plan

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	suiteIs = regexp.MustCompile(`\.suite == "([^"]+)"`)
	inList  = regexp.MustCompile(`IN\(\.(suite|name);([^)]*)\)`)
	quoted  = regexp.MustCompile(`"([^"]+)"`)
	stepRun = regexp.MustCompile(`"\$RUNNER_TEMP/dt" run ([a-z0-9./-]+) `)
)

// reach is what tests.yml says about the plan's cells: the suites and cell
// names its plan filter keeps, those its storage-v2 skip leaves out on
// purpose, and the cells a step runs by name.
type reach struct {
	kept, skipped, stepped map[string]bool
}

func readWorkflow(t *testing.T) reach {
	t.Helper()
	b, err := os.ReadFile("../../../.github/workflows/tests.yml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	skip := strings.Index(s, `select($v2 != "true"`)
	keep := strings.Index(s, `select(.job or`)
	end := strings.Index(s[max(keep, 0):], `| .slug`)
	if skip < 0 || keep < skip || end < 0 {
		t.Fatal("tests.yml's plan filter no longer reads as its storage-v2 skip, then its keep, then .slug")
	}
	r := reach{kept: terms(s[keep : keep+end]), skipped: terms(s[skip:keep]), stepped: map[string]bool{}}
	for _, m := range stepRun.FindAllStringSubmatch(s, -1) {
		r.stepped[m[1]] = true
	}
	return r
}

// terms is the suites and cell names a part of the filter names, as
// "suite:NAME" and "name:NAME".
func terms(part string) map[string]bool {
	out := map[string]bool{}
	for _, m := range suiteIs.FindAllStringSubmatch(part, -1) {
		out["suite:"+m[1]] = true
	}
	for _, m := range inList.FindAllStringSubmatch(part, -1) {
		for _, q := range quoted.FindAllStringSubmatch(m[2], -1) {
			out[m[1]+":"+q[1]] = true
		}
	}
	return out
}

func (r reach) runs(c Cell) bool {
	return c.Job != "" || r.stepped[c.Name] ||
		r.kept["suite:"+c.Suite] || r.kept["name:"+c.Name] ||
		r.skipped["suite:"+c.Suite] || r.skipped["name:"+c.Name]
}

// Every pr and merge cell reaches tests.yml: it names a job, the plan
// filter keeps it, a step runs it by name, or the filter's storage-v2 skip
// leaves it out on purpose. A cell none of these reach never runs at its
// tier, so a suite or option registered without a way into the workflow
// fails here rather than enforcing nothing.
func TestEveryTierCellReachesTheWorkflow(t *testing.T) {
	r := readWorkflow(t)
	for _, tier := range []string{"pr", "merge"} {
		cells, err := Plan("../../..", repo(t), tier, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cells {
			if !r.runs(c) {
				t.Errorf("%s cell %s: tests.yml gives it no job, its plan filter neither keeps nor skips it, and no step runs it", tier, c.Name)
			}
		}
	}
}
