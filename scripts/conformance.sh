#!/usr/bin/env bash
# The MCP conformance run of spec section 8.4.
#
# Builds the server, starts it on a free loopback port with a throwaway data
# directory and admin secret, and hands off to scripts/conformance-harness.mjs,
# which mints a token THROUGH THE WHOLE OAUTH FLOW, proxies the bearer in, and
# runs the pinned @modelcontextprotocol/conformance package against the
# baseline in both directions.
#
# The backend is the fake of section 13.1, with both AGENT_GM_BACKEND=fake and
# AGENT_GM_ALLOW_FAKE=1, because the server refuses to start with only the
# first. No phone is involved and nothing is sent.
set -euo pipefail

cd "$(dirname "$0")/.."

# The suite is run at the spec revision it actually knows. Bumping this pin is
# a deliberate act with a baseline review, not a floating "latest" that turns
# a green CI line red on somebody else's release day.
PACKAGE="${AGENT_GM_CONFORMANCE_PACKAGE:-@modelcontextprotocol/conformance@0.1.16}"
BASELINE="${AGENT_GM_CONFORMANCE_BASELINE:-scripts/mcp-conformance-baseline.yaml}"

echo "conformance: building" >&2
go build -trimpath -o bin/agent-gm ./cmd/agent-gm

port="$(node -e 'const s=require("net").createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close();});')"
data="$(mktemp -d)"
secret="$(openssl rand -hex 32)"
key="$(openssl rand -hex 32)"
url="http://127.0.0.1:${port}"

cleanup() {
  if [[ -n "${server_pid:-}" ]] && kill -0 "$server_pid" 2>/dev/null; then
    # By PID, never by name: `pkill -f agent-gm` matches the shell that is
    # running this script.
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$data"
}
trap cleanup EXIT

echo "conformance: starting the server on ${url}" >&2
AGENT_GM_DATA_DIR="$data" \
AGENT_GM_DATA_KEY="$key" \
AGENT_GM_ADMIN_SECRET="$secret" \
AGENT_GM_PUBLIC_URL="$url" \
AGENT_GM_LISTEN_ADDR="127.0.0.1:${port}" \
AGENT_GM_BACKEND=fake \
AGENT_GM_ALLOW_FAKE=1 \
AGENT_GM_LOG_LEVEL=error \
  ./bin/agent-gm serve &
server_pid=$!

for _ in $(seq 1 100); do
  if curl -fsS "${url}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
if ! curl -fsS "${url}/healthz" >/dev/null 2>&1; then
  echo "conformance: the server never became healthy" >&2
  exit 1
fi

AGENT_GM_CONFORMANCE_SERVER="$url" \
AGENT_GM_ADMIN_SECRET="$secret" \
AGENT_GM_CONFORMANCE_PACKAGE="$PACKAGE" \
AGENT_GM_CONFORMANCE_BASELINE="$BASELINE" \
  node scripts/conformance-harness.mjs
