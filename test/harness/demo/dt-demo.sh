#!/usr/bin/env bash
# dt-demo.sh: the dt demo, ten steps played by player.sh so nobody types during the talk.
#
# It shows what runs on this machine, how a diff picks the cells, a real cell on a laptop, a
# regression, a known failure, a stale known entry, one test's history, a Linux-only cell run in dtc
# (--in, --parallel), and a dedicated host's saved run. Nothing leaves the machine: no push, no
# posting, no ssh, SLACK_DIR=/nonexistent. Every edit happens in a throwaway worktree at DEMO_REF and
# is reverted on exit. Written for macOS: steps 3 to 6 mount with its SMB client. README.md says what
# each step shows and what it needs.
#
#   dt-demo.sh setup     before the talk: the worktree, warm builds and dtc, clean results, checks
#   dt-demo.sh           the demo, live; a step that breaks can show its saved output instead
#   dt-demo.sh replay    the demo from saved outputs only: no build, no mount, no network
#   dt-demo.sh record    run every step unattended and save its outputs for replay and the f key
#   dt-demo.sh reset     revert the demo edits, unmount a leftover mount, stop a leftover dfs
#   dt-demo.sh cleanup   reset, then remove the worktree
#
# Keys between steps: Enter next step, r run it again, f show its saved output, b back, 1-9 jump, q quit.
# Settings: DEMO_REF (default f02e02620, the commit the recordings were made at), DEMO_WT (default
# dittofs-demo-dt beside this checkout), and the player's DEMO_TYPE_SPEED, DEMO_TIMEOUT, DEMO_HINTS=1
# and DEMO_MASK=0.

set -u

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(git -C "$DEMO_DIR" rev-parse --show-toplevel)" || exit 1
REF="${DEMO_REF:-f02e0262025814b7886755d4622c1f0629068112}"
export DEMO_DIR DEMO_STEPS=10 DEMO_COUNTED=9 DEMO_WT="${DEMO_WT:-${REPO%/*}/dittofs-demo-dt}"
export SLACK_DIR=/nonexistent
DT=test/harness/bin/dt

# shellcheck source=test/harness/demo/player.sh
. "$DEMO_DIR/player.sh"

LAN_IP=$(ifconfig 2>/dev/null | awk '$1 == "inet" && $2 != "127.0.0.1" && $2 !~ /^169\.254\./ {print $2; exit}')

# ---- the steps -------------------------------------------------------------------------------------

step1() {
  title "Step 1: dt lists what runs here" "Show: the pr tier's suites with their format and needs, and whether this Mac can run each one"
  look '^unit +pr .* yes$' "HERE says yes for lint, unit and bench-smoke"
  look 'no: linux root' "every no has its reason, e.g. pjdfstest: \"no: linux root\""
  look '^GROUP +SUITES' "four groups at the bottom"
  cmd 30 <<'EOF'
test/harness/bin/dt list --tier pr
EOF
}

step2() {
  title "Step 2: the diff decides what runs" "Show: three throwaway one-line commits, the pr plan for each, then back where it started"
  look '^docs change +0 cells' "docs change: 0 cells"
  look '^block store change +[0-9]+ cells' "block store change: the store's cells"
  look '^SMB adapter change .*e2e/' "SMB adapter change: adds the e2e cells"
  look '^worktree back at' "back where it started"
  cmd 60 <<EOF
"$DEMO_DIR/plan-compare.sh"
EOF
}

step3() {
  title "Step 3: a real cell runs on a laptop" "Show: dt builds dfs from this checkout, starts it, and mounts it with the Mac's own SMB client"
  look 'SMB 3\.1\.1 mounted' "SMB 3.1.1 mounted"
  look '\{password\}@' "{password} in the mount line, never the secret"
  look 'PASS: xattr' "files.sh and xattr.sh pass"
  look 'dt exit: 0' "passed, exit 0"
  cmd <<'EOF'
test/harness/bin/dt run clients/macos; echo "dt exit: $?"
EOF
}

step4() {
  title "Step 4: a regression fails the run" "Show: one broken expectation in a client test, named in the output and in the report"
  look 'FAIL: file.txt reads back other content' "FAIL: file.txt reads back other content"
  look 'dt exit: 1' "failed, exit 1"
  look '^clients/macos +test/clients/files\.sh ' "one NEW FAILURE row: files.sh"
  cmd 10 <<'EOF'
sed -i '' 's/uname -s)" ]/uname -s)!" ]/' test/clients/files.sh && git --no-pager diff -U0 test/clients/files.sh | grep '^[-+]\['
EOF
  cmd <<'EOF'
test/harness/bin/dt run clients/macos; echo "dt exit: $?"
EOF
  cmd 30 <<'EOF'
test/harness/bin/dt report --failures
EOF
}

