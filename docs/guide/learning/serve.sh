#!/usr/bin/env bash
# Serves the learning decks at http://0.0.0.0:$PORT/learn-dittofs/.
# Decks fetch their strings and glossary as JSON, which browsers refuse over
# file://, so they need a server even locally.
set -euo pipefail

PORT="${PORT:-8765}"
BIND="${BIND:-0.0.0.0}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Serving http://${BIND}:${PORT}/learn-dittofs/"
exec python3 -m http.server "$PORT" --bind "$BIND" --directory "$ROOT"
