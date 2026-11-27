#!/usr/bin/env bash
#
# Only a push run saves to the Actions cache.
#
# A pull request run's cache entry can be read only by that pull request's
# runs, and a merge queue run's only by its own re-runs, while every run reads
# its base branch's. Their saves serve almost nothing and push the repo past
# its cache quota, where GitHub evicts the base branches' entries first.
#
# Three checks:
#   - in a workflow that runs on pull_request or merge_group, and in every
#     action under .github/actions, an actions/setup-go step sets cache: false
#     (its default saves on every event); .github/actions/go-cache restores the
#     Go caches instead;
#   - there, no actions/cache step either, whose post step saves on every event;
#   - every actions/cache/save step's if: requires github.event_name == 'push'.
#
# Usage: go-cache-push-only.sh [repo root]
set -euo pipefail

ROOT="${1:-.}"

fail=0
for file in "$ROOT"/.github/workflows/*.yml "$ROOT"/.github/workflows/*.yaml \
    "$ROOT"/.github/actions/*/action.yml "$ROOT"/.github/actions/*/action.yaml; do
    [ -f "$file" ] || continue
    pr=0
    case "$file" in
        */.github/actions/*) pr=1 ;;
        *) grep -q -E '^  (pull_request|merge_group):' "$file" && pr=1 ;;
    esac
    # Each step of a steps: list is read whole, from its "- " to the next one at
    # the same indent, so a key before uses: (if:, name:) counts too.
    awk -v pr="$pr" '
        function indent(s) { match(s, /^ */); return RLENGTH }
        function check(   n, lines, i, setup, nocache, both, save, pushonly) {
            n = split(buf, lines, "\n")
            for (i = 1; i <= n; i++) {
                if (lines[i] ~ /uses: *actions\/setup-go@/) setup = 1
                if (lines[i] ~ /^[ -]*cache: *false *(#.*)?$/) nocache = 1
                if (lines[i] ~ /uses: *actions\/cache@/) both = 1
                if (lines[i] ~ /uses: *actions\/cache\/save@/) save = 1
                if (lines[i] ~ /^[ -]*if:.*github\.event_name == .push./) pushonly = 1
            }
            if (pr && setup && !nocache) {
                printf "%s:%d: actions/setup-go without cache: false; use .github/actions/go-cache\n", FILENAME, start
                bad = 1
            }
            if (pr && both) {
                printf "%s:%d: actions/cache saves on every event; use actions/cache/restore and a push-only save\n", FILENAME, start
                bad = 1
            }
            if (save && !pushonly) {
                printf "%s:%d: actions/cache/save without github.event_name == '\''push'\'' in its if:\n", FILENAME, start
                bad = 1
            }
        }
        function flush() { if (buf != "") check(); buf = "" }
        /^ *#/ || /^ *$/ { next }
        /^ *steps:/ { flush(); insteps = 1; dash = -1; sindent = indent($0); next }
        insteps && indent($0) <= sindent { flush(); insteps = 0 }
        insteps && /^ *- / && (dash < 0 || indent($0) == dash) {
            flush(); dash = indent($0); buf = $0; start = NR; next
        }
        insteps && dash >= 0 { buf = buf "\n" $0 }
        END { flush(); exit bad }
    ' "$file" || fail=1
done

exit "$fail"
