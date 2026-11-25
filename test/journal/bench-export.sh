#!/bin/sh
# The scheduled pass after a bench run: bench-export.sh DIR syncs the run
# folder into $BENCH_EXPORT_DEST/<parent-name>/<folder-name>, so the two kinds
# read apart in the bucket listing, and bench-export.sh --freshness DIR exits
# 0 while DIR's newest finished run — the newest entry holding a done marker,
# which bench-run.sh writes only when the bench work succeeded — is younger
# than $BENCH_FRESHNESS_DAYS nights (default 2); otherwise it raises one gh
# issue, skipping while one is already open. --freshness DIR is a KIND folder
# (the small or full directory, e.g. $ROOT/small), never the run root: prune
# touches the kind dirs, so a root read stays falsely young for a month.
# Retention: the local run folders go only after a full 31 days
# (bench-run.sh prune; find compares whole days), the bucket copy is kept. A
# relative DIR resolves against the repo root once the script cd's there, so
# callers pass an absolute path; a --freshness DIR that does not exist reads
# as stale — a dead collection must alert, never look healthy. The threshold
# leans on a date that accepts -d relative dates. A refused or failed run
# exits non-zero and leaves what it created in place, never retried, never
# swallowed.
set -u
cd "$(dirname "$0")/../.." || exit 2

if [ $# -eq 0 ]; then
	echo "usage: bench-export.sh <dir>|--freshness <dir>" >&2
	exit 2
fi

case $1 in
--freshness)
	dir=${2:-}
	[ -n "$dir" ] || { echo "usage: bench-export.sh --freshness <dir>" >&2; exit 2; }
	# newest finished entry by mtime, no find: a glob and -nt stay inside
	# the shell. Only a folder holding a done marker competes — a run whose
	# night failed wrote no metrics, so however young it is it must not read
	# as a fresh collection. An empty dir (or one whose runs all failed)
	# leaves newest empty, which reads as stale: a dead collection must
	# never read as healthy. The -d skips the literal, unexpanded pattern
	# such a dir leaves in the loop.
	newest=
	for d in "$dir"/*/; do
		[ -d "$d" ] || continue
		[ -f "$d/done" ] || continue
		[ -z "$newest" ] || [ "$d" -nt "$newest" ] || continue
		newest=$d
	done
	# mktemp, not a predictable name: a shared /tmp lets another user plant a
	# symlink at a guessable path whose target the touch below would alter;
	# mktemp refuses that race.
	ref=$(mktemp) || { echo "cannot create the freshness reference file" >&2; exit 2; }
	touch -t "$(date -d "-${BENCH_FRESHNESS_DAYS:-2} days" +%Y%m%d%H%M)" "$ref" || {
		rm -f "$ref"
		echo "cannot compute the ${BENCH_FRESHNESS_DAYS:-2}-night threshold" >&2; exit 2; }
	# decision: the ref file stays until after the comparison. -nt is true
	# whenever its right operand is missing, so a ref deleted before the test
	# would make even an empty dir read fresh.
	[ -n "$newest" ] && [ "$newest" -nt "$ref" ] && { rm -f "$ref"; exit 0; }
	rm -f "$ref"
	# stale: one issue, never a second while one is open. The open list is
	# captured once — a second, unguarded call could fail empty and raise a
	# duplicate alert. The repo is named because several remotes or an old
	# origin make gh ask which one or silently drop; gh's own stderr passes
	# through beside the wrapper message. Deduplication is by label, which
	# the open list asks for and the created issue carries.
	open=$(gh issue list -R cubbit/dittofs --state open --label bench-data-stale) ||
		{ echo "gh issue list failed (gh missing or not authenticated)" >&2; exit 2; }
	[ -z "$open" ] || exit 0
	gh issue create -R cubbit/dittofs \
		--label bench-data-stale \
		--title "journal bench data: no finished run in ${BENCH_FRESHNESS_DAYS:-2} nights (bench-data-stale)" \
		--body "newest finished run folder in $dir: ${newest:-none} — raised by the bench host's freshness check" \
		|| { echo "gh issue create failed" >&2; exit 2; }
	exit 0 ;;
*)
	dir=$1
	[ -d "$dir" ] || { echo "no such run directory: $dir" >&2; exit 2; }
	test -n "${BENCH_EXPORT_DEST:-}" || { echo "set BENCH_EXPORT_DEST to the bucket prefix (s3://…)" >&2; exit 2; }
	command -v aws >/dev/null || { echo "aws not found for s3 sync" >&2; exit 2; }
	# The run's parent travels with it: $DEST/<parent-name>/<folder-name>/
	# keeps the small and full runs readable apart in the bucket listing.
	exec aws s3 sync "$dir" "${BENCH_EXPORT_DEST%/}/$(basename "$(dirname "$dir")")/$(basename "$dir")/" ;;
esac
