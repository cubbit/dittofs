#!/usr/bin/env bash
#
# A commit that calls itself documentation changes documentation.
#
# `docs:` and `chore(release):` are the two types that say, in the subject line,
# that nothing executable moved. A reviewer reads the subject and skips the
# diff, so a commit that claims one of them and carries Go source or a workflow
# is the one change nobody looks at. That is not hypothetical: a two-line
# ROADMAP edit titled `docs(planning):` once carried 830 deletions across 15
# files, reverted two merged security fixes, and stayed green because the
# regression tests went with them.
#
# Allowed paths for those two types: docs/**, .planning/**, any *.md, README*.
#
# `chore(release)` additionally allows flake.nix, and only flake.nix: a release
# commit bumps the version there and changes nothing else, so refusing it would
# block every release rather than catch anything.
#
# The one exception is a Go file whose diff touches only `//` comment lines and
# blank lines — a typo fix in a doc comment is a documentation change whatever
# file it lives in. The exception applies to modified files only: an added,
# deleted or renamed .go file is a structural change regardless of its
# contents, which is precisely the case above. Block comments (/* */) are not
# recognised, so a typo fix inside one fails closed and wants a different type.
#
# Merge commits carry no meaningful type and are skipped. So are reverts: the
# type in `Revert "docs(x): y"` describes the commit being undone, not this
# diff, and it does not parse as a conventional type anyway.
#
# The merge queue squashes a pull request into one commit under the pull
# request's title, so the title is the subject that lands. With PR_TITLE set,
# as pr-title.yml sets it, the title is judged too, against the branch's whole
# diff: a pull request titled docs: whose commits are typed right but whose
# diff carries code then fails on the pull request, not in the queue.
#
# Usage: test/commit-type-matches-diff.sh [base-ref]   (default: origin/develop)

set -euo pipefail

base="${1:-origin/develop}"

if ! merge_base=$(git merge-base "$base" HEAD 2>/dev/null); then
	echo "cannot find a merge base with ${base}: fetch it first (fetch-depth: 0)" >&2
	exit 2
fi

# Paths a documentation commit is allowed to touch.
is_doc_path() {
	case "$1" in
	docs/* | .planning/* | *.md | README*) return 0 ;;
	*) return 1 ;;
	esac
}

# The changes of commit $1 to path $2, or the branch's when $1 is empty, with
# no context lines.
diff_of() {
	if [ -n "$1" ]; then
		git show --format='' --unified=0 "$1" -- "$2"
	else
		git diff --unified=0 "$merge_base" HEAD -- "$2"
	fi
}

# The paths commit $1 changes, or the branch when $1 is empty, as
# status<TAB>path[<TAB>renamed-to] lines.
changes_of() {
	if [ -n "$1" ]; then
		git show --name-status --format='' "$1"
	else
		git diff --name-status "$merge_base" HEAD
	fi
}

# True when every changed line in this file is a // comment or blank. Context
# lines are excluded by --unified=0; the +++/--- headers are dropped by name.
#
# Counts the code lines rather than testing for one with `grep -q`. -q exits on
# the first match, closing the pipe under a sed that is still writing, and
# pipefail then reports that SIGPIPE as the pipeline's status; negating it reads
# a large diff full of code as "no code found". That fails open on exactly the
# big rewrite this script exists to catch, and only on the big ones — a short
# diff finishes writing before the pipe closes and is judged correctly.
#
# A comment counts as prose only when the // is followed by whitespace or ends
# the line. A directive is not prose: //go:build can remove a whole file from
# the build, //go:embed changes what ships, //go:linkname changes what a symbol
# resolves to. Requiring the space rejects every //word form at once, including
# directives Go has not invented yet, and the two that do carry a space are
# named. A comment written without the space reads as code and fails closed,
# which costs an author one retyped commit subject.
go_comments_only() {
	local diff code
	# A diff git can't produce fails closed: the file counts as code.
	diff=$(diff_of "$1" "$2") || return 1
	code=$(printf '%s\n' "$diff" |
		grep -E '^[+-]' |
		grep -Ev '^(\+\+\+|---)' |
		sed -E 's/^[+-][[:space:]]*//' |
		awk '
			/^$/                      { next }
			/^\/\/ \+build/           { code++; next }
			/^\/\/ #cgo/              { code++; next }
			/^\/\/([[:space:]]|$)/    { next }
			                          { code++ }
			END                       { print code + 0 }
		' || true)
	[ "${code:-0}" -eq 0 ]
}

