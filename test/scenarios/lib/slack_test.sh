#!/usr/bin/env bash
# Tests of the Slack library (slack.sh) and its two users: the scenario runs' messages go out as
# they always have, and the nightly performance run posts by the daily-thread rule. Every post goes
# to a local fake of the API, never to Slack.
#
# Usage: test/scenarios/lib/slack_test.sh

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
TMP="$(mktemp -d)"
FAKE=""
trap 'test -z "$FAKE" || kill "$FAKE"; rm -rf "$TMP"' EXIT

FAILURES=0
ok()   { echo "ok: $1"; }
fail() { FAILURES=$((FAILURES + 1)); echo "FAIL: $1"; }

# The fake API: it appends each chat.postMessage body to TMP/posts, one line each, and answers with
# ts <n>.000000, n the posts it has taken. While TMP/refuse exists, it refuses the next post, as
# Slack does with ok false, and removes TMP/refuse.
cat >"$TMP/fake.py" <<'EOF'
import http.server
import os
import sys

posts = open(sys.argv[1], "a")
count = 0


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        global count
        body = self.rfile.read(int(self.headers["Content-Length"])).decode()
        self.send_response(200)
        self.end_headers()
        if os.path.exists(sys.argv[2]):
            os.remove(sys.argv[2])
            self.wfile.write(b'{"ok":false,"error":"refused"}')
            return
        count += 1
        posts.write(body + "\n")
        posts.flush()
        self.wfile.write(b'{"ok":true,"ts":"%d.000000"}' % count)

    def log_message(self, *args):
        pass


server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
print(server.server_address[1], flush=True)
server.serve_forever()
EOF
touch "$TMP/posts"
mkfifo "$TMP/port"
python3 "$TMP/fake.py" "$TMP/posts" "$TMP/refuse" >"$TMP/port" &
FAKE=$!
read -r PORT <"$TMP/port"
test -n "$PORT" || { echo "FAIL: the fake API did not start"; exit 1; }
export SLACK_API="http://127.0.0.1:$PORT" SLACK_DIR="$TMP/secrets"
mkdir "$SLACK_DIR"
echo test-token >"$SLACK_DIR/slack-token"
echo C0TEST >"$SLACK_DIR/slack-channel"

# take [brief]: sets GOT to the bodies posted since the last take, one line each; with brief, each
# as "<text> | <thread's ts, or top> | <broadcast or not>".
SEEN=0
take() {
    GOT="$(tail -n +$((SEEN + 1)) "$TMP/posts")"
    SEEN=$(($(wc -l <"$TMP/posts")))
    test -z "${1:-}" || GOT="$(python3 -c 'import json, sys
for d in map(json.loads, sys.stdin):
    print(d["text"], "|", d.get("thread_ts", "top"), "|", "broadcast" if d.get("reply_broadcast") else "not")' <<<"$GOT")"
}
# check NAME WANT: GOT is WANT.
check() {
    if test "$GOT" = "$2"; then ok "$1"; else fail "$1"; diff <(echo "$2") <(echo "$GOT"); fi
}

# A scenario run's messages, through report.sh, go out as before the poster moved to slack.sh:
# the color, the escaping, the thread and the broadcast.
(
    # shellcheck disable=SC2034 # report.sh reads them
    CACHE="$TMP/cache" SCENARIOS=(a.sh)
    # shellcheck source-path=SCRIPTDIR source=report.sh
    . "$HERE/lib/report.sh"
    slack ":x: *a.sh failed*
*Check:* \`line 3: test \"\$(cat f)\" = a & b <c>\`
$(printf '\t')tab\\back" 1700000000.000100 >/dev/null
    slack ":warning: *a.sh passed but was slow*
31 s (usually 3 s) line 12: x" >/dev/null
    slack ":white_check_mark: *a.sh passes again*
host · commit \`abc\`" 2.000000 yes >/dev/null
)
take
# shellcheck disable=SC2016 # the bodies are literal
check "a scenario run's messages are the bodies they always were" \
    '{"channel":"C0TEST","text":":x: *a.sh failed*","attachments":[{"color":"#E01E5A","text":"*Check:* `line 3: test \"$(cat f)\" = a &amp; b &lt;c&gt;`\ntab\\back","mrkdwn_in":["text"]}],"thread_ts":"1700000000.000100"}
{"channel":"C0TEST","text":":warning: *a.sh passed but was slow*","attachments":[{"color":"#ECB22E","text":"31 s (usually 3 s) line 12: x","mrkdwn_in":["text"]}]}
{"channel":"C0TEST","text":":white_check_mark: *a.sh passes again*","attachments":[{"color":"#2EB67D","text":"host · commit `abc`","mrkdwn_in":["text"]}],"thread_ts":"2.000000","reply_broadcast":true}'

