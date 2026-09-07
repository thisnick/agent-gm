#!/usr/bin/env bash
# restore-drill -- spec section 15.2, performed rather than described.
#
# A backup nobody has restored is a file, not a backup. This script takes a
# real snapshot from a real running server, restores it into a FRESH data
# directory, starts a second server on it, and asserts that what was there is
# there. Then it does the same thing with the wrong data key, because the
# other half of section 15.2 -- "restoring the database without sessions/ (or
# without the key) leaves every account signed_out with its history intact" --
# is a promise about a failure mode, and a promise about a failure mode that
# has never been exercised is a guess.
#
# The three things that move together (section 15.2):
#
#   1. the snapshot          -> copied in as agent-gm.sqlite3
#   2. the whole sessions/   -> copied in wholesale
#   3. AGENT_GM_DATA_KEY     -> supplied to the restored server
#
# Everything here is the fake backend of section 13.1 with both
# AGENT_GM_BACKEND=fake and AGENT_GM_ALLOW_FAKE=1. No phone, no network, no
# Google, and the only phone numbers are 555 fixtures. Ports are chosen free
# at random and bound on loopback only.
set -euo pipefail

cd "$(dirname "$0")/.."

die() { echo "restore-drill: $*" >&2; exit 1; }
note() { echo "restore-drill: $*" >&2; }

command -v curl >/dev/null 2>&1 || die "curl is not on PATH"
command -v jq >/dev/null 2>&1 || die "jq is not on PATH; run this under devbox"

note "building"
go build -trimpath -o bin/agent-gm ./cmd/agent-gm

freeport() {
  node -e 'const s=require("net").createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close();});'
}

key="$(openssl rand -hex 32)"
wrong_key="$(openssl rand -hex 32)"
secret="$(openssl rand -hex 32)"

primary="$(mktemp -d)"
restored="$(mktemp -d)"
keyless="$(mktemp -d)"
logs="$(mktemp -d)"

# Every PID this script starts is captured at launch and signalled by that
# PID alone. `pkill -f agent-gm` would match this shell.
pids=()
cleanup() {
  for p in "${pids[@]:-}"; do
    [ -n "$p" ] || continue
    kill -0 "$p" 2>/dev/null && { kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; }
  done
  rm -rf "$primary" "$restored" "$keyless" "$logs"
}
trap cleanup EXIT

# start <data-dir> <data-key> sets $URL and $PID.
#
# It is deliberately NOT a function whose result is read with $( ): a command
# substitution keeps its pipe open until every process holding it exits, and a
# server started inside one holds it for ever. That is a hang with no error
# message, and it cost half an hour once already.
URL=""
PID=""
start() {
  local data="$1" k="$2" port
  port="$(freeport)"
  URL="http://127.0.0.1:${port}"
  AGENT_GM_DATA_DIR="$data" \
  AGENT_GM_DATA_KEY="$k" \
  AGENT_GM_ADMIN_SECRET="$secret" \
  AGENT_GM_PUBLIC_URL="$URL" \
  AGENT_GM_LISTEN_ADDR="127.0.0.1:${port}" \
  AGENT_GM_BACKEND=fake \
  AGENT_GM_ALLOW_FAKE=1 \
  AGENT_GM_FAKE_ACCOUNT=drill@example.test \
  AGENT_GM_LOG_LEVEL=error \
    ./bin/agent-gm serve >"$logs/$(basename "$data").log" 2>&1 &
  # Captured at launch, and the only thing this script ever signals. A
  # `pkill -f agent-gm` would match the shell running this script.
  PID=$!
  pids+=("$PID")
  for _ in $(seq 1 150); do
    curl -fsS "${URL}/healthz" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  cat "$logs/$(basename "$data").log" >&2 || true
  die "a server on $data never became healthy"
}

stop() {
  local p="$1"
  kill "$p" 2>/dev/null || true
  wait "$p" 2>/dev/null || true
}

token_for() {
  curl -fsS -X POST "$1/v1/auth/admin-session" \
    -H 'content-type: application/json' \
    -d "$(jq -nc --arg s "$secret" '{secret:$s}')" | jq -r '.data.access_token'
}

