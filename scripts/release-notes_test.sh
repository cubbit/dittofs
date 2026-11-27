#!/usr/bin/env bash
# Tests for release-notes.py: the notes it writes carry the CHANGELOG.md
# section, and stay under GitHub's release body limit however long the
# commit list GoReleaser wrote.
#
# Usage: ./release-notes_test.sh

set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/release-notes.py"
FAILURES=0
ASSERTIONS=0

ok()   { ASSERTIONS=$((ASSERTIONS + 1)); echo "ok: $1"; }
fail() { FAILURES=$((FAILURES + 1));     echo "FAIL: $1"; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
cd "$WORK" || exit 2
printf '## [1.2.3]\n\n- The change.\n\n## [1.2.2]\n\n- Older.\n' >CHANGELOG.md

# notes NAME VERSION BODY_LINES: the notes for VERSION over a body of
# BODY_LINES commit lines after an installation header.
notes() {
	{ echo "### Installation"; yes '* abcdef0 feat: a commit subject of a fair length (#1234)' | head -n "$3"; } |
		python3 "$SCRIPT" "v$2"
}

out=$(notes short 1.2.3 10)
if [[ "$out" == *"- The change."* && "$out" != *"cut to fit"* ]]; then ok "a short body keeps its section, uncut"; else fail "a short body: $out"; fi

for v in 1.2.3 9.9.9; do
	out=$(notes long "$v" 5000)
	if [ "${#out}" -gt 125000 ]; then
		fail "a long body for $v: ${#out} characters, want at most 125000"
	elif [[ "$out" != *"cut to fit GitHub's release size limit"* ]]; then
		fail "a long body for $v: no note that it was cut"
	else
		ok "a long body for $v is cut below the limit, with a note"
	fi
done

echo
echo "$ASSERTIONS passed, $FAILURES failed"
[ "$FAILURES" -eq 0 ]