# judge SUBJECT SHA LABEL: fails, naming the paths, when SUBJECT claims docs:
# or chore(release): and commit SHA (the branch when empty) changes a path
# outside what that type allows.
judge() {
	local subject=$1 sha=$2 label=$3 type scope release offenders st path rename_to changes
	# Conventional type and optional scope: "type(scope)!: summary".
	if [[ ! "$subject" =~ ^([a-z]+)(\(([^\)]*)\))?!?: ]]; then
		return 0
	fi
	type="${BASH_REMATCH[1]}"
	scope="${BASH_REMATCH[3]:-}"

	release=false
	if [ "$type$scope" = "chorerelease" ]; then
		release=true
	elif [ "$type" != "docs" ]; then
		return 0
	fi

	# Read before the loop, so a git failure fails the check instead of
	# reading as no changes.
	if ! changes=$(changes_of "$sha"); then
		echo "${label} ${subject}: git could not list its changes" >&2
		return 2
	fi
	offenders=""
	while IFS=$'\t' read -r st path rename_to; do
		[ -n "$path" ] || continue
		# Renames report the destination in a third field; judge that.
		[ "${st:0:1}" = "R" ] && path="$rename_to"
		is_doc_path "$path" && continue
		# The version bump a release commit exists to make.
		if [ "$release" = true ] && [ "$path" = "flake.nix" ]; then
			continue
		fi
		if [ "${path##*.}" = "go" ] && [ "$st" = "M" ] && go_comments_only "$sha" "$path"; then
			continue
		fi
		offenders+="  ${st}  ${path}"$'\n'
	done <<<"$changes"

	if [ -n "$offenders" ]; then
		echo "${label} ${subject}" >&2
		printf '%s' "$offenders" >&2
		return 1
	fi
}

status=0

for sha in $(git rev-list --no-merges "${merge_base}..HEAD"); do
	# First physical line only. %s folds a subject that wraps before the
	# first blank line into one line, which would hide a second line. sed
	# rather than head: head closes the pipe early, and on a long message
	# pipefail turns git's SIGPIPE into exit 141.
	subject=$(git show -s --format=%B "$sha" | sed -n 1p)
	judge "$subject" "$sha" "${sha:0:9}" || status=1
done

title_failed=false
if [ -n "${PR_TITLE:-}" ] && ! judge "$PR_TITLE" "" "the pull request's title:"; then
	status=1
	title_failed=true
fi

if [ "$status" -ne 0 ]; then
	echo >&2
	echo "A docs: or chore(release): commit may only touch docs/**, .planning/**," >&2
	echo "*.md or README*. A modified .go file is also allowed when its diff changes" >&2
	echo "only // comment and blank lines, and chore(release): may bump flake.nix." >&2
	echo >&2
	echo "Retype the commit for what it actually changes, or split the code out of it." >&2
	if [ "$title_failed" = true ]; then
		echo "The merge queue squashes the pull request under its title: retitle it for what" >&2
		echo "the branch changes, and this check runs again on the edit." >&2
	fi
	exit 1
fi

if [ -n "${PR_TITLE:-}" ]; then
	echo "every docs: and chore(release): commit, and the pull request's title, stays inside its allowed paths"
else
	echo "every docs: and chore(release): commit stays inside its allowed paths"
fi
