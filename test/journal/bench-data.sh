#!/bin/sh
# One group of the nightly data cell: bench-data.sh GROUP OUTDIR runs the J rows of GROUP
# beside their raw twins at the small sizes, ten counts of one iteration each, and writes
# OUTDIR/GROUP.txt for dt's go-bench reader and benchstat, with its cpu, mutex, block and heap
# profiles beside it. A directory on a RAM-backed filesystem fails the benchmark itself.
set -u

group=$1
out=$2
test -n "${DITTOFS_BENCH_DIR:-}" || { echo "set DITTOFS_BENCH_DIR to a directory on the device under test" >&2; exit 2; }
dir=$DITTOFS_BENCH_DIR
# The profilers' sampling, one place: every 100 µs blocked, one mutex event in 100. Rate 1 would
# distort the figures recorded in the same run.
block_rate=100000
mutex_fraction=100

case $group in
j1) pattern='^Benchmark(J1_|RawSequential$|RawRandom$)' ;;
j2) pattern='^Benchmark(J2_|RawScaling$)' ;;
j3) pattern='^BenchmarkJ3_' ;;
*) echo "bench-data.sh: unknown group $group" >&2; exit 2 ;;
esac

mkdir -p "$out" || exit 2

# A run that timed out or was killed leaves its journal-bench-* directory behind; they would pile
# up on the device unseen, so the stale ones, by that name alone, go at the start.
rm -rf "$dir"/journal-bench-*

# The binary go test keeps beside a profile goes to a scratch name in OUTDIR and is removed.
DITTOFS_BENCH_DIR=$dir DITTOFS_BENCH_SCALE=small go test -count=10 -benchtime=1x \
	-tags=journal_conformance,journal_bench -run '^$' -bench "$pattern" \
	-o "$out/$group.test" -outputdir "$out" \
	-cpuprofile "$group.cpu" -mutexprofile "$group.mutex" -mutexprofilefraction "$mutex_fraction" \
	-blockprofile "$group.block" -blockprofilerate "$block_rate" -memprofile "$group.heap" \
	./pkg/block/journal/conformance/ > "$out/$group.txt"
status=$?
rm -f "$out/$group.test"
cat "$out/$group.txt"

# The box the figures came from, beside them, written once by the first group that has it: the
# toolchain, then what the run printed before its first result (filesystem, mount options, the
# raw device's fsync probe). The commit is in dt's own summary.
if [ ! -s "$out/meta.txt" ] && grep -q '^goos:' "$out/$group.txt"; then
	{
		go version
		sed '/^goos:/q' "$out/$group.txt" | grep -v -e '^goos:' -e '^commit:'
	} > "$out/meta.txt"
fi

exit $status
