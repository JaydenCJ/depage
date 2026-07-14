#!/usr/bin/env bash
# End-to-end smoke test for depage: builds the CLI and the bundled fixture
# server, starts the server on an ephemeral loopback port, and flattens the
# same 57-record dataset through every pagination style, asserting on real
# NDJSON output, detection summaries, and exit codes. No network beyond
# 127.0.0.1, idempotent, finishes in seconds.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

BIN="$WORKDIR/depage"
FIXTURE="$WORKDIR/fixture-server"

echo "1. build the CLI and the fixture server"
(cd "$ROOT" && go build -o "$BIN" ./cmd/depage) || fail "go build ./cmd/depage failed"
(cd "$ROOT" && go build -o "$FIXTURE" ./examples/fixture-server) || fail "go build ./examples/fixture-server failed"

echo "2. version matches the manifest"
"$BIN" --version | grep -qx "depage 0.1.0" || fail "--version mismatch"

echo "3. start the fixture API on an ephemeral loopback port"
"$FIXTURE" --addr 127.0.0.1:0 > "$WORKDIR/addr" &
SERVER_PID=$!
for _ in $(seq 1 100); do
  [ -s "$WORKDIR/addr" ] && break
  sleep 0.05
done
BASE="$(head -n1 "$WORKDIR/addr")"
[ -n "$BASE" ] || fail "fixture server did not report its address"

echo "4. cursor pagination flattens to 57 NDJSON lines"
"$BIN" -q "$BASE/cursor/users?limit=10" > "$WORKDIR/cursor.ndjson" 2> "$WORKDIR/cursor.err" \
  || fail "cursor run failed: $(cat "$WORKDIR/cursor.err")"
[ "$(wc -l < "$WORKDIR/cursor.ndjson")" -eq 57 ] || fail "cursor: expected 57 lines"
head -n1 "$WORKDIR/cursor.ndjson" | grep -qx '{"id":1,"name":"user-01","team":"atlas"}' || fail "cursor: first record wrong"
tail -n1 "$WORKDIR/cursor.ndjson" | grep -q '"id":57' || fail "cursor: last record wrong"

echo "5. the summary names the detected style"
"$BIN" "$BASE/cursor/users?limit=10" > /dev/null 2> "$WORKDIR/summary"
grep -q "style=cursor (via body field /next_cursor), pages=6, items=57" "$WORKDIR/summary" \
  || fail "cursor summary wrong: $(cat "$WORKDIR/summary")"

echo "6. offset, page, Link-header, and next-URL styles all flatten identically"
for style in "offset:offset/users?offset=0&limit=10" \
             "page:page/users?page=1&per_page=10" \
             "link-header:link/users?page=1" \
             "next-url:nexturl/users?page=1"; do
  name="${style%%:*}"; path="${style#*:}"
  "$BIN" "$BASE/$path" > "$WORKDIR/$name.ndjson" 2> "$WORKDIR/$name.err" || fail "$name run failed"
  [ "$(wc -l < "$WORKDIR/$name.ndjson")" -eq 57 ] || fail "$name: expected 57 lines"
  grep -q "style=$name" "$WORKDIR/$name.err" || fail "$name: style not detected ($(cat "$WORKDIR/$name.err"))"
  cmp -s "$WORKDIR/cursor.ndjson" "$WORKDIR/$name.ndjson" || fail "$name: output differs from cursor output"
done

echo "7. an unpaginated endpoint is a single page (style=none)"
"$BIN" "$BASE/single/users" > "$WORKDIR/single.ndjson" 2> "$WORKDIR/single.err" || fail "single run failed"
[ "$(wc -l < "$WORKDIR/single.ndjson")" -eq 57 ] || fail "single: expected 57 lines"
grep -q "style=none" "$WORKDIR/single.err" || fail "single: expected style=none"

echo "8. --max-items truncates the stream"
[ "$("$BIN" -q --max-items 25 "$BASE/cursor/users?limit=10" | wc -l)" -eq 25 ] || fail "--max-items 25 not honored"

echo "9. --pages emits raw page envelopes"
"$BIN" -q --pages "$BASE/page/users?page=1&per_page=10" > "$WORKDIR/pages.ndjson" || fail "--pages run failed"
[ "$(wc -l < "$WORKDIR/pages.ndjson")" -eq 6 ] || fail "--pages: expected 6 page lines"
head -n1 "$WORKDIR/pages.ndjson" | grep -q '"total_pages":6' || fail "--pages: envelope missing"

echo "10. a 429 with Retry-After is retried transparently"
"$BIN" -q --retries 2 "$BASE/flaky/users?limit=20" > "$WORKDIR/flaky.ndjson" 2>&1 || fail "flaky run failed"
[ "$(wc -l < "$WORKDIR/flaky.ndjson")" -eq 57 ] || fail "flaky: expected 57 lines after retry"

echo "11. usage errors exit 2"
set +e
"$BIN" > /dev/null 2>&1
[ $? -eq 2 ] || fail "missing URL should exit 2"
"$BIN" --style sideways "$BASE/cursor/users" > /dev/null 2>&1
[ $? -eq 2 ] || fail "bad --style should exit 2"

echo "12. HTTP failures exit 1"
"$BIN" -q "$BASE/no/such/path" > /dev/null 2>&1
[ $? -eq 1 ] || fail "404 should exit 1"
set -e

echo "SMOKE OK"