# The day's thread: its parent is posted with the day's first result and reused all day; the next
# day gets a parent of its own.
# shellcheck source-path=SCRIPTDIR source=slack.sh
. "$HERE/lib/slack.sh"
FIRST="$(SLACK_TODAY=2026-11-20 slack_day "$TMP/day" Notes)"
AGAIN="$(SLACK_TODAY=2026-11-20 slack_day "$TMP/day" Notes)"
NEXT="$(SLACK_TODAY=2026-11-21 slack_day "$TMP/day" Notes)"
take
check "a day's parent is posted once, the next day's once" \
    '{"channel":"C0TEST","text":":calendar: *Notes, 2026-11-20*","attachments":[{"color":"#2EB67D","text":"Each result of the day is a reply in this thread; a change also shows in the channel.","mrkdwn_in":["text"]}]}
{"channel":"C0TEST","text":":calendar: *Notes, 2026-11-21*","attachments":[{"color":"#2EB67D","text":"Each result of the day is a reply in this thread; a change also shows in the channel.","mrkdwn_in":["text"]}]}'
if test -n "$FIRST" && test "$AGAIN" = "$FIRST" && test "$NEXT" != "$FIRST"; then
    ok "the same day gets the same thread, the next day another"
else
    fail "the same day gets the same thread, the next day another: $FIRST, $AGAIN, $NEXT"
fi

# The nightly performance run. night DATE ALERT: posts a night of DATE whose alert (score.py's)
# is ALERT and whose summary is "<DATE> summary" over "details".
night() {
    mkdir -p "$TMP/perf/$1"
    printf '%s summary\ndetails\n' "$1" >"$TMP/perf/$1/slack.txt"
    echo "$2" >"$TMP/perf/$1/alert"
    SLACK_TODAY="$1" REPO="$ROOT" "$ROOT/bench/ditto/night.sh" post "$TMP/perf/$1"
}
# The ts the fake gives the next post.
next_ts() { echo "$((SEEN + 1)).000000"; }

PARENT="$(next_ts)"
night 2026-11-20 new
take brief
check "a night with a new flag or failure replies in the day's thread, shown in the channel too" \
    ":calendar: *Nightly performance, 2026-11-20* | top | not
2026-11-20 summary | $PARENT | broadcast"

night 2026-11-20 same
take brief
check "a second night the same day joins its thread; a repeat stays there" \
    "2026-11-20 summary | $PARENT | not"

PARENT="$(next_ts)"
night 2026-11-21 same
take brief
check "a repeat on the next day stays in that day's thread" \
    ":calendar: *Nightly performance, 2026-11-21* | top | not
2026-11-21 summary | $PARENT | not"

PARENT="$(next_ts)"
night 2026-11-22 clear
take brief
check "the first night with nothing flagged or failing after an alert shows in the channel too" \
    ":calendar: *Nightly performance, 2026-11-22* | top | not
:white_check_mark: *Nightly performance: nothing flagged or failing any more* | $PARENT | broadcast"

PARENT="$(next_ts)"
night 2026-11-23 clear
take brief
check "a quiet night with no alert open stays in the thread" \
    ":calendar: *Nightly performance, 2026-11-23* | top | not
2026-11-23 summary | $PARENT | not"

PARENT="$(next_ts)"
night 2026-11-24 same
take brief
check "flags or failures already seen, back after a clear night, show in the channel too" \
    ":calendar: *Nightly performance, 2026-11-24* | top | not
2026-11-24 summary | $PARENT | broadcast"

# A day's parent that fails to post: the night's reply goes at the top, the day is not kept, and the
# next post that day tries the parent again.
touch "$TMP/refuse"
night 2026-11-25 clear
take brief
check "a night whose parent fails to post replies at the top" \
    ":white_check_mark: *Nightly performance: nothing flagged or failing any more* | top | broadcast"
PARENT="$(next_ts)"
night 2026-11-25 clear
take brief
check "and the next post that day posts the parent and replies in it" \
    ":calendar: *Nightly performance, 2026-11-25* | top | not
2026-11-25 summary | $PARENT | not"

if (unset SLACK_DIR && night 2026-11-26 new); then
    take
    check "without SLACK_DIR a night posts nothing" ""
else
    fail "without SLACK_DIR a night posts nothing: the post failed"
fi

test "$FAILURES" -eq 0 || { echo "$FAILURES failed"; exit 1; }
echo "all passed"
