#!/bin/sh
# One entry of the dedicated host's scheduled bench runs: bench-run.sh small|full|prune.
# small and full each run one collection pass under $BENCH_RUN_ROOT, whose folders also
# hold the dt results tree; prune removes the run folders of either kind a month old
# (find compares whole days, so only a full 31 days is old enough) and nothing else.
# An unknown word is a usage error and an unset BENCH_RUN_ROOT
# refuses the run; both exit 2 on stderr. A refused or failed run exits non-zero and
# leaves what it created in place, never retried, never swallowed.
set -u
cd "$(dirname "$0")/../.." || exit 2

if [ $# -ne 1 ]; then
	echo "usage: bench-run.sh small|full|prune" >&2
	exit 2
fi

case $1 in
small | full)
	test -n "${BENCH_RUN_ROOT:-}" || { echo "set BENCH_RUN_ROOT to the root holding run folders" >&2; exit 2; }
	# Placeholder: exiting 2 refuses the scheduled run loudly rather than
	# recording an empty one.
	exit 2
	;;
prune)
	test -n "${BENCH_RUN_ROOT:-}" || { echo "set BENCH_RUN_ROOT to the root holding run folders" >&2; exit 2; }
	# Only this script's own kinds are pruned; the results tree and anything else a
	# hand run left under the root survives whatever its age. A missing kind directory
	# skips, so an empty or fresh root prunes nothing and still exits 0; a kind whose
	# find fails sets rc, the other kind still prunes, and the failure leaves in the status.
	rc=0
	for k in small full; do
		d=$BENCH_RUN_ROOT/$k
		[ -d "$d" ] || continue
		find "$d" -mindepth 1 -maxdepth 1 -mtime +30 -exec rm -rf {} + || rc=1
	done
	exit $rc
	;;
*)
	echo "usage: bench-run.sh small|full|prune" >&2
	exit 2
	;;
esac
