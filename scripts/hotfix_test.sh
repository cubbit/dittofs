#!/usr/bin/env bash
# Tests for hotfix.sh: each case builds a throwaway repository with a bare
# origin, sources hotfix.sh and runs hotfix_check, or the script itself for its
# arguments, or back_merge for the --ours back-merge.
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

# A signing key of its own, since back_merge signs its merge.
KEY=""
if command -v ssh-keygen >/dev/null 2>&1; then
	ssh-keygen -q -t ed25519 -N '' -f "$WORK/key" && KEY="$WORK/key"
fi

# repo NAME: origin holds main at v0.37.0 and develop one commit past it, with
# v0.38.0-rc1 there, and hotfix/0.37.1 cut from v0.37.0 with a fix and its
# changelog section. The checkout is on hotfix/0.37.1, fetched.
repo() {
	local dir="$WORK/$1"
	git init -q --bare "$dir.git"
	mkdir -p "$dir" && cd "$dir" || exit 2
	git init -q -b main
	git config user.name t
	git config user.email t@example.com
	git config commit.gpgsign false
	git config tag.gpgsign false
	git config core.hooksPath /dev/null
	if [ -n "$KEY" ]; then
		git config gpg.format ssh
		git config user.signingkey "$KEY"
	fi
	git remote add origin "$dir.git"
	printf '## [0.37.0]\n\nv1.\n' >CHANGELOG.md
	echo v1 >code
	git add . && git commit -q -m "chore(release): v0.37.0" && git tag -a -m v0.37.0 v0.37.0
	git checkout -q -b develop
	printf '## [0.38.0-rc1]\n\nv2.\n\n## [0.37.0]\n\nv1.\n' >CHANGELOG.md
	echo v2 >code
	git commit -q -am "feat: v2" && git tag -a -m v0.38.0-rc1 v0.38.0-rc1
	git checkout -q -b hotfix/0.37.1 v0.37.0
	printf '## [0.37.1]\n\nthe fix.\n\n## [0.37.0]\n\nv1.\n' >CHANGELOG.md
	git commit -q -am "fix: the fix"
	git push -q origin main develop hotfix/0.37.1 --tags 2>/dev/null
	git fetch -q origin
}

# check NAME WANT_EXIT VERSION [MESSAGE]: runs hotfix_check VERSION in the
# current repository and checks its exit status, and that its output holds
# MESSAGE when given.
check() {
	local name="$1" want="$2" version="$3" msg="${4:-}" out
	out=$(bash -c 'source "$1" && hotfix_check "$2"' bash "$SCRIPT" "$version" 2>&1)
	judge "$name" "$?" "$want" "$out" "$msg"
}

# run NAME WANT_EXIT MESSAGE ARGS...: runs the script with ARGS.
run() {
	local name="$1" want="$2" msg="$3" out
	shift 3
	out=$(bash "$SCRIPT" "$@" 2>&1 </dev/null)
	judge "$name" "$?" "$want" "$out" "$msg"
}

judge() {
	local name="$1" got="$2" want="$3" out="$4" msg="$5"
	if [ "$got" -ne "$want" ]; then
		fail "$name: exit $got, want $want: $out"
	elif [ -n "$msg" ] && [[ "$out" != *"$msg"* ]]; then
		fail "$name: output $out, want it to contain $msg"
	else
		ok "$name"
	fi
}

# An rc tag on develop, not on main, leaves main's final tag the one to patch.
repo good
check "the next patch of main's final tag, an rc on develop" 0 0.37.1
check "an rc version" 1 0.37.1-rc1 "a hotfix has no rc"

repo rcmain
git push -q origin develop:main && git fetch -q origin
git reset -q --hard origin/main
printf '## [0.37.1]\n\n## [0.38.0-rc1]\n' >CHANGELOG.md
git commit -q -am "fix: on the rc" && git push -q -f origin hotfix/0.37.1 && git fetch -q origin
check "main moved to an rc" 1 0.37.1 "origin/main is not at v0.37.0"

