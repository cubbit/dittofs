#!/usr/bin/env bash
# Tests for commit-type-matches-diff.sh: each case builds a branch in a
# throwaway repository and runs the script against its base, with and without
# PR_TITLE, the pull request's title the merge queue squashes under.
#
# Usage: ./commit-type-matches-diff_test.sh

set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/commit-type-matches-diff.sh"
FAILURES=0
ASSERTIONS=0

ok()   { ASSERTIONS=$((ASSERTIONS + 1)); echo "ok: $1"; }
fail() { FAILURES=$((FAILURES + 1));     echo "FAIL: $1"; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

g() { git -c user.name=t -c user.email=t@example.com -c commit.gpgsign=false -c core.hooksPath=/dev/null "$@"; }

# repo NAME: a fresh repository with one commit on main, holding a README and
# one Go file, and a branch named work checked out from it.
repo() {
	local dir="$WORK/$1"
	mkdir -p "$dir/pkg" && cd "$dir" || exit 2
	g init -q -b main
	echo "readme" >README.md
	printf 'package pkg\n\n// Answer is the answer.\nfunc Answer() int { return 42 }\n' >pkg/a.go
	g add . && g commit -q -m "chore: start"
	g checkout -q -b work
}

# commit SUBJECT FILE TEXT: writes TEXT to FILE and commits it as SUBJECT.
commit() {
	mkdir -p "$(dirname "$2")"
	printf '%s' "$3" >"$2"
	g add . && g commit -q -m "$1"
}

# expect NAME WANT [TITLE]: runs the script against main, with PR_TITLE set to
# TITLE when one is given, and checks its exit status; on a failure WANT, the
# stderr must also name pkg/a.go.
expect() {
	local name=$1 want=$2 got err
	if [ $# -ge 3 ]; then
		err=$(PR_TITLE="$3" bash "$SCRIPT" main 2>&1 >/dev/null); got=$?
	else
		err=$(env -u PR_TITLE bash "$SCRIPT" main 2>&1 >/dev/null); got=$?
	fi
	if [ "$got" -ne "$want" ]; then
		fail "$name: exit $got, want $want; stderr: $err"
		return
	fi
	if [ "$want" -eq 1 ] && [[ "$err" != *"pkg/a.go"* ]]; then
		fail "$name: stderr does not name pkg/a.go: $err"
		return
	fi
	ok "$name"
}

# Commits typed for what they change, one of them code: each passes on its own,
# and only a title that claims docs: over the whole branch fails.
repo typed
commit "docs: explain the answer" docs/answer.md $'the answer\n'
commit "test: change the answer" pkg/a.go $'package pkg\n\n// Answer is the answer.\nfunc Answer() int { return 43 }\n'
expect "typed commits, no title" 0
expect "typed commits under a docs: title" 1 "docs: rewrite the answer"
expect "typed commits under a test: title" 0 "test: rewrite the answer"
expect "typed commits under a scoped docs title" 1 "docs(answer): rewrite it"
expect "typed commits under a title with no type" 0 "Rewrite the answer"

# A docs: title over a change to a Go comment alone is a documentation change.
repo comment
commit "docs: fix a typo" pkg/a.go $'package pkg\n\n// Answer is the one answer.\nfunc Answer() int { return 42 }\n'
expect "a comment-only Go change under a docs: title" 0 "docs: fix a typo"

# The per-commit check still holds: a docs: commit carrying code fails with no
# title at all.
repo mistyped
commit "docs: change the answer" pkg/a.go $'package pkg\n\n// Answer is the answer.\nfunc Answer() int { return 44 }\n'
expect "a docs: commit carrying code, no title" 1

echo
echo "$ASSERTIONS passed, $FAILURES failed"
[ "$FAILURES" -eq 0 ]