step5() {
  title "Step 5: a known failure passes" "Show: the same failure declared known, and kept on record with its reason"
  look 'known: test/clients/known-demo.yaml' "one registry line"
  look 'FAIL: file.txt reads back other content' "the FAIL line still prints"
  look 'dt exit: 0' "passed, exit 0"
  look '"grade":"known"' "CTRF: files.sh skipped, grade known, with its reason"
  cmd 10 <<EOF
cp "$DEMO_DIR/known-demo.yaml" test/clients/ && cat test/clients/known-demo.yaml
EOF
  cmd 10 <<'EOF'
perl -0pi -e 's|(      macos:\n(?:        .*\n)*?        tier: nightly\n)|$1        known: test/clients/known-demo.yaml\n|' test/suites.yaml && git --no-pager diff -U1 test/suites.yaml | tail -n 3
EOF
  cmd <<'EOF'
test/harness/bin/dt run clients/macos; echo "dt exit: $?"
EOF
  cmd 10 <<'EOF'
jq -c '.results.tests[] | {name, status, extra}' "$(ls -d test/harness/results/*/ | tail -1)summary.json"
EOF
}

step6() {
  title "Step 6: a stale known entry fails" "Show: the fix lands, but nobody removes the known entry"
  look 'passed unexpectedly: test/clients/files.sh' "1 passed unexpectedly: test/clients/files.sh"
  look 'dt exit: 1' "failed, exit 1"
  cmd <<'EOF'
git checkout -q test/clients/files.sh && test/harness/bin/dt run clients/macos; echo "dt exit: $?"
EOF
}

step7() {
  title "Step 7: one test's history" "Show: the runs just made, for one test, and the demo edits reverted"
  look 'worktree clean' "git status prints nothing: a clean tree"
  look 'failed \(new\)' "failed (new)"
  look 'failed \(known\)' "failed (known)"
  look 'unexpected_pass' "passed (unexpected_pass)"
  cmd 10 <<'EOF'
git checkout -q test/suites.yaml && rm -f test/clients/known-demo.yaml && git status --short && echo "worktree clean"
EOF
  cmd 30 <<'EOF'
test/harness/bin/dt history clients/test/clients/files.sh
EOF
}

step8() {
  title "Step 8: a cell that can't run here runs in dtc" "Show: not run on the Mac; dtc, the Linux container; --in auto moves the run there; --parallel 3 runs 3 cells at once"
  look 'not run' "--in host: not run, dt mounts NFS only on Linux"
  look '^::error' "the ::error annotation"
  look 'dt exit: 2' "exit 2, in under a second"
  look '^Linux [0-9]' "dtc shell: Linux, as root"
  look '^\[dt\] in dtc: dt run pjdfstest/badger-s3/4\.1 chmod/00\.t --in=host' "--in auto: [dt] in dtc, the same run with --in=host"
  look '^\[pjdfstest/badger-s3/4\] \[dt\] fixture: dfs started' "--parallel 3: each line led by its cell's name"
  look '^pjdfstest/badger-s3/3 +passed' "NFSv3, v4 and v4.1 passed, each in about 13 s"
  look 'dt exit: 0 in [0-9]+ s' "exit 0, about 18 s for one cell, and for all three at once"
  cmd 60 <<'EOF'
test/harness/bin/dt run pjdfstest/badger-s3/4.1 --in host --ci; echo "dt exit: $?"
EOF
  cmd 30 <<'EOF'
test/harness/bin/dtc shell -c 'uname -sr; id -un'
EOF
  cmd 120 <<'EOF'
test/harness/bin/dt run pjdfstest/badger-s3/4.1 chmod/00.t --in auto | grep -Ev '(^|] )ok [0-9]+$'; echo "dt exit: ${PIPESTATUS[0]} in ${SECONDS} s"
EOF
  cmd 120 <<'EOF'
test/harness/bin/dt run pjdfstest chmod/00.t --in container --parallel 3 | grep -Ev '(^|] )ok [0-9]+$'; echo "dt exit: ${PIPESTATUS[0]} in ${SECONDS} s"
EOF
}

