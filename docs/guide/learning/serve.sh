#!/usr/bin/env bash
# Serves the learning decks at http://127.0.0.1:$PORT/learn-dittofs/.
# Decks fetch their strings and glossary as JSON, which browsers refuse over
# file://, so they need a server even locally. It listens on loopback only;
# BIND=0.0.0.0 serves the decks to other machines too.
set -euo pipefail

PORT="${PORT:-8765}"
BIND="${BIND:-127.0.0.1}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Serving http://${BIND}:${PORT}/learn-dittofs/"
exec python3 -m http.server "$PORT" --bind "$BIND" --directory "$ROOT"
