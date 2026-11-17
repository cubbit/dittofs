#!/usr/bin/env bash
# The talk-night launcher: one typed command, a fresh journal every time.
# The scratch dir is removed first, so a story never meets the resume
# refusal — and an encore run works exactly like the first one.
#
#   ./run.sh                # the basic story
#   ./run.sh crash          # the durability story
#   ./run.sh limits         # the space story
set -euo pipefail
story="${1:-basic}"
dir="${2:-/tmp/jdemo-talk}"
rm -rf "$dir"
exec go run ./cmd/jdemo --story "$story" --dir "$dir"
