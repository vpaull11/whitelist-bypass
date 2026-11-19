#!/bin/sh
# Live SFU test: one creator, 2 or 3 independent joiners, identical conn IDs,
# distinct response bodies and a persistent TCP stream across a peer restart.
# Usage: CLIENTS=3 ./headless/tests/test-telemost-multi-client.sh cookies.json
set -eu
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CREATOR="${CREATOR:-$ROOT/headless/telemost/headless-telemost-creator}"
JOINER="${JOINER:-$ROOT/headless/telemost-joiner/headless-telemost-joiner}"
PYTHON="${PYTHON:-python3}"
CLIENTS="${CLIENTS:-2}"
DURATION="${DURATION:-95}"
BASE_PORT="${BASE_PORT:-11080}"
case "$CLIENTS" in 2|3) ;; *) echo 'CLIENTS must be 2 or 3' >&2; exit 2;; esac
case "$DURATION" in ''|*[!0-9]*) echo 'Invalid DURATION' >&2; exit 2;; esac
[ "$DURATION" -ge 90 ] && [ "$DURATION" -le 300 ] || { echo 'DURATION must be 90..300' >&2; exit 2; }
[ "$#" -eq 1 ] && [ -f "$1" ] || { echo "Usage: $0 <cookies-yandex.json>" >&2; exit 2; }
[ -x "$CREATOR" ] && [ -x "$JOINER" ] || { echo 'Build creator and joiner first' >&2; exit 2; }
command -v "$PYTHON" >/dev/null
command -v curl >/dev/null
LOGS=$(mktemp -d -t tm-multi.XXXXXX)
PIDS=''
cleanup() {
    trap - EXIT INT TERM
    for pid in $PIDS; do kill "$pid" 2>/dev/null || true; done
    wait 2>/dev/null || true
    echo "Logs: $LOGS"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
# Fixture listens on the creator machine's loopback. No public echo service,
# user-specific content or external test account is needed beyond Telemost.
"$PYTHON" -I "$ROOT/headless/tests/telemost-echo-server.py" >"$LOGS/http-port" 2>"$LOGS/http.log" &
HTTP_PID=$!; PIDS="$PIDS $HTTP_PID"
i=0
while [ ! -s "$LOGS/http-port" ]; do
    kill -0 "$HTTP_PID" 2>/dev/null || { cat "$LOGS/http.log" >&2; exit 1; }
    i=$((i+1)); [ "$i" -lt 30 ] || exit 1
    sleep 1
done
HTTP_PORT=$(tr -d '\r\n' <"$LOGS/http-port")
"$CREATOR" --cookies "$1" >"$LOGS/creator.log" 2>&1 &
CREATOR_PID=$!; PIDS="$PIDS $CREATOR_PID"
i=0; LINK=''
while [ "$i" -lt 60 ]; do
    LINK=$(sed -n 's/.*join_link: //p' "$LOGS/creator.log" | head -1 | tr -d '\r')
    [ -n "$LINK" ] && break
    kill -0 "$CREATOR_PID" 2>/dev/null || { tail -30 "$LOGS/creator.log" >&2; exit 1; }
    sleep 1; i=$((i+1))
done
[ -n "$LINK" ] || { echo 'No join link' >&2; exit 1; }
start_joiner() {
    number=$1; generation=$2
    # Intentionally vary pacing: one client's config must not reset neighbors.
    fps=24; batch=30
    [ "$number" -ne 2 ] || { fps=12; batch=15; }
    "$JOINER" --tm-link "$LINK" --socks-port "$((BASE_PORT+number))" --name "Multi-$number" \
        --vp8-fps "$fps" --vp8-batch "$batch" >"$LOGS/joiner-$number-$generation.log" 2>&1 &
    LAST_PID=$!; PIDS="$PIDS $LAST_PID"
    waited=0
    while ! grep -q 'TUNNEL CONNECTED' "$LOGS/joiner-$number-$generation.log"; do
        kill -0 "$LAST_PID" 2>/dev/null || { tail -30 "$LOGS/joiner-$number-$generation.log" >&2; exit 1; }
        waited=$((waited+1)); [ "$waited" -le 60 ] || { echo "Joiner $number timeout" >&2; exit 1; }
        sleep 1
    done
}
fetch() {
    number=$1; route=$2
    # Explicitly clear NO_PROXY: loopback must be reached through the tunnel,
    # not by curl directly to the fixture on this machine.
    curl --fail --silent --show-error --noproxy '' --socks5-hostname "127.0.0.1:$((BASE_PORT+number))" \
        --connect-timeout 15 --max-time "$((DURATION+45))" "http://127.0.0.1:$HTTP_PORT/$route"
}
number=1
while [ "$number" -le "$CLIENTS" ]; do
    start_joiner "$number" initial
    [ "$number" -ne 2 ] || SECOND_PID=$LAST_PID
    fetch "$number" "client/$number" >"$LOGS/body-$number"
    printf 'telemost-client-%s\n' "$number" >"$LOGS/expected-$number"
    cmp "$LOGS/expected-$number" "$LOGS/body-$number"
    number=$((number+1))
done
# Persistent requests are launched concurrently and must survive joiner 2's
# departure/reappearance, including a real SFU slot layout update.
fetch 1 "stream/1?seconds=$DURATION" >"$LOGS/stream-1" &
STREAM1=$!; PIDS="$PIDS $STREAM1"
STREAM3=''
if [ "$CLIENTS" -eq 3 ]; then
    fetch 3 "stream/3?seconds=$DURATION" >"$LOGS/stream-3" &
    STREAM3=$!; PIDS="$PIDS $STREAM3"
fi
sleep "$((DURATION/2))"
kill "$SECOND_PID"
wait "$SECOND_PID" 2>/dev/null || true
start_joiner 2 restarted
fetch 2 client/2 >"$LOGS/body-2-restarted"
cmp "$LOGS/expected-2" "$LOGS/body-2-restarted"
wait "$STREAM1"
[ -z "$STREAM3" ] || wait "$STREAM3"
for number in 1 3; do
    [ "$number" -le "$CLIENTS" ] || continue
    : >"$LOGS/expected-stream-$number"
    i=0
    while [ "$i" -lt "$DURATION" ]; do
        printf 'telemost-client-%s\n' "$number" >>"$LOGS/expected-stream-$number"
        i=$((i+1))
    done
    cmp "$LOGS/expected-stream-$number" "$LOGS/stream-$number"
done
echo "PASS: $CLIENTS clients, distinct payloads, ${DURATION}s persistent TCP, peer restart"
echo 'UDP, creator restart, upstream SOCKS and maximum SFU capacity require separate live checks.'