step9() {
  local run="$DEMO_DIR/nightly-run"
  title "Step 9: the dedicated host grades the same commit" "Show: what the plan sends to the dedicated host, and its saved merge run on 419a84010"
  look 'dedicated' "the dedicated cells, each limit the sum of its tests' limits"
  look 'system .*passed' "system passed"
  look '"known":13' "26 passed, 13 known"
  cmd 30 <<'EOF'
test/harness/bin/dt plan --tier nightly | awk 'NR==1 || $2=="dedicated"'
EOF
  cmd 30 <<EOF
test/harness/bin/dt report "$run"
EOF
  cmd 10 <<EOF
jq -c '[.results.tests[] | .extra.grade // .status] | group_by(.) | map({(.[0]): length}) | add' "$run/summary.json"
EOF
}

step10() {
  title "You can start with dt doctor" "Which needs this machine meets, and how to meet the rest"
  cmd 60 <<'EOF'
test/harness/bin/dt doctor
EOF
}

# ---- the hooks -------------------------------------------------------------------------------------

demo_undo() { # STEP: undo the step's own edit, so r applies it once again
  case "$1" in
    4) (cd "$DEMO_WT" && git checkout -q test/clients/files.sh) ;;
    5) (cd "$DEMO_WT" && git checkout -q test/suites.yaml && rm -f test/clients/known-demo.yaml) ;;
  esac
}

demo_reset() { # [quiet]
  [ -d "$DEMO_WT" ] || return 0
  (cd "$DEMO_WT" && git checkout -q -- test/clients/files.sh test/suites.yaml 2>/dev/null; rm -f test/clients/known-demo.yaml)
  local m
  # Only the mounts this worktree's runs made, as their logs name them: a dt run from another
  # checkout mounts under /tmp/dtNNN too.
  for m in $(grep -rhoE 'mounted at /(private/)?tmp/dt[0-9]+' "$DEMO_WT/test/harness/results" 2>/dev/null | awk '{print $3}' | sort -u); do
    umount "$m" 2>/dev/null
  done
  pkill -f "$DEMO_WT/test/harness/results/.*/bin/dfs" 2>/dev/null
  # A step 8 run cut short can leave its dtc container: only this worktree's, found by its mount.
  local c
  c=$(perl -e 'alarm 5; exec @ARGV' docker ps -q --filter name=^dtc- --filter "volume=$DEMO_WT" 2>/dev/null)
  # shellcheck disable=SC2086 # one container id per word
  [ -z "$c" ] || docker rm -f $c >/dev/null 2>&1
  [ "${1:-}" = quiet ] || { ok "demo edits reverted"; git -C "$DEMO_WT" status --short; }
}

demo_after_record() {
  rm -rf "$DEMO_WT/test/harness/results"
  ok "results cleared, so step 7's history counts the live run's alone"
}

demo_cleanup() {
  [ -d "$DEMO_WT" ] || return 0
  git -C "$REPO" worktree remove --force "$DEMO_WT" && ok "worktree removed"
}

