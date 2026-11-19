package run

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// readGoBench reads what go test -bench wrote to path, run at GOMAXPROCS
// procs: each benchmark's number for each unit, by "NAME UNIT", as
// "BenchmarkAppend/4KiB ns/op", the -procs suffix go test adds past one CPU
// left out, and why the file shows a failure: a --- FAIL line, or one
// benchmark name in two packages, whose figures would merge. A benchmark
// that prints has its name and its figures on separate lines.
//
// decision: a benchmark run more than once, by -count, keeps the median of
// its runs, which one slow run doesn't move. The samples stay in the file for
// benchstat; the median is for history.
func readGoBench(path string, procs int) (map[string]float64, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	suffix := "-" + strconv.Itoa(procs)
	if procs == 1 {
		suffix = ""
	}
	runs := map[string][]float64{}
	pkgOf := map[string]string{}
	var failed []string
	var pkg, name string
	for line := range strings.Lines(string(data)) {
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
			continue
		case strings.HasPrefix(line, "--- FAIL: "):
			failed = append(failed, strings.TrimSpace(line))
			continue
		case len(f) == 2 && f[0] == "pkg:":
			pkg = f[1]
			continue
		case strings.HasPrefix(f[0], "Benchmark"):
			name = strings.TrimSuffix(f[0], suffix)
			if p, ok := pkgOf[name]; ok && p != pkg {
				failed = append(failed, fmt.Sprintf("%s ran in %s and %s", name, p, pkg))
			}
			pkgOf[name], f = pkg, f[1:]
		}
		if name == "" || len(f) < 3 || len(f)%2 == 0 {
			continue
		}
		if _, err := strconv.Atoi(f[0]); err != nil {
			continue
		}
		for i := 1; i < len(f); i += 2 {
			if v, err := strconv.ParseFloat(f[i], 64); err == nil {
				runs[name+" "+f[i+1]] = append(runs[name+" "+f[i+1]], v)
			}
		}
	}
	if len(runs) == 0 {
		return nil, failed, fmt.Errorf("%s holds no benchmark lines", filepath.Base(path))
	}
	numbers := make(map[string]float64, len(runs))
	for key, vs := range runs {
		numbers[key] = median(vs)
	}
	return numbers, failed, nil
}

// median is the middle of vs, sorted in place, or the mean of the two in the
// middle.
func median(vs []float64) float64 {
	slices.Sort(vs)
	m := len(vs) / 2
	if len(vs)%2 == 1 {
		return vs[m]
	}
	return (vs[m-1] + vs[m]) / 2
}

// gomaxprocs is the GOMAXPROCS a command run with env takes: the last
// GOMAXPROCS=N in env, else this process's, which a go test run on this host
// takes too.
func gomaxprocs(env []string) int {
	for _, kv := range slices.Backward(env) {
		if v, ok := strings.CutPrefix(kv, "GOMAXPROCS="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return runtime.GOMAXPROCS(0)
}
