#!/usr/bin/env bash
# Cut a hotfix of the newest release: bump flake.nix on hotfix/X.Y.Z,
# fast-forward main to it, tag it, then merge main back into develop.
#
# Usage: scripts/hotfix.sh [--ours] X.Y.Z
#
# A hotfix patches the release main holds, which is the newest final release:
# X.Y.Z must be the next patch of the highest final tag on origin, and main
# must sit exactly on that tag. An older line (v0.37 once a later release is
# out) is not supported here; it would need a maintenance branch, release/X.Y.
#
# Run it on hotfix/X.Y.Z, a branch cut from that tag that holds the fix and a
# "## [X.Y.Z]" section in CHANGELOG.md, pushed to origin. Refuses unless the tree
# is clean and in sync with origin/hotfix/X.Y.Z, the tag is new locally and on
# origin, the branch contains origin/main, X.Y.Z is that next patch and the
# changelog section exists. Everything up to the push is local; the push asks
# first.
#
# The back-merge keeps main an ancestor of develop, which release.sh needs. A
# v1 fix rarely applies to develop's code, so --ours records the merge with
# `-s ours`, leaving develop's tree as it is apart from CHANGELOG.md, which gains
# the hotfix's section. Without it the merge is a normal one, and on a conflict
# the script stops and prints the commands that finish it.
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

# latest_final_tag prints the highest final (non-rc) version tag on origin.
latest_final_tag() {
  git ls-remote --tags --refs origin 'v*' | sed 's|.*refs/tags/||' |
    grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1
}

# hotfix_check VERSION refuses, through die, unless the checkout may cut the
# hotfix VERSION. It reads origin's tags, and origin/main and the branch as last
# fetched, so fetch first.
hotfix_check() {
  local version="$1" branch latest want
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "usage: $0 [--ours] X.Y.Z (a hotfix has no rc)"
  branch="hotfix/$version"

  [[ "$(git branch --show-current)" == "$branch" ]] || die "not on $branch"
  [[ -z "$(git status --porcelain --untracked-files=no)" ]] || die "tracked files have uncommitted changes"

  # Before the version check: a rerun after a failed tag push finds its own tag.
  if git ls-remote --exit-code --tags origin "refs/tags/v$version" >/dev/null; then
    die "tag v$version already exists on origin"
  elif git rev-parse -q --verify "refs/tags/v$version" >/dev/null; then
    die "tag v$version exists here but not on origin: a run stopped after tagging. Push it with: git push origin v$version, or drop it with: git tag -d v$version"
  fi

  git rev-parse -q --verify "refs/remotes/origin/$branch" >/dev/null || die "$branch is not on origin; push it first"
  [[ "$(git rev-parse HEAD)" == "$(git rev-parse "origin/$branch")" ]] || die "$branch is not in sync with origin/$branch"
  git merge-base --is-ancestor origin/main HEAD || die "$branch does not contain origin/main; rebase it onto origin/main"

  latest="$(latest_final_tag)"
  [[ -n "$latest" ]] || die "origin has no final release tag"
  git rev-parse -q --verify "refs/tags/$latest" >/dev/null || die "tag $latest is not fetched; run: git fetch origin --tags"
  [[ "$(git rev-parse origin/main)" == "$(git rev-parse "$latest^{commit}")" ]] ||
    die "origin/main is not at $latest, the newest final release, so it holds no release to patch; hotfix.sh patches only the release main holds"
  want="$(next_patch "$latest")"
  [[ "$version" == "$want" ]] || die "$version is not the next patch of $latest; want $want"

  grep -q "^## \[$version\]" CHANGELOG.md ||
    die "CHANGELOG.md has no '## [$version]' section; add it on $branch first"
}

# changelog_add VERSION PREV FROM writes FROM's "## [VERSION]" section of
# CHANGELOG.md into the working CHANGELOG.md, above its "## [PREV]" heading.
# It changes nothing when the section is already there.
changelog_add() {
  local version="$1" prev="$2" from="$3" section
  grep -q "^## \[$version\]" CHANGELOG.md && return 0
  grep -q "^## \[$prev\]" CHANGELOG.md || return 1
  section="$(git show "$from:CHANGELOG.md" | awk -v h="## [$version]" '
    index($0, h) == 1 { on = 1; print; next }
    on && /^## \[/ { exit }
    on { print }')"
  [[ -n "$section" ]] || return 1
  SECTION="$section" awk -v h="## [$prev]" '
    index($0, h) == 1 && !done { print ENVIRON["SECTION"]; print ""; done = 1 }
    { print }' CHANGELOG.md > CHANGELOG.md.new
  mv CHANGELOG.md.new CHANGELOG.md
}

main() {
  local ours="" version=""
  while (($#)); do
    case "$1" in
      --ours) ours=1 ;;
      -*) die "unknown option $1; usage: $0 [--ours] X.Y.Z" ;;
      *) [[ -z "$version" ]] || die "one version only; usage: $0 [--ours] X.Y.Z"; version="$1" ;;
    esac
    shift
  done
  [[ -n "$version" ]] || die "usage: $0 [--ours] X.Y.Z"
  local tag="v$version" branch="hotfix/$version"

  cd "$(git rev-parse --show-toplevel)"
  git fetch -q origin main develop "$branch" --tags 2>/dev/null ||
    git fetch -q origin main develop --tags
  hotfix_check "$version"
  local prev
  prev="$(latest_final_tag)"

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

  back_merge "$tag" "$ours" "${prev#v}"
}

# back_merge TAG OURS PREV merges TAG into origin/develop on a detached HEAD
# and pushes it, so main stays an ancestor of develop. With OURS the merge keeps
# develop's tree, and CHANGELOG.md gains TAG's section above PREV's.
back_merge() {
  local tag="$1" ours="$2" prev="$3" msg="Merge $1 into develop"
  local version="${tag#v}"
  local finish="git commit -S --no-edit && git push origin HEAD:develop && git switch hotfix/$version"
  git fetch -q origin develop
  git switch -q --detach origin/develop
  if [[ -n "$ours" ]]; then
    git merge -S -q -s ours -m "$msg" "$tag"
    if changelog_add "$version" "$prev" "$tag"; then
      if ! git diff --quiet CHANGELOG.md; then
        git commit -S -q --amend --no-edit CHANGELOG.md
      fi
    else
      echo "hotfix: develop's CHANGELOG.md has no '## [$prev]' heading to put the $version section above; add it by hand after this run" >&2
    fi
  elif ! git merge -S -q --no-ff -m "$msg" "$tag"; then
    echo
    echo "hotfix: the back-merge of $tag into develop stopped on a conflict." >&2
    echo "Resolve it, then: git add <files> && $finish" >&2
    echo "Or, if the fix does not apply to develop's code: git merge --abort && git merge -S -s ours -m '$msg' $tag," >&2
    echo "then copy the '## [$version]' section of CHANGELOG.md from $tag, and: git commit -S --amend --no-edit CHANGELOG.md && git push origin HEAD:develop" >&2
    exit 1
  fi
  git push origin HEAD:develop ||
    die "back-merge not pushed (develop moved?); redo it with: git fetch origin develop && git switch --detach origin/develop && git merge -S ${ours:+-s ours }-m '$msg' $tag && git push origin HEAD:develop"
  echo "Released $tag; main is at $tag and merged into develop."
  git switch -q "hotfix/$version"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