demo_setup() { # [--quick]
  strong "dt demo setup (worktree $DEMO_WT at ${REF:0:9})"
  local free t0 t1 rc
  free=$(df -g / | awk 'NR==2 {print $4}')
  [ "${free:-0}" -ge 30 ] && ok "$free GB free on /" || warn "only ${free} GB free on /: each clients/macos run writes a dfs binary"
  t0=$(date +%s); docker info >/dev/null 2>&1; rc=$?; t1=$(date +%s)
  if [ "$rc" != 0 ]; then warn "Docker is off: start it, then setup again; step 8 runs in dtc"
  elif [ $((t1 - t0)) -le 3 ]; then ok "docker answers ($((t1 - t0)) s)"
  else warn "docker info took $((t1 - t0)) s: restart Docker Desktop; steps 1 and 10 wait up to 30 s on it"; fi
  [ -n "$LAN_IP" ] && ok "an IPv4 LAN address is up (masked on screen)" || warn "no LAN address: steps 3 to 6 end not run; use f for their saved outputs"
  [ "$(tput cols 2>/dev/null || echo 0)" -ge 120 ] && ok "terminal is $(tput cols) columns" || warn "terminal is $(tput cols 2>/dev/null) columns: make it at least 120 at the font you share"
  if [ -d "$DEMO_WT" ]; then
    [ -z "$(git -C "$DEMO_WT" status --short)" ] || demo_reset quiet
    [ -z "$(git -C "$DEMO_WT" status --short)" ] && ok "worktree clean" || { bad "worktree has changes: $(git -C "$DEMO_WT" status --short | head -3)"; exit 1; }
  else
    git -C "$REPO" cat-file -e "$REF^{commit}" 2>/dev/null || git -C "$REPO" fetch -q origin "$REF" || { bad "no commit $REF"; exit 1; }
    git -C "$REPO" worktree add -q --detach "$DEMO_WT" "$REF" || exit 1
    touch "$(git -C "$DEMO_WT" rev-parse --absolute-git-dir)/dt-demo" && ok "worktree added" || exit 1
  fi
  local at want
  at=$(git -C "$DEMO_WT" rev-parse HEAD) want=$(git -C "$REPO" rev-parse "$REF^{commit}")
  [ "$at" = "$want" ] && ok "worktree at $(git -C "$DEMO_WT" log -1 --format='%h %s' | cut -c1-80)" || warn "worktree at ${at:0:9}, not ${REF:0:9}: cleanup, then setup again"
  cd "$DEMO_WT" || exit 1
  t0=$(date +%s); $DT --help >/dev/null 2>&1 || { bad "dt does not build"; exit 1; }; t1=$(date +%s)
  ok "dt builds ($((t1 - t0)) s)"
  if [ "${1:-}" != --quick ]; then
    say "  warming up the one real cell (builds dfs and dfsctl, mounts once)..."
    t0=$(date +%s)
    if $DT run clients/macos >/dev/null 2>&1; then t1=$(date +%s); ok "clients/macos passes ($((t1 - t0)) s)"; else warn "clients/macos did not pass: check the network, or plan to use f in steps 3 to 6"; fi
    say "  warming up dtc for step 8 (its image, Localstack, one pjdfstest cell in the container)..."
    t0=$(date +%s)
    if test/harness/bin/dtc build >/dev/null 2>&1 && $DT services up localstack >/dev/null 2>&1 &&
      $DT run pjdfstest/badger-s3/4.1 chmod/00.t --in auto >/dev/null 2>&1; then
      t1=$(date +%s); ok "pjdfstest/badger-s3/4.1 passes in dtc ($((t1 - t0)) s)"
    else warn "pjdfstest did not pass in dtc: check Docker and Localstack, or plan to use f in step 8"; fi
  fi
  docker image inspect dittofs-dtc >/dev/null 2>&1 && ok "the dtc image is built" || warn "no dtc image: run setup without --quick (test/harness/bin/dtc build, about 1 min)"
  curl -s -m 3 http://127.0.0.1:4566/_localstack/health | grep -Eq '"s3": "(available|running)"' &&
    ok "Localstack answers: step 8's --in auto moves into dtc" ||
    warn "Localstack is down: step 8's --in auto stays on the Mac, not run (test/harness/bin/dt services up localstack)"
  grep -q '^      macos:' test/suites.yaml && ok "step 5's edit target is there" || warn "no '      macos:' block in test/suites.yaml: step 5's perl line will not match"
  rm -rf test/harness/results
  ok "results cleared: step 7's history counts runs from here"
  [ -n "$(ls "$DEMO_SAVED"/step*-*.txt 2>/dev/null)" ] && ok "saved outputs in $DEMO_SAVED" || warn "no saved outputs yet: run $(demo_name) record once, then setup again"
  strong "ready. Next: $(demo_name) (DEMO_HINTS=1 shows each step's 'look for' first)"
}

# The steps commit, reset and check out files in DEMO_WT, so the demo runs only in the detached
# linked worktree setup made, which it marks, never in a checkout that may hold work.
case "${1:-}" in
  help | -h | --help | replay) ;;
  *)
    if [ -d "$DEMO_WT" ]; then
      dirs=$(git -C "$DEMO_WT" rev-parse --path-format=absolute --git-dir --git-common-dir 2>/dev/null | sort -u | wc -l)
      if [ "$dirs" -ne 2 ] || git -C "$DEMO_WT" symbolic-ref -q HEAD >/dev/null; then
        bad "DEMO_WT=$DEMO_WT is not a detached worktree of its own, and the demo resets it: refusing"
        exit 1
      fi
      if [ ! -f "$(git -C "$DEMO_WT" rev-parse --absolute-git-dir)/dt-demo" ]; then
        bad "DEMO_WT=$DEMO_WT has no mark from $(demo_name) setup, and the demo resets it: refusing (an older setup's worktree: git worktree remove it, then setup)"
        exit 1
      fi
    fi
    ;;
esac

demo_main "$@"
