// Package archtest is the architecture tests' toolkit: an importer that reads
// each package's types from the export data the go command compiled, so a
// test that type-checks one package's files does not type-check every
// dependency from source; an export pin's printer (Exports, Violations); a
// purity check (Impurities); and a transitive import walk over go list's
// graph, per platform, with or without tests (Walk, Check). Planted files
// reach the pin and the purity check as extra paths, or through WithHidden
// behind a build tag; a planted module reaches the walk as a directory. Only
// tests import it.
package archtest

import (
	"bytes"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/cubbit/dittofs/internal/prof"
)

// ExportImporter returns an importer for the packages that patterns name
// and every package they import, as go list resolves the patterns from the
// current directory, which go test sets to the package under test. The
// first import runs go list -export once; it compiles what the build cache
// lacks, with the race detector when this binary has it, so the build of
// this test binary already cached them. A package go list does not report
// fails its import.
func ExportImporter(patterns ...string) types.Importer {
	exports := sync.OnceValues(func() (map[string]string, error) {
		return exportFiles(patterns)
	})

	return &lockedImporter{imp: importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		files, err := exports()
		if err != nil {
			return nil, err
		}

		if files[path] == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}

		return os.Open(files[path])
	})}
}

// lockedImporter imports one package at a time: the gc importer fills a
// package map with no lock of its own.
type lockedImporter struct {
	mu  sync.Mutex
	imp types.Importer
}

func (l *lockedImporter) Import(path string) (*types.Package, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.imp.Import(path)
}

// exportFiles lists the export data file of each package patterns name and
// of every package they import, by import path.
func exportFiles(patterns []string) (map[string]string, error) {
	args := []string{"list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}"}
	if prof.RaceEnabled {
		args = append(args, "-race")
	}

	cmd := exec.Command("go", append(args, patterns...)...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -export: %w: %s", err, stderr.String())
	}

	files := map[string]string{}

	for line := range strings.Lines(string(out)) {
		path, file, _ := strings.Cut(strings.TrimSpace(line), "=")
		files[path] = file
	}

	return files, nil
}
