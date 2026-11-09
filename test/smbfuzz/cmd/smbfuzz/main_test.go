//go:build linux

package main

import (
	"io"
	"os"
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