# api <url> <token> <method> <path> [body]
#
# A failure prints the SERVER's error envelope, not curl's exit code. A drill
# that fails with "curl: (22) error 400" tells whoever is reading the CI log
# nothing about which clause broke.
api() {
  local url="$1" tok="$2" method="$3" path="$4" body="${5:-}"
  local args=(-sS -X "$method" "$url$path" -H "authorization: Bearer $tok"
              -w '\n%{http_code}')
  [ -n "$body" ] && args+=(-H 'content-type: application/json' -d "$body")
  local out code
  out="$(curl "${args[@]}")" || die "$method $path: curl failed"
  code="${out##*$'\n'}"
  out="${out%$'\n'*}"
  case "$code" in
    2*) printf '%s' "$out" ;;
    *)  die "$method $path -> HTTP $code: $out" ;;
  esac
}

# --- 1. a server with something in it -----------------------------------------

note "starting the primary server on a throwaway data directory"
start "$primary" "$key"
url_a="$URL"; pid_a="$PID"
tok_a="$(token_for "$url_a")"

# The six cookies section 3.2 requires. They are fixture strings; nothing here
# ever reaches Google, and the backend is the fake.
cookies='{"SID":"fixture-SID","HSID":"fixture-HSID","OSID":"fixture-OSID","SSID":"fixture-SSID","APISID":"fixture-APISID","SAPISID":"fixture-SAPISID"}'
pairing="$(api "$url_a" "$tok_a" POST /v1/pairing/start "$(jq -nc --argjson c "$cookies" '{cookies:$c}')" | jq -r '.data.pairing_id')"
[ -n "$pairing" ] && [ "$pairing" != null ] || die "pairing produced no pairing_id"

# The pairing is asynchronous even against the fake: start returns a
# pairing_id and the account exists only once the poll says `paired`.
acct=""
for _ in $(seq 1 100); do
  poll="$(api "$url_a" "$tok_a" GET "/v1/pairing/$pairing")"
  case "$(printf '%s' "$poll" | jq -r '.data.state')" in
    paired) acct="$(printf '%s' "$poll" | jq -r '.data.account_id')"; break ;;
    failed|expired) die "pairing ended $(printf '%s' "$poll" | jq -c '.data.state, .data.error')" ;;
  esac
  sleep 0.1
done
[ -n "$acct" ] && [ "$acct" != null ] || die "the pairing never reached 'paired'"
note "paired $acct"

# A conversation and a message, so the restore has content to be judged on
# rather than an empty schema. +1 202 555 0123 is the reserved fictional
# range; it is not dialled and cannot be.
conv="$(api "$url_a" "$tok_a" POST /v1/conversations \
  "$(jq -nc --arg a "$acct" '{account_id:$a,recipients:["+12025550123"]}')" \
  | jq -r '.data.conversation.id // .data.conversation_id // .data.id')"
[ -n "$conv" ] && [ "$conv" != null ] || die "starting a conversation produced no id"
api "$url_a" "$tok_a" POST "/v1/conversations/$conv/messages" \
  '{"text":"the drill message"}' >/dev/null
note "seeded $conv with one message"

# A settings row, so the restore is judged on the database's own mutable
# state and not only on rows the pairing wrote.
api "$url_a" "$tok_a" PATCH /v1/admin/settings '{"backup.keep":3}' >/dev/null

# A send is synchronous but INGESTION is not: the message reaches the store
# through the event stream (section 5.3), so the drill waits for the row it
# is going to be judged on rather than racing it.
before_msgs=0
for _ in $(seq 1 150); do
  before_msgs="$(api "$url_a" "$tok_a" GET "/v1/conversations/$conv/messages" | jq '.data.items | length')"
  [ "$before_msgs" -ge 1 ] && break
  sleep 0.1
done
before_convs="$(api "$url_a" "$tok_a" GET "/v1/conversations?account_id=$acct" | jq '.data.items | length')"
note "primary holds $before_convs conversation(s) and $before_msgs message(s)"
[ "$before_msgs" -ge 1 ] || die "the primary server ingested no message; there is nothing to restore"
[ "$before_convs" -ge 1 ] || die "the primary server lists no conversation; there is nothing to restore"

