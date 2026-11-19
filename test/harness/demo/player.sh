# shellcheck shell=bash
# player.sh: plays a terminal demo step by step, so nobody types during a talk.
#
# A demo script sets the DEMO_ settings below, defines step1 to stepN, sources this file and calls
# demo_main "$@". README.md in this directory has a whole demo in a dozen lines; dt-demo.sh is a real one.
#
# Each step clears the screen and shows its title. It types its commands out one at a time, waiting
# for Enter before running each, as a terminal would. After the step it checks the output for what
# the step says to look for and marks each item. record runs every step unattended and saves each
# command's output; replay plays the saved outputs back, and f shows them during a live run.
#
# Settings, from the demo script or the caller's environment:
#   DEMO_DIR         the demo's own directory (required)
#   DEMO_STEPS       how many steps: step1 to stepN (required)
#   DEMO_WT          the directory every command runs in (required, except for replay)
#   DEMO_COUNTED     how many steps are numbered on screen; later ones are titled CLOSE (default: all)
#   DEMO_SAVED       where record writes the outputs and replay reads them (default: $DEMO_DIR/recordings)
#   DEMO_TYPE_SPEED  seconds per typed character, 0 to print at once (default 0.02)
#   DEMO_TIMEOUT     seconds a command may run before it is stopped (default 180)
#   DEMO_HINTS=1     show a step's "look for" list before it runs
#   DEMO_MASK=0      show LAN addresses and full paths, which are masked by default
# Hooks a demo may define: demo_setup [ARG], demo_reset [quiet], demo_undo STEP, demo_cleanup,
# demo_after_record.
#
# Works with macOS's /bin/bash 3.2. Needs perl.

: "${DEMO_DIR:?set DEMO_DIR to the directory of the demo}"
: "${DEMO_STEPS:?set DEMO_STEPS to the number of steps}"
DEMO_WT="${DEMO_WT:-}"
DEMO_COUNTED="${DEMO_COUNTED:-$DEMO_STEPS}"
DEMO_SAVED="${DEMO_SAVED:-$DEMO_DIR/recordings}"
DEMO_TYPE_SPEED="${DEMO_TYPE_SPEED:-0.02}"
DEMO_TIMEOUT="${DEMO_TIMEOUT:-180}"
DEMO_HINTS="${DEMO_HINTS:-0}"
DEMO_MASK="${DEMO_MASK:-1}"
# How the demo's directory shows on screen: its path in its repository, or else its name.
DEMO_LABEL=$(git -C "$DEMO_DIR" rev-parse --show-prefix 2>/dev/null)
DEMO_LABEL="${DEMO_LABEL%/}"
DEMO_LABEL="${DEMO_LABEL:-${DEMO_DIR##*/}}"

if [ -t 1 ]; then
  B=$'\e[1m' D=$'\e[2m' R=$'\e[31m' G=$'\e[32m' Y=$'\e[33m' C=$'\e[36m' N=$'\e[0m'
else
  B='' D='' R='' G='' Y='' C='' N=''
fi

say()    { printf '%s\n' "$*"; }
strong() { printf '%s%s%s\n' "$B" "$*" "$N"; }
ok()     { printf '%s  ok%s    %s\n' "$G" "$N" "$*"; }
warn()   { printf '%s  warn%s  %s\n' "$Y" "$N" "$*"; }
bad()    { printf '%s  fail%s  %s\n' "$R" "$N" "$*"; }

# demo_name: how to call this demo again, for messages.
demo_name() { printf '%s/%s' "$DEMO_LABEL" "${0##*/}"; }

# hook NAME [ARGS]: runs the demo's hook when it defines one.
hook() { local f=$1; shift; if declare -F "$f" >/dev/null; then "$f" "$@"; fi; }

# demo_mask: hides the machine's private IPv4 addresses and shortens the demo's paths, unless
# DEMO_MASK=0. 172.17.0.x is left alone: it is Docker's bridge inside its VM, no address of the host's.
demo_mask() {
  if [ "$DEMO_MASK" = 1 ]; then
    DD="$DEMO_DIR" DL="$DEMO_LABEL" WT="$DEMO_WT" perl -pe '
      BEGIN { $| = 1 }
      s/\Q$ENV{DD}\E/$ENV{DL}/g if length $ENV{DD};
      s/\Q$ENV{WT}\E/<worktree>/g if length $ENV{WT};
      s/\Q$ENV{HOME}\E/~/g if length $ENV{HOME};
      s/(?<![\d.])(?:10(?:\.\d{1,3}){3}|192\.168(?:\.\d{1,3}){2}|172\.(?!17\.0\.)(?:1[6-9]|2\d|3[01])(?:\.\d{1,3}){2})(?![\d.]*\d)/<lan-ip>/g'
  else
    cat
  fi
}

