#!/bin/sh
# One group of the nightly data cell: bench-data.sh GROUP OUTDIR runs the J rows of GROUP
# beside their raw twins at the small sizes, three counts of one iteration each, and writes
# OUTDIR/GROUP.txt for dt's go-bench reader and benchstat, with its cpu, mutex, block and heap
# profiles beside it. A directory on a RAM-backed filesystem fails the benchmark itself.
set -u

group=$1
out=$2
dir=${DITTOFS_BENCH_DIR:-$HOME/dittofs-bench}

case $group in
j1) pattern='^Benchmark(J1_|RawSequential$|RawRandom$)' ;;
j2) pattern='^Benchmark(J2_|RawScaling$)' ;;
j3) pattern='^BenchmarkJ3_' ;;
*) echo "bench-data.sh: unknown group $group" >&2; exit 2 ;;
esac

mkdir -p "$dir" "$out" || exit 2

# The binary go test keeps beside a profile goes to a scratch name in OUTDIR and is removed.
DITTOFS_BENCH_DIR=$dir DITTOFS_BENCH_SCALE=small go test -count=3 -benchtime=1x \
	-tags=journal_conformance,journal_bench -run '^$' -bench "$pattern" \
	-o "$out/$group.test" -outputdir "$out" \
	-cpuprofile "$group.cpu" -mutexprofile "$group.mutex" -blockprofile "$group.block" -memprofile "$group.heap" \
	./pkg/block/journal/conformance/ > "$out/$group.txt"
status=$?
rm -f "$out/$group.test"
cat "$out/$group.txt"

# The box the figures came from, beside them: the toolchain, then what the run printed before
# its first result (filesystem, mount options, the raw device's fsync probe). The commit is in
# dt's own summary.
{
	go version
	sed '/^goos:/q' "$out/$group.txt" | grep -v '^goos:'
} > "$out/meta.txt"

exit $status
