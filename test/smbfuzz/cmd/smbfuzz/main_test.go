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
