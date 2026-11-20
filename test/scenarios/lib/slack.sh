# shellcheck shell=bash
# Posting to Slack, for the scenario runs (report.sh) and the nightly performance run
# (bench/ditto/night.sh). Nothing posts without a bot token (scope chat:write) in
# SLACK_DIR/slack-token and a channel ID in SLACK_DIR/slack-channel. SLACK_API is the API's address
# (default https://slack.com/api).
#
# The scenarios keep their own threads (report.sh). Any other notifier posts each result as a reply
# in its thread of the day (slack_day). A result that changes its state, a new failure or flag or
# the recovery from them, shows in the channel too (slack's BROADCAST); one that repeats the state
# stays in the thread.
# decision: this file sits beside report.sh, in the directory both the scenario runs and the tree
# night.sh extracts already hold, so neither has to take in another one.

# slack TEXT [THREAD] [BROADCAST]: posts TEXT and prints the new message's ts. The first line is the
# message, the rest a colored attachment: red failing, yellow slow, green passing. With THREAD it is
# a reply there; BROADCAST=yes shows the reply in the channel too. Does nothing without a token and
# a channel. The token goes to curl on stdin, never on the command line.
slack() {
    test -s "$SLACK_DIR/slack-token" && test -s "$SLACK_DIR/slack-channel" || return 0
    local TEXT COLOR TITLE DETAILS BODY
    case "$1" in :x:*) COLOR="#E01E5A" ;; :warning:*) COLOR="#ECB22E" ;; *) COLOR="#2EB67D" ;; esac
    # &, < and > are escaped for Slack, then the text becomes a JSON string.
    TEXT="$(printf '%s' "$1" | tr -d '\000-\011\013-\037' | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g')"
    TEXT="${TEXT//\\/\\\\}"
    TEXT="${TEXT//\"/\\\"}"
    TEXT="${TEXT//$'\n'/\\n}"
    TITLE="${TEXT%%\\n*}"
    DETAILS="${TEXT#*\\n}"
    BODY="{\"channel\":\"$(cat "$SLACK_DIR/slack-channel")\",\"text\":\"$TITLE\",\"attachments\":[{\"color\":\"$COLOR\",\"text\":\"$DETAILS\",\"mrkdwn_in\":[\"text\"]}]${2:+,\"thread_ts\":\"$2\"}"
    test "${3:-no}" = no || BODY="$BODY,\"reply_broadcast\":true"
    printf 'header = "Authorization: Bearer %s"\n' "$(cat "$SLACK_DIR/slack-token")" |
        curl -sf -K - -H 'Content-Type: application/json; charset=utf-8' -d "$BODY}" "${SLACK_API:-https://slack.com/api}/chat.postMessage" |
        grep -oE '"ts": *"[0-9]+\.[0-9]+"' | head -1 | grep -oE '[0-9]+\.[0-9]+' || true
}

# slack_day FILE TITLE: prints the ts of today's thread, posting its parent, "TITLE, <date>", the
# first time today. FILE keeps the day and the parent's ts. Today is the date in Europe/Berlin, the
# team's, whatever the host's time zone (SLACK_TODAY sets it). Without a parent, as when nothing
# posts, it prints nothing, so a reply posts at the top instead, and the next post tries the parent
# again.
# decision: the parent is a fixed title, not the day's first result, so a second result the same
# day is not filed under the first one's verdict.
slack_day() {
    local today="${SLACK_TODAY:-$(TZ=Europe/Berlin date +%F)}" day="" ts=""
    read -r day ts 2>/dev/null <"$1" || true
    if test "$day" != "$today"; then
        ts="$(slack ":calendar: *$2, $today*
Each result of the day is a reply in this thread; a change also shows in the channel.")"
        test -z "$ts" || echo "$today $ts" >"$1"
    fi
    echo "$ts"
}
