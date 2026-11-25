#!/bin/sh
# One entry of the dedicated host's scheduled bench runs: bench-run.sh small|full|prune.
# small and full run in a dedicated checkout — BENCH_CHECKOUT names it and the
# guard enforces it — whose tree each moves to
# the branch tip before anything is recorded. Each runs one collection pass
# under $BENCH_RUN_ROOT, whose folders also
# hold the dt results tree; prune removes the run folders of either kind, and
# dt's own run dirs under .dt-results, a month old
# (find compares whole days, so only a full 31 days is old enough) and nothing
# else. BENCH_NOT_AFTER is the latest permitted start, in epoch seconds: the
# scheduled environment computes it, the worst-case duration and the lock-wait
# budget subtracted from the night's start; a hand run sets it far in the
# future. An unknown word is a usage error, and an unset BENCH_RUN_ROOT,
# BENCH_NOT_AFTER or BENCH_CHECKOUT
# refuses the run; the exits are 2 on stderr. A refused or failed run exits non-zero and
# leaves what it created in place, never retried, never swallowed.
set -u

if [ $# -ne 1 ]; then
	echo "usage: bench-run.sh small|full|prune" >&2
	exit 2
fi

case $1 in
small | full)
	test -n "${BENCH_RUN_ROOT:-}" || { echo "set BENCH_RUN_ROOT to the root holding run folders" >&2; exit 2; }
	kind=$1
	test -n "${BENCH_NOT_AFTER:-}" || { echo "set BENCH_NOT_AFTER to the latest start epoch that still ends before the nightly work" >&2; exit 2; }
	test -n "${BENCH_CHECKOUT:-}" || { echo "set BENCH_CHECKOUT to the dedicated checkout of the scheduled runs" >&2; exit 2; }
	[ -d "$BENCH_CHECKOUT" ] || { echo "no such checkout directory" >&2; exit 2; }
	# Guard time: a threshold that is not a whole number would error [ -ge ]
	# inside the wait, and the errors the wait then skips turn it silently off.
	# The two wait budgets take the same guard: a spelled-out duration errors
	# the shell arithmetic inside its wait, past the point of refusing well.
	case "${BENCH_QUIET_LOAD:-4}" in
	'' | *[!0-9]*) echo "BENCH_QUIET_LOAD must be a whole number" >&2; exit 2 ;;
	esac
	case "${BENCH_QUIET_LOAD_MAX_WAIT:-14400}" in
	'' | *[!0-9]*) echo "BENCH_QUIET_LOAD_MAX_WAIT must be a whole number of seconds" >&2; exit 2 ;;
	esac
	case "${BENCH_LOCK_WAIT:-7200}" in
	'' | *[!0-9]*) echo "BENCH_LOCK_WAIT must be a whole number of seconds" >&2; exit 2 ;;
	esac
	# Before the lock and the quiet wait, so a past-deadline run refuses at
	# once instead of sitting in either wait.
	[ "$(date +%s)" -lt "$BENCH_NOT_AFTER" ] || { echo "too late to start before the nightly work" >&2; exit 2; }
	# The suite calls, the git steps and the prune self-call all resolve
	# inside this checkout, never in whatever tree the script was invoked from.
	cd "$BENCH_CHECKOUT" || exit 2

	# One bench run at a time: a lock held past its wait refuses the run
	# rather than racing the other kind over the same host. The lock is
	# taken before the quiet wait, so the wait re-judges the load only after
	# the run has the host: waiting on the lock and then starting while the
	# other kind's load is still high is the overlap the lock prevents.
	# decision: an unset BENCH_LOCK takes no lock and runs anyway — the
	# scheduled environment always sets it, and a hand run has no host lock
	# to take. The exemption is worth only what a forgotten cron env costs
	# (an overlap the lock exists to prevent); refuse unset instead if the
	# host ever shows one.
	if [ -n "${BENCH_LOCK:-}" ]; then
		exec 9>"$BENCH_LOCK"
		flock -w "${BENCH_LOCK_WAIT:-7200}" 9 || { echo "lock $BENCH_LOCK not free within ${BENCH_LOCK_WAIT:-7200}s" >&2; exit 2; }
	fi

	# A load that never drops under the threshold within the budget refuses
	# the run instead of running hot.
	# decision: a missing or unreadable /proc/loadavg skips the quiet wait
	# and runs immediately (a dev machine has no /proc; the bench host has
	# it). The exemption is worth only what running hot costs — take on the
	# host check before pointing this script at a host without /proc.
	if [ -r /proc/loadavg ]; then
		deadline=$(( $(date +%s) + ${BENCH_QUIET_LOAD_MAX_WAIT:-14400} ))
		while [ "$(cut -d' ' -f1 /proc/loadavg | cut -d. -f1)" -ge "${BENCH_QUIET_LOAD:-4}" ]; do
			[ "$(date +%s)" -lt "$deadline" ] || { echo "host not quiet under ${BENCH_QUIET_LOAD:-4} within ${BENCH_QUIET_LOAD_MAX_WAIT:-14400}s" >&2; exit 2; }
			sleep 300
		done
	fi

	# After both waits: the budget the waits ate may leave the night's work
	# too close to start at all.
	[ "$(date +%s)" -lt "$BENCH_NOT_AFTER" ] || { echo "too late to start before the nightly work" >&2; exit 2; }
	# Update before anything is recorded: a fetch or checkout failure aborts
	# here, so no run folder is ever filed against a stale commit. The
	# branch is hardcoded because these scripts retire with it once it
	# reaches develop — nothing outlives the branch to point elsewhere.
	# Uncommitted work in this dedicated checkout is not kept.
	git fetch origin feat/storage-v2 || { echo "git fetch failed" >&2; exit 2; }
	# Detached at the tip, never onto the branch: a hand run must not find
	# its worktree's branch ref moved.
	git checkout -f --detach FETCH_HEAD || { echo "git checkout failed" >&2; exit 2; }

	# The previous folder of the kind feeds benchstat; it is read before the
	# run dir exists so it can never be the run itself. Only the newest
	# folder holding the done marker is a candidate: a night that did not
	# fully succeed is dropped as a baseline, partial figures included.
	prev=
	if [ -d "$BENCH_RUN_ROOT/$kind" ]; then
		prev=$(ls -1dt "$BENCH_RUN_ROOT/$kind"/*/ 2>/dev/null | while IFS= read -r d; do
			[ -f "$d/done" ] && { printf '%s\n' "$d"; break; }
		done)
	fi
	rundir=$BENCH_RUN_ROOT/$kind/$(date -u +%Y%m%dT%H%M%SZ)-$(git rev-parse --short=12 HEAD)
	mkdir -p "$rundir" || exit 2
	git rev-parse HEAD >"$rundir/commit.txt"

	s=0
	if [ "$kind" = small ]; then
		# Three collection groups; one group's failure is recorded and the
		# later groups still run.
		for g in j1 j2 j3; do
			sh test/journal/bench-data.sh "$g" "$rundir" || s=$?
		done
	else
		JOURNAL_BENCH_COUNT=10 sh test/harness/bin/dt run journal-bench \
			--results "$BENCH_RUN_ROOT/.dt-results" >"$rundir/dt.log" 2>&1
		s=$?
		cat "$rundir/dt.log"
		out=$(sed -n 's/^results: //p' "$rundir/dt.log" | tail -1)
		# Losing the figures to a missing trailer or result file must not
		# leave the run looking complete.
		if ! { [ -n "$out" ] && cp "$out/journal-bench/bench.txt" "$rundir/bench.txt"; }; then
			echo "no dt trailer or no bench.txt to copy from $out" >&2
			# The first status stays: a dt refusal (its own exit 2) must not
			# be overwritten into a generic failure.
			[ "$s" -ne 0 ] || s=1
		fi
	fi

	# The marker says the run's bench work fully succeeded; only a folder
	# holding it is a fresh collection or a benchstat candidate.
	# decision: a partially failed small night is dropped too, even though
	# its later groups wrote their files — a night's figures compare only
	# against a night that completed them all, and the failed night still
	# surfaces through the run's exit status and its kept files.
	if [ "$s" -eq 0 ]; then
		: >"$rundir/done"
	fi

	# benchstat runs only when this run completed, the binary exists and a
	# finished previous run of the kind does; otherwise the run completes
	# without benchstat.txt, keeping a failed night's figures out of the
	# comparison. A failing benchstat reaches the run's status: the
	# half-written benchstat.txt may stay behind, the exit status says what
	# happened.
	if [ -f "$rundir/done" ] && command -v benchstat >/dev/null && [ -n "$prev" ]; then
		for f in bench.txt j1.txt j2.txt j3.txt; do
			[ -f "$prev/$f" ] && [ -f "$rundir/$f" ] || continue
			{ echo "### $f"; benchstat -ignore commit "$prev/$f" "$rundir/$f"; } >>"$rundir/benchstat.txt" || s=$?
		done
	fi

	# An export failure leaves the run folder in place and its status in the
	# run's exit, alongside any earlier failure.
	if [ -n "${BENCH_EXPORT_DEST:-}" ]; then
		sh test/journal/bench-export.sh "$rundir" || s=$?
	fi
	# A prune failure leaves in the run's status, like the export's does.
	sh test/journal/bench-run.sh prune || s=$?
	exit "$s"
	;;
prune)
	cd "$(dirname "$0")/../.." || exit 2
	test -n "${BENCH_RUN_ROOT:-}" || { echo "set BENCH_RUN_ROOT to the root holding run folders" >&2; exit 2; }
	# Only this script's own kinds and dt's results tree are pruned: dt's
	# run dirs share the same window — each one's bench.txt is already copied
	# into the run folder — while anything else a hand run left under the
	# root survives, whatever its age. A missing directory
	# skips, so an empty or fresh root prunes nothing and still exits 0; a directory whose
	# find fails sets rc, the others still prune, and the failure leaves in the status.
	rc=0
	for k in small full .dt-results; do
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
