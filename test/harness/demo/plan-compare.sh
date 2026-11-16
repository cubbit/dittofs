#!/usr/bin/env bash
# plan-compare.sh: step 2 of dt-demo.sh. One throwaway local commit per kind of change, the pr plan
# for each, then the worktree is reset to where it started.
set -u
cd "${DEMO_WT:?run it from dt-demo.sh, which sets DEMO_WT}" || exit 9
base=$(git rev-parse HEAD)
commit() { git -c core.hooksPath=/dev/null -c commit.gpgsign=false commit -qam "demo: $1"; }
show() {
  local n e2e
  n=$(test/harness/bin/dt plan --tier pr --base HEAD~1 | tail -n +2 | wc -l | tr -d ' ')
  e2e=$(test/harness/bin/dt plan --tier pr --base HEAD~1 | awk '/^e2e/{print $1}' | paste -sd' ' -)
  printf '%-22s %2s cells; e2e cells: %s\n' "$1" "$n" "${e2e:-none}"
}
try() {
  echo '// demo: a one-line change' >>"$2" && commit "$1" && show "$1"
  git reset -q --hard "$base"
}
try "docs change" docs/internals/testing.md
try "block store change" pkg/block/blockstore.go
try "SMB adapter change" internal/adapter/smb/channel_sequence.go
git log -1 --format='worktree back at %h'
git status --short
