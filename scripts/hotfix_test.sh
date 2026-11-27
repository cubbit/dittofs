#!/usr/bin/env bash
# Tests for hotfix.sh's checks: each case builds a throwaway repository whose
# origin refs are set by hand, sources hotfix.sh and runs hotfix_check, which
# reads local refs alone, so no remote is needed.
#
# Usage: ./hotfix_test.sh

set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/hotfix.sh"
FAILURES=0
ASSERTIONS=0

ok()   { ASSERTIONS=$((ASSERTIONS + 1)); echo "ok: $1"; }
fail() { FAILURES=$((FAILURES + 1));     echo "FAIL: $1"; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

g() { git -c user.name=t -c user.email=t@example.com -c commit.gpgsign=false -c tag.gpgsign=false -c core.hooksPath=/dev/null "$@"; }

# repo NAME: main at v0.37.0 (an rc tag v0.38.0-rc1 above it), origin/main
# there, and hotfix/0.37.1 cut from it with a fix and its changelog section,
# pushed: origin/hotfix/0.37.1 at its head.
repo() {
	local dir="$WORK/$1"
	mkdir -p "$dir" && cd "$dir" || exit 2
	g init -q -b main
	printf '## [0.37.0]\n' >CHANGELOG.md
	g add . && g commit -q -m "chore(release): v0.37.0" && g tag v0.37.0
	g commit -q --allow-empty -m "feat: next" && g tag v0.38.0-rc1
	g update-ref refs/remotes/origin/main HEAD
	g checkout -q -b hotfix/0.37.1 v0.37.0
	g merge -q --ff-only main
	printf '## [0.37.1]\n\n## [0.37.0]\n' >CHANGELOG.md
	g commit -q -am "fix: the fix"
	g update-ref refs/remotes/origin/hotfix/0.37.1 HEAD
}

# expect NAME WANT_EXIT VERSION [MESSAGE]: runs hotfix_check VERSION in the
# current repository and checks its exit status, and that its stderr holds
# MESSAGE when given.
expect() {
	local name="$1" want="$2" version="$3" msg="${4:-}" out got
	out=$(bash -c 'source "$1" && hotfix_check "$2"' bash "$SCRIPT" "$version" 2>&1)
	got=$?
	if [ "$got" -ne "$want" ]; then
		fail "$name: exit $got, want $want: $out"
		return
	fi
	if [ -n "$msg" ] && [[ "$out" != *"$msg"* ]]; then
		fail "$name: output $out, want it to contain $msg"
		return
	fi
	ok "$name"
}

repo good
expect "the next patch of main's latest tag" 0 0.37.1
expect "an rc version" 1 0.37.1-rc1 "a hotfix has no rc"

repo skipped
g checkout -q -b hotfix/0.37.2
g update-ref refs/remotes/origin/hotfix/0.37.2 HEAD
expect "a patch that skips one" 1 0.37.2 "want 0.37.1"

repo minor
g checkout -q -b hotfix/0.38.0
g update-ref refs/remotes/origin/hotfix/0.38.0 HEAD
expect "a minor bump" 1 0.38.0 "want 0.37.1"

repo wrongbranch
g checkout -q -b fix/other
expect "another branch" 1 0.37.1 "not on hotfix/0.37.1"

repo dirty
echo x >>CHANGELOG.md
expect "uncommitted changes" 1 0.37.1 "uncommitted changes"

repo unsynced
g commit -q --allow-empty -m "fix: more"
expect "ahead of origin" 1 0.37.1 "not in sync"

repo unpushed
g update-ref -d refs/remotes/origin/hotfix/0.37.1
expect "not on origin" 1 0.37.1 "push it first"

repo nomain
g checkout -q main && g commit -q --allow-empty -m "fix: on main" && g checkout -q hotfix/0.37.1
g update-ref refs/remotes/origin/main main
expect "main moved past the branch" 1 0.37.1 "does not contain origin/main"

repo tagged
g tag v0.37.1
expect "the tag exists" 1 0.37.1 "already exists"

repo nochangelog
printf '## [0.37.0]\n' >CHANGELOG.md
g commit -q -am "docs: drop the section"
g update-ref refs/remotes/origin/hotfix/0.37.1 HEAD
expect "no changelog section" 1 0.37.1 "no '## [0.37.1]' section"

echo
echo "$ASSERTIONS passed, $FAILURES failed"
[ "$FAILURES" -eq 0 ]
