#!/usr/bin/env bash
# End-to-end demo with the REAL binaries, a fictional supplier and a throw-away database.
# Needs: Go toolchain (with a C compiler for SQLite), curl. Run from the repository root:  ./scripts/demo.sh
set -u
cd "$(dirname "$0")/.."

APP_PORT="${APP_PORT:-8080}"
SUP_PORT="${SUP_PORT:-9090}"
APP="http://localhost:${APP_PORT}"
SUP="http://localhost:${SUP_PORT}"
WORK="$(mktemp -d)"
DB="${WORK}/demo.db"
SUP_PID=""; APP_PID=""

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
note() { printf '\033[2m%s\033[0m\n' "$*"; }
# Stop whatever listens on a port. On Git Bash for Windows the PID in $! is not the Windows
# PID, so a plain `kill` can silently miss the process; there we fall back to taskkill.
stop_port() {  # port, [bash pid]
  [ -n "${2:-}" ] && kill "$2" 2>/dev/null
  if command -v taskkill >/dev/null 2>&1; then
    local pid
    pid="$(netstat -ano 2>/dev/null | tr -d '\r' | awk -v p=":$1" '$2 ~ p"$" && $4 == "LISTENING" {print $5; exit}')"
    [ -n "$pid" ] && taskkill //F //PID "$pid" >/dev/null 2>&1
  fi
}
port_in_use() { curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$1/"; }  # curl exit 7 = connection refused
wait_port_free() {
  for _ in $(seq 1 100); do port_in_use "$1" || return 0; sleep 0.1; done
  echo "ERROR: something still listens on port $1. Stop it (netstat -ano | grep :$1 ; taskkill //F //PID <pid>) and re-run."
  exit 1
}
cleanup() {
  stop_port "$APP_PORT" "$APP_PID"
  stop_port "$SUP_PORT" "$SUP_PID"
  wait 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

for port in "$APP_PORT" "$SUP_PORT"; do
  if port_in_use "$port"; then
    echo "ERROR: port $port is already in use (an earlier demo run still alive?). Free it or set APP_PORT / SUP_PORT."
    exit 1
  fi
done

get()  { curl -s "$APP$1"; }
post() { curl -s -X POST "$APP$1" ${2:+-H 'Content-Type: application/json' -d "$2"}; }
field() { sed -n "s/^  \"$1\": \"\{0,1\}\([^\",]*\).*/\1/p" | head -1; }   # top-level scalar field of pretty JSON
line()  { grep -E "\"$1\"" | head -1 | sed 's/^ *//'; }

wait_for_status() {  # id, then acceptable statuses (regex)
  for _ in $(seq 1 200); do
    s="$(get "/v1/updates/$1" | field status)"
    if echo "$s" | grep -Eq "^($2)$"; then return 0; fi
    sleep 0.1
  done
  echo "timed out waiting for update $1 ($2), last status: $s"; return 1
}
wait_up() { for _ in $(seq 1 100); do curl -s -o /dev/null "$1" && return 0; sleep 0.1; done; echo "server at $1 did not start"; exit 1; }
start_app() { ./bin/server -addr ":${APP_PORT}" -db "$DB" -supplier "$SUP" -retry-backoff 50ms "$@" >>"$WORK/app.log" 2>&1 & APP_PID=$!; wait_up "$APP/healthz"; }
show_summary() { get "/v1/updates/$1" | sed -n 's/^  "summary": "\(.*\)",$/\1/p'; }
show_counts()  { get /v1/catalog/summary | tr -d '\n ' ; echo; }

say "0. Build and start the fictional supplier and the application"
mkdir -p bin
go build -o bin/server ./cmd/server && go build -o bin/fakesupplier ./cmd/fakesupplier || exit 1
./bin/fakesupplier -addr ":${SUP_PORT}" -feeds data/feeds >"$WORK/supplier.log" 2>&1 & SUP_PID=$!
wait_up "$SUP/admin/outage" 2>/dev/null || sleep 0.5
start_app
note "database: $DB"

say "1. SUPPLIER OUTAGE: start day 1 while the supplier goes down at batch 2"
curl -s -X POST "$SUP/admin/outage?from_batch=2"
post /v1/updates '{"feed":"day1-initial"}' | line status
wait_for_status 1 "interrupted|completed"
echo "status : $(get /v1/updates/1 | field status)   complete: $(get /v1/updates/1 | field complete)"
echo "summary: $(show_summary 1)"
note "Batches 0 and 1 are kept (nothing erased), and the update is NOT reported as complete:"
show_counts

say "2. RESUME after the supplier is back"
curl -s -X DELETE "$SUP/admin/outage"
post /v1/updates/1/resume | line status
wait_for_status 1 "completed|interrupted"
echo "status : $(get /v1/updates/1 | field status)   complete: $(get /v1/updates/1 | field complete)"
echo "summary: $(show_summary 1)"
note "Problems that still need attention (invalid records and one version conflict):"
get "/v1/updates/1/issues?status=open" | grep -E '"(kind|sku|message)"' | sed 's/^ *//'
note "HP-1008: withdrawal v3 arrived BEFORE the older upsert v2, so it stays withdrawn:"
get /v1/products/HP-1008 | grep -E '"(status|version)"' | sed 's/^ *//'

say "3. IDEMPOTENCY: import the very same day-1 update again"
post /v1/updates '{"feed":"day1-initial"}' | line status
wait_for_status 2 "completed|interrupted"
echo "summary: $(show_summary 2)"
note "Same product count as before; nothing created, updated or withdrawn:"
show_counts

say "4. APPLICATION CRASH: day 2 with a process that dies (like kill -9) after 2 batches"
stop_port "$APP_PORT" "$APP_PID"; wait_port_free "$APP_PORT"
start_app -demo-crash-after-batch 2
post /v1/updates '{"feed":"day2-changes"}' | line status
for _ in $(seq 1 100); do port_in_use "$APP_PORT" || break; sleep 0.1; done   # wait until the process is gone
if port_in_use "$APP_PORT"; then echo "ERROR: the application did not crash as the demo expects"; exit 1; fi
echo "the application process died without any clean shutdown"
note "Restarting the application against the same database..."
start_app
status3="$(get /v1/updates/3 | field status)"
echo "status : $status3   complete: $(get /v1/updates/3 | field complete)"
echo "summary: $(show_summary 3)"
if [ "$status3" != "interrupted" ]; then echo "ERROR: expected update 3 to be 'interrupted' after the crash, got '$status3'"; exit 1; fi
post /v1/updates/3/resume | line status
wait_for_status 3 "completed|interrupted"
echo "status : $(get /v1/updates/3 | field status)   complete: $(get /v1/updates/3 | field complete)"
echo "summary: $(show_summary 3)"

say "5. WHAT CHANGED in day 2"
get "/v1/updates/3/changes" | grep -E '^      "(sku|change_type)"' | sed 's/^ *//; s/"//g; s/,$//' | paste - - | sed 's/\t/   /'
note "HP-1008 history (withdrawn by v3, relisted only by the NEWER v4):"
get /v1/products/HP-1008/history | grep -E '"change_type"' | sed 's/^ *//'
say "Final catalog"
show_counts