# --- 2. the backup ------------------------------------------------------------

snapshot="$(api "$url_a" "$tok_a" POST /v1/admin/backup '{}' | jq -r '.data.path')"
[ -f "$snapshot" ] || die "POST /v1/admin/backup reported $snapshot, which does not exist"
note "snapshot $snapshot"

# The property a restore depends on: the snapshot is a standalone database
# with no -wal sidecar (section 15.2). A file copy of a WAL-mode database
# would open perfectly well and be quietly out of date, which is exactly the
# failure this assertion exists to catch.
[ ! -e "${snapshot}-wal" ] || die "the snapshot has a -wal sidecar; it is not standalone"
sqlite3 "$snapshot" 'pragma integrity_check;' | grep -qx ok \
  || die "the snapshot fails its own integrity check"
note "snapshot is standalone and passes integrity_check"

stop "$pid_a"
note "primary server stopped"

# --- 3. restore: all three things ---------------------------------------------

cp "$snapshot" "$restored/agent-gm.sqlite3"
cp -R "$primary/sessions" "$restored/sessions"
chmod 700 "$restored/sessions"
note "restored the snapshot and sessions/ into a fresh data directory"

start "$restored" "$key"
url_b="$URL"; pid_b="$PID"
tok_b="$(token_for "$url_b")"

after_convs="$(api "$url_b" "$tok_b" GET "/v1/conversations?account_id=$acct" | jq '.data.items | length')"
after_msgs="$(api "$url_b" "$tok_b" GET "/v1/conversations/$conv/messages" | jq '.data.items | length')"
keep="$(api "$url_b" "$tok_b" GET /v1/admin/settings | jq -r '(.data.items // .data.settings)[] | select(.key=="backup.keep") | .value')"
state="$(api "$url_b" "$tok_b" GET /v1/health | jq -r --arg a "$acct" '.data.accounts[] | select(.account_id==$a) | .state')"

[ "$after_convs" = "$before_convs" ] || die "restored server has $after_convs conversation(s), primary had $before_convs"
[ "$after_msgs" = "$before_msgs" ] || die "restored server has $after_msgs message(s), primary had $before_msgs"
[ "$keep" = "3" ] || die "restored server reports backup.keep=$keep, want 3"
[ "$state" != "signed_out" ] || die "the account came back signed_out even though sessions/ and the key were restored"
note "restored: $after_convs conversation(s), $after_msgs message(s), backup.keep=$keep, $acct is '$state'"

stop "$pid_b"

# --- 4. restore: the key is missing -------------------------------------------

# Section 15.2's stated failure mode, exercised rather than asserted in prose:
# the database and sessions/ without the key leave every account signed_out
# with its history intact. The server must still START and still SERVE -- a
# refusal here would turn a recoverable state into an outage.
cp "$snapshot" "$keyless/agent-gm.sqlite3"
cp -R "$primary/sessions" "$keyless/sessions"
chmod 700 "$keyless/sessions"

start "$keyless" "$wrong_key"
url_c="$URL"; pid_c="$PID"
tok_c="$(token_for "$url_c")"

state_c="$(api "$url_c" "$tok_c" GET /v1/health | jq -r --arg a "$acct" '.data.accounts[] | select(.account_id==$a) | .state')"
msgs_c="$(api "$url_c" "$tok_c" GET "/v1/conversations/$conv/messages" | jq '.data.items | length')"
[ "$state_c" = "signed_out" ] || die "with the wrong data key $acct is '$state_c', want signed_out"
[ "$msgs_c" = "$before_msgs" ] || die "with the wrong data key the history is $msgs_c message(s), want $before_msgs intact"
note "wrong key: $acct is signed_out with $msgs_c message(s) intact, and the server still serves"

stop "$pid_c"

note "PASS -- the snapshot restored into a fresh data directory, and the"
note "       documented failure mode behaved as section 15.2 documents it"