repo outoforder
git tag -a -m v0.38.0 v0.38.0 develop && git push -q origin v0.38.0
check "a higher final tag main does not hold" 1 0.37.1 "origin/main is not at v0.38.0"

repo skipped
git checkout -q -b hotfix/0.37.2 && git push -q origin hotfix/0.37.2 && git fetch -q origin
check "a patch that skips one" 1 0.37.2 "want 0.37.1"

repo minor
git checkout -q -b hotfix/0.38.0 && git push -q origin hotfix/0.38.0 && git fetch -q origin
check "a minor bump" 1 0.38.0 "want 0.37.1"

repo wrongbranch
git checkout -q -b fix/other
check "another branch" 1 0.37.1 "not on hotfix/0.37.1"

repo dirty
echo x >>CHANGELOG.md
check "uncommitted changes" 1 0.37.1 "uncommitted changes"

repo unsynced
git commit -q --allow-empty -m "fix: more"
check "ahead of origin" 1 0.37.1 "not in sync"

repo unpushed
git push -q origin --delete hotfix/0.37.1 && git fetch -q --prune origin
check "not on origin" 1 0.37.1 "push it first"

repo nomain
git checkout -q main && git commit -q --allow-empty -m "fix: on main" && git push -q origin main && git checkout -q hotfix/0.37.1
git fetch -q origin
check "main moved past the branch" 1 0.37.1 "does not contain origin/main"

repo taggedorigin
git tag v0.37.1 && git push -q origin v0.37.1
check "the tag is on origin" 1 0.37.1 "already exists on origin"

# A rerun after the tag push failed names the tag, not the next version.
repo taggedlocal
git tag v0.37.1
check "the tag is local only" 1 0.37.1 "exists here but not on origin"

repo nochangelog
printf '## [0.37.0]\n\nv1.\n' >CHANGELOG.md
git commit -q -am "docs: drop the section" && git push -q origin hotfix/0.37.1 && git fetch -q origin
check "no changelog section" 1 0.37.1 "no '## [0.37.1]' section"

repo args
git checkout -q -b fix/none
run "an unknown option" 1 "unknown option --force" --force 0.37.1
run "two versions" 1 "one version only" 0.37.1 0.37.2
run "no version" 1 "usage:" --ours
run "--ours after the version" 1 "not on hotfix/0.37.1" 0.37.1 --ours
run "--ours before the version" 1 "not on hotfix/0.37.1" --ours 0.37.1

# --ours keeps develop's tree but gives its CHANGELOG.md the hotfix's section.
if [ -z "$KEY" ]; then
	echo "skip: the --ours back-merge, since ssh-keygen isn't on the PATH to sign it"
else
	repo backmerge
	git tag -a -m v0.37.1 v0.37.1 && git push -q origin HEAD:main v0.37.1
	out=$(bash -c 'source "$1" && back_merge v0.37.1 1 0.37.0' bash "$SCRIPT" 2>&1)
	got=$?
	git fetch -q origin
	want=$'## [0.38.0-rc1]\n\nv2.\n\n## [0.37.1]\n\nthe fix.\n\n## [0.37.0]\n\nv1.'
	if [ "$got" -ne 0 ]; then
		fail "--ours back-merge: exit $got: $out"
	elif [ "$(git show origin/develop:CHANGELOG.md)" != "$want" ]; then
		fail "--ours back-merge: develop's CHANGELOG.md is $(git show origin/develop:CHANGELOG.md), want $want"
	elif [ "$(git show origin/develop:code)" != v2 ]; then
		fail "--ours back-merge: develop's code is $(git show origin/develop:code), want v2"
	elif ! git merge-base --is-ancestor v0.37.1 origin/develop; then
		fail "--ours back-merge: v0.37.1 is not an ancestor of develop"
	elif ! git cat-file commit origin/develop | grep -q '^gpgsig'; then
		fail "--ours back-merge: the merge is not signed"
	else
		ok "--ours back-merge"
	fi
fi

echo
echo "$ASSERTIONS passed, $FAILURES failed"
[ "$FAILURES" -eq 0 ]
