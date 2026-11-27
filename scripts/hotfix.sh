#!/usr/bin/env bash
# Cut a hotfix of a released line: bump flake.nix on hotfix/X.Y.Z, fast-forward
# main to it, tag it, then merge main back into develop.
#
# Usage: scripts/hotfix.sh [--ours] X.Y.Z
#
# Run it on hotfix/X.Y.Z, a branch cut from main's latest tag that holds the fix
# and a "## [X.Y.Z]" section in CHANGELOG.md, pushed to origin. Refuses unless
# the tree is clean and in sync with origin/hotfix/X.Y.Z, the branch contains
# origin/main, X.Y.Z is the next patch of main's latest tag, the tag is new and
# the changelog section exists. Everything up to the push is local; the push
# asks first.
#
# The back-merge keeps main an ancestor of develop, which release.sh needs. A
# v1 fix rarely applies to develop's code, so --ours records the merge with
# `-s ours`, leaving develop's tree as it is. Without it the merge is a normal
# one, and on a conflict the script stops and prints the commands that finish it.
#
# Notes:
# - The pushes to main and develop go through the repository admin's ruleset
#   bypass, as release.sh's do: neither branch takes a direct push otherwise.
# - SSH signing failing with "Couldn't get agent socket": pass the key through
#   git's env config, as release.sh's notes in CLAUDE.md show.
set -euo pipefail

die() { echo "hotfix: $*" >&2; exit 1; }

# next_patch vA.B.C prints A.B.(C+1).
next_patch() {
  local v="${1#v}"
  [[ "$v" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] || return 1
  echo "${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.$((BASH_REMATCH[3] + 1))"
}

# hotfix_check VERSION refuses, through die, unless the checkout may cut the
# hotfix VERSION. It reads the local refs alone, so fetch first.
hotfix_check() {
  local version="$1" branch latest want
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "usage: $0 [--ours] X.Y.Z (a hotfix has no rc)"
  branch="hotfix/$version"

  [[ "$(git branch --show-current)" == "$branch" ]] || die "not on $branch"
  [[ -z "$(git status --porcelain --untracked-files=no)" ]] || die "tracked files have uncommitted changes"
  git rev-parse -q --verify "refs/remotes/origin/$branch" >/dev/null || die "$branch is not on origin; push it first"
  [[ "$(git rev-parse HEAD)" == "$(git rev-parse "origin/$branch")" ]] || die "$branch is not in sync with origin/$branch"
  git merge-base --is-ancestor origin/main HEAD || die "$branch does not contain origin/main; rebase it onto origin/main"

  latest="$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' origin/main 2>/dev/null)" ||
    die "main has no release tag"
  want="$(next_patch "$latest")" || die "main's latest tag $latest is not vX.Y.Z"
  [[ "$version" == "$want" ]] || die "$version is not the next patch of main's latest tag $latest; want $want"
  ! git rev-parse -q --verify "refs/tags/v$version" >/dev/null || die "tag v$version already exists"

  grep -q "^## \[$version\]" CHANGELOG.md ||
    die "CHANGELOG.md has no '## [$version]' section; add it on $branch first"
}

main() {
  local ours=""
  if [[ "${1:-}" == --ours ]]; then ours=1; shift; fi
  local version="${1:-}"
  [[ -n "$version" ]] || die "usage: $0 [--ours] X.Y.Z"
  local tag="v$version" branch="hotfix/$version"

  cd "$(git rev-parse --show-toplevel)"
  git fetch -q origin main develop "$branch" --tags 2>/dev/null ||
    git fetch -q origin main develop --tags
  hotfix_check "$version"

  # CI on the commit being released. Not a gate, as in release.sh: it is listed
  # for the confirmation below to weigh.
  local not_green
  not_green="$(gh run list --commit "$(git rev-parse HEAD)" --limit 100 --json name,status,conclusion \
    --jq '.[] | select(.conclusion != "success" and .conclusion != "skipped") | "  \(.name): \(if .conclusion == "" then .status else .conclusion end)"' |
    sort -u)" || not_green="  (could not ask gh)"
  echo "CI on $(git rev-parse --short HEAD):"
  echo "${not_green:-  all green}"

  sed -i.bak -E "s/^( *version = \")[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?(\";)$/\1$version\3/" flake.nix
  rm flake.nix.bak
  [[ "$(git diff --numstat flake.nix | cut -f1)" == 1 ]] || { git checkout flake.nix; die "flake.nix version bump did not change exactly one line"; }

  git commit -S -q -m "chore(release): $tag" flake.nix
  git tag -s "$tag" -m "$tag"
  local sha
  sha="$(git rev-parse --short HEAD)"

  echo
  echo "Committed $sha and tagged $tag locally. Pushing will:"
  echo "  push $branch and fast-forward main to $sha, then push $tag (triggers release.yml)"
  echo "  then merge main into develop$([[ -n "$ours" ]] && echo " with -s ours, develop's tree unchanged") and push develop"
  read -r -p "Push? [y/N] " ok
  if [[ "$ok" != [yY] ]]; then
    echo "Not pushed. To undo: git tag -d $tag && git reset --hard origin/$branch"
    exit 0
  fi

  # main only fast-forwards: the branch contains it. The tag goes after, so
  # release.yml never starts on a tag main does not contain.
  git push --atomic --no-follow-tags origin "HEAD:$branch" HEAD:main ||
    die "push failed; nothing was published. Finish with: git push --atomic --no-follow-tags origin HEAD:$branch HEAD:main && git push origin $tag"
  git push origin "$tag" || die "main pushed, tag not; finish with: git push origin $tag"

  back_merge "$tag" "$ours"
}

# back_merge TAG OURS merges TAG into origin/develop on a detached HEAD and
# pushes it, so main stays an ancestor of develop.
back_merge() {
  local tag="$1" ours="$2" msg="Merge $1 into develop"
  local finish="git commit -S --no-edit && git push origin HEAD:develop && git switch hotfix/${tag#v}"
  git fetch -q origin develop
  git switch -q --detach origin/develop
  if [[ -n "$ours" ]]; then
    git merge -S -q -s ours -m "$msg" "$tag"
  elif ! git merge -S -q --no-ff -m "$msg" "$tag"; then
    echo
    echo "hotfix: the back-merge of $tag into develop stopped on a conflict." >&2
    echo "Resolve it, then: git add <files> && $finish" >&2
    echo "Or, if the fix does not apply to develop's code: git merge --abort && git merge -S -s ours -m '$msg' $tag && git push origin HEAD:develop" >&2
    exit 1
  fi
  git push origin HEAD:develop ||
    die "back-merge not pushed (develop moved?); redo it with: git fetch origin develop && git switch --detach origin/develop && git merge -S ${ours:+-s ours }-m '$msg' $tag && git push origin HEAD:develop"
  echo "Released $tag; main is at $tag and merged into develop."
  git switch -q "hotfix/${tag#v}"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
