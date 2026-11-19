package run

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// procs is the -GOMAXPROCS suffix go test adds to a benchmark's name.
var procs = regexp.MustCompile(`-\d+$`)

// readGoBench reads the benchmark lines go test wrote to path, as
// "BenchmarkAppend/4KiB-8  3  1234 ns/op  56 B/op  7 allocs/op": each
// benchmark's number for each unit, by "NAME UNIT", as
// "BenchmarkAppend/4KiB ns/op", the -GOMAXPROCS suffix left out.
//
// decision: a benchmark run more than once, by -count, keeps the median of
// its runs, which one slow run doesn't move.
func readGoBench(path string) (map[string]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	runs := map[string][]float64{}
	for line := range strings.Lines(string(data)) {
		f := strings.Fields(line)
		if len(f) < 4 || len(f)%2 != 0 || !strings.HasPrefix(f[0], "Benchmark") {
			continue
		}
		if _, err := strconv.Atoi(f[1]); err != nil {
			continue
		}
		name := procs.ReplaceAllString(f[0], "")
		for i := 2; i < len(f); i += 2 {
			if v, err := strconv.ParseFloat(f[i], 64); err == nil {
				runs[name+" "+f[i+1]] = append(runs[name+" "+f[i+1]], v)
			}
		}
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("%s holds no benchmark lines", filepath.Base(path))
	}
	numbers := make(map[string]float64, len(runs))
	for key, vs := range runs {
		slices.Sort(vs)
		mid := len(vs) / 2
		numbers[key] = vs[mid]
		if len(vs)%2 == 0 {
			numbers[key] = (vs[mid-1] + vs[mid]) / 2
		}
	}
	return numbers, nil
}