# typeit CMD: prints the prompt and the command a character at a time.
typeit() {
  printf '%s$%s ' "$C" "$N"
  if [ "$DEMO_TYPE_SPEED" = 0 ]; then
    printf '%s' "$1"
  else
    S="$DEMO_TYPE_SPEED" perl -e '$| = 1; for (split //, $ARGV[0]) { print; select(undef, undef, undef, $ENV{S}) }' "$1"
  fi
}

# wait_enter: the presenter's Enter after the typed command, as in a terminal. It reads the
# player's own input, which demo_main keeps on fd 3: cmd's stdin is the step's text, already read.
wait_enter() { [ "$MODE" = record ] && { echo; return; }; local k; IFS= read -rs k <&3; echo; }

# run_limited SECONDS CMD...: runs CMD in a process group of its own and, after SECONDS, stops the
# whole group (TERM, then KILL 40 s later: on TERM a cell's fixture may take 25 s to unmount, then
# 10 s to stop dfs), so no child outlives the limit; it then exits 142.
# Ctrl-C stops the group too, then the player.
run_limited() {
  perl -e '
    my $t = shift;
    my $pid = fork;
    defined $pid or die "fork: $!\n";
    if (!$pid) { setpgrp(0, 0); exec @ARGV or exit 127 }
    my ($late, $int) = (0, 0);
    $SIG{ALRM} = sub { kill($late++ ? "KILL" : "TERM", -$pid); alarm 40 };
    $SIG{INT} = sub { $int = 1; kill "TERM", -$pid };
    $SIG{TERM} = sub { kill "TERM", -$pid };
    alarm $t;
    1 while waitpid($pid, 0) == -1 && $!{EINTR};
    my $st = $?;
    alarm 0;
    if ($int) { $SIG{INT} = "DEFAULT"; kill "INT", $$ }
    if ($late) { kill "KILL", -$pid; exit 142 }
    exit($st & 127 ? 128 + ($st & 127) : $st >> 8);
  ' "$@"
}

# ---- what a step calls -----------------------------------------------------------------------------
# A step calls title, then look for each item to check, then cmd for each command (its text on stdin).
# cmd numbers the step's commands: the saved output of step S's K-th command is $DEMO_SAVED/stepS-K.txt.

MODE='' STEP=0 IDX=0 CAP='' BROKE=0 TAKES_BROKEN=''
LOOK_RE=() LOOK_TXT=()

title() { # TITLE SUBTITLE
  IDX=0 LOOK_RE=() LOOK_TXT=() BROKE=0
  CAP=$(mktemp "${TMPDIR:-/tmp}/demo.XXXXXX") || exit 1
  [ "$MODE" = record ] || { command -v tput >/dev/null && tput clear || printf '\e[H\e[2J'; }
  local n="CLOSE"
  [ "$STEP" -le "$DEMO_COUNTED" ] && n="STEP $STEP OF $DEMO_COUNTED"
  printf '%s%s%s\n%s%s%s\n%s%s%s\n\n' "$D" "$n" "$N" "$B" "$1" "$N" "$D" "$2" "$N"
}

look() { LOOK_RE+=("$1"); LOOK_TXT+=("$2"); } # REGEX TEXT: an extended regex the step's output must match

