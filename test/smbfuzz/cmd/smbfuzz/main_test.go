//go:build linux

package main

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/test/smbfuzz/fuzz"
)

// `smbfuzz plan --round N` prints round N of the newest generator.
func TestPlanCommand(t *testing.T) {
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := planCmd([]string{"--round", "42"})
	os.Stdout = old
	_ = w.Close()
	got, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := fuzz.Generate(fuzz.LatestGen, 42)
	if string(got) != p.Format() {
		t.Fatalf("got:\n%s\nwant:\n%s", got, p.Format())
	}
}

// dt grades only 75 as not run; a divergence must still fail the run when
// other rounds did not run.
func TestExitCode(t *testing.T) {
	for _, c := range []struct{ failed, notRun, want int }{
		{0, 0, 0}, {1, 0, 1}, {0, 1, 75}, {1, 1, 1},
	} {
		if got := exitCode(c.failed, c.notRun); got != c.want {
			t.Errorf("exitCode(%d failed, %d not run) = %d, want %d", c.failed, c.notRun, got, c.want)
		}
	}
}

// Each round is a JUnit test case read the way dt's junit reader reads one
// (test/harness/run/readers.go): named by its code attribute, failed by a
// failure element, skipped by a skipped one, each with its message.
func TestJUnitRounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smb-fuzz.xml")
	cases := []junitCase{
		{Code: "round-1", Name: "round-1", Classname: "smb-fuzz"},
		{Code: "round-2", Name: "round-2", Classname: "smb-fuzz", Failure: &junitMessage{Message: "stat:size:dir at step 03"}},
		{Code: "round-3", Name: "round-3", Classname: "smb-fuzz", Skipped: &junitMessage{Message: "not run: tree setup"}},
	}
	if err := writeJUnit(path, cases); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		Message string `xml:"message,attr"`
	}
	var suite struct {
		Cases []struct {
			Code    string  `xml:"code,attr"`
			Failure *result `xml:"failure"`
			Skipped *result `xml:"skipped"`
		} `xml:"testcase"`
	}
	if err := xml.Unmarshal(b, &suite); err != nil {
		t.Fatal(err)
	}
	if len(suite.Cases) != 3 {
		t.Fatalf("%d test cases:\n%s", len(suite.Cases), b)
	}
	c := suite.Cases
	if c[0].Code != "round-1" || c[0].Failure != nil || c[0].Skipped != nil ||
		c[1].Code != "round-2" || c[1].Failure == nil || c[1].Failure.Message != "stat:size:dir at step 03" ||
		c[2].Code != "round-3" || c[2].Skipped == nil || c[2].Skipped.Message != "not run: tree setup" {
		t.Fatalf("read back as %+v from:\n%s", c, b)
	}
}
