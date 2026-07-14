#!/usr/bin/env bash
# Runnable quickstart: start the bundled fixture API on a loopback port and
# flatten the same 57-record dataset through every pagination style. This
# is exactly what the README shows, minus any real API. Offline, no state.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

echo "building depage and the fixture server…"
(cd "$ROOT" && go build -o "$WORKDIR/depage" ./cmd/depage)
(cd "$ROOT" && go build -o "$WORKDIR/fixture-server" ./examples/fixture-server)

"$WORKDIR/fixture-server" --addr 127.0.0.1:0 > "$WORKDIR/addr" &
SERVER_PID=$!
for _ in $(seq 1 100); do
  [ -s "$WORKDIR/addr" ] && break
  sleep 0.05
done
BASE="$(head -n1 "$WORKDIR/addr")"
echo "fixture API listening at $BASE"
echo

for path in "cursor/users?limit=10" \
            "offset/users?offset=0&limit=10" \
            "page/users?page=1&per_page=10" \
            "link/users?page=1" \
            "nexturl/users?page=1"; do
  echo "\$ depage '$BASE/$path'"
  "$WORKDIR/depage" "$BASE/$path" > "$WORKDIR/out.ndjson"
  echo "   first record:  $(head -n1 "$WORKDIR/out.ndjson")"
  echo "   record count:  $(wc -l < "$WORKDIR/out.ndjson" | tr -d ' ')"
  echo
done

echo "same 57 records five ways — that is the point."