hints() { # with DEMO_HINTS=1, before a step's first command
  [ "$DEMO_HINTS" = 1 ] && [ "${#LOOK_TXT[@]}" -gt 0 ] || return 0
  local i
  printf '%sLook for:%s\n' "$D" "$N"
  for ((i = 0; i < ${#LOOK_TXT[@]}; i++)); do printf '%s  - %s%s\n' "$D" "${LOOK_TXT[$i]}" "$N"; done
  echo
}

cmd() { # [TIMEOUT] < command text
  local c t=${1:-$DEMO_TIMEOUT} f rc shown
  c=$(cat)
  [ "$IDX" = 0 ] && hints
  IDX=$((IDX + 1))
  f="$DEMO_SAVED/step$STEP-$IDX.txt"
  shown=${c//"$DEMO_DIR"/$DEMO_LABEL}
  typeit "$shown"
  wait_enter
  case "$MODE" in
    replay)
      if [ -f "$f" ]; then
        sed 1d "$f" | perl -pe 'BEGIN { $| = 1 } select(undef, undef, undef, 0.004)' | tee -a "$CAP"
      else
        printf '%s(no saved output: %s)%s\n' "$Y" "$f" "$N"
      fi
      ;;
    *)
      (cd "$DEMO_WT" && run_limited "$t" bash -c "$c" 2>&1) | demo_mask | tee -a "$CAP" | {
        if [ "$MODE" = record ]; then
          { printf '# %s | %s | %s\n' "$shown" "$(date '+%Y-%m-%d %H:%M %Z')" "$(git -C "$DEMO_WT" rev-parse --short=9 HEAD 2>/dev/null || echo -)"; cat; } >"$f.new"
          sed 1d "$f.new" | tail -n 3
        else
          cat
        fi
      }
      rc=${PIPESTATUS[0]}
      if [ "$rc" -ge 128 ]; then
        BROKE=1
        printf '%s(stopped after %ss)%s\n' "$R" "$t" "$N"
      fi
      ;;
  esac
  echo
}

checks() {
  [ "${#LOOK_RE[@]}" -gt 0 ] || return 0
  local i miss=0
  printf '%sLOOK FOR%s\n' "$D" "$N"
  for ((i = 0; i < ${#LOOK_RE[@]}; i++)); do
    if grep -Eq -- "${LOOK_RE[$i]}" "$CAP"; then
      printf '  %s✓%s %s\n' "$G" "$N" "${LOOK_TXT[$i]}"
    else
      printf '  %s✗%s %s\n' "$R" "$N" "${LOOK_TXT[$i]}"
      miss=1
    fi
  done
  [ "$miss" = 0 ] || BROKE=1
}

# keep_take: in record, a step whose checks all passed replaces its saved outputs; a step that
# broke keeps the earlier ones, so a bad take never becomes the fallback.
keep_take() {
  local f
  if [ "$BROKE" = 0 ]; then
    rm -f "$DEMO_SAVED/step$STEP"-*.txt
    for f in "$DEMO_SAVED/step$STEP"-*.txt.new; do [ -f "$f" ] && mv "$f" "${f%.new}"; done
  else
    rm -f "$DEMO_SAVED/step$STEP"-*.txt.new
    TAKES_BROKEN="$TAKES_BROKEN $STEP"
    warn "step $STEP: something differs, so its earlier recording stays"
  fi
}

# ---- the player ------------------------------------------------------------------------------------

play() {
  if [ "$MODE" != replay ]; then
    { [ -n "$DEMO_WT" ] && [ -d "$DEMO_WT" ]; } || { bad "no directory to run in (DEMO_WT=$DEMO_WT): run $(demo_name) setup first"; exit 1; }
    trap 'hook demo_reset quiet' EXIT
  fi
  [ "$MODE" = record ] && mkdir -p "$DEMO_SAVED"
  STEP=1
  while [ "$STEP" -le "$DEMO_STEPS" ]; do
    "step$STEP"
    checks
    rm -f "$CAP"
    if [ "$MODE" = record ]; then
      keep_take
      STEP=$((STEP + 1))
      continue
    fi
    local hint="Enter next · r again · f saved output · b back · 1-9 jump · q quit"
    [ "$BROKE" = 1 ] && hint="${Y}something differs: f shows the saved output${N} · $hint"
    printf '\n%s%s%s ' "$D" "$hint" "$N"
    local k
    IFS= read -rsn1 k
    echo
    case "$k" in
      "") STEP=$((STEP + 1)) ;;
      r) hook demo_undo "$STEP" ;; # undo the step's own edit, so it applies once again
      f) show_saved "$STEP"; printf '%sEnter to go on%s ' "$D" "$N"; IFS= read -rs k; STEP=$((STEP + 1)) ;;
      b) [ "$STEP" -gt 1 ] && STEP=$((STEP - 1)) ;;
      [1-9]) [ "$k" -le "$DEMO_STEPS" ] && STEP=$k ;;
      q) break ;;
    esac
  done
  printf '\n%send of the demo%s\n' "$D" "$N"
}

show_saved() { # STEP
  local f found=0
  for f in "$DEMO_SAVED"/step"$1"-*.txt; do
    [ -f "$f" ] || continue
    found=1
    printf '%s$%s %s\n' "$C" "$N" "$(head -1 "$f" | sed 's/^# //; s/ | .*//')"
    sed 1d "$f"
    echo
  done
  [ "$found" = 1 ] || printf '%sno saved output for step %s in %s (run: %s record)%s\n' "$Y" "$1" "$DEMO_SAVED" "$(demo_name)" "$N"
}

usage() { # the demo script's opening comment
  awk 'NR == 1 { next } /^# shellcheck/ { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
}

demo_main() {
  exec 3<&0 # the presenter's keys, for wait_enter
  case "${1:-}" in
    setup) MODE=setup; if declare -F demo_setup >/dev/null; then demo_setup "${2:-}"; else ok "this demo needs no setup"; fi ;;
    record)
      MODE=record; DEMO_TYPE_SPEED=0; play; hook demo_reset; hook demo_after_record
      [ -z "$TAKES_BROKEN" ] || { bad "step(s)$TAKES_BROKEN broke: their earlier recordings stay in $DEMO_SAVED"; exit 1; }
      ok "saved to $DEMO_SAVED" ;;
    replay) MODE=replay; play ;;
    reset) MODE=reset; hook demo_reset ;;
    cleanup) MODE=cleanup; hook demo_reset quiet; hook demo_cleanup ;;
    "" | live) MODE=live; play ;;
    -h | --help | help) usage ;;
    *) bad "unknown command: $1 (setup, live, replay, record, reset, cleanup)"; exit 2 ;;
  esac
}
