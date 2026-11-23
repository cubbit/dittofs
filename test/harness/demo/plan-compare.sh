#!/usr/bin/env bash
# plan-compare.sh: step 2 of dt-demo.sh. One throwaway local commit per kind of change, the pr plan
# for each, then the worktree is reset to where it started.
set -u
cd "${DEMO_WT:?run it from dt-demo.sh, which sets DEMO_WT}" || exit 9
# It commits and resets --hard, so only on the demo's detached HEAD, never on a branch.
if git symbolic-ref -q HEAD >/dev/null; then
  echo "plan-compare.sh: HEAD is on a branch in $DEMO_WT; it runs only on the demo's detached worktree" >&2
  exit 9
fi
base=$(git rev-parse HEAD)
commit() { git -c core.hooksPath=/dev/null -c commit.gpgsign=false commit -qam "demo: $1"; }
show() {
  local plan n e2e
  plan=$(test/harness/bin/dt plan --tier pr --base HEAD~1) || { printf '%-22s dt plan failed\n' "$1"; return 1; }
  n=$(printf '%s\n' "$plan" | tail -n +2 | grep -c .)
  e2e=$(printf '%s\n' "$plan" | awk '/^e2e/{print $1}' | paste -sd' ' -)
  printf '%-22s %2s cells; e2e cells: %s\n' "$1" "$n" "${e2e:-none}"
}
try() {
  echo '// demo: a one-line change' >>"$2" && commit "$1" && show "$1"
  git reset -q --hard "$base"
}
try "docs change" docs/internals/testing.md
try "block store change" pkg/block/engine/engine.go
try "SMB adapter change" internal/adapter/smb/channel_sequence.go
git log -1 --format='worktree back at %h'
git status --short
