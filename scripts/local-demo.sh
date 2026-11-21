#!/usr/bin/env bash
# The whole flow on your machine, no testnet funds needed:
# Postgres -> Hardhat node (1 block/s) -> mock tUSD8 + AgentPayRouter + mandate -> settler
# (virtual at 10 blocks deep, verified at 30, to mimic QDay's tiers) -> demo merchant -> agent.
#
#   scripts/local-demo.sh          run the agent scenarios, then stop everything
#   scripts/local-demo.sh --keep   leave it running: dashboard http://localhost:8080, merchant http://localhost:8787
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD
LOGS=$ROOT/.demo
mkdir -p "$LOGS"
SETTLER_PORT=${SETTLER_PORT:-8080}
MERCHANT_PORT=${MERCHANT_PORT:-8787}
SECRET=local-demo-webhook-secret
PIDS=()
cleanup() {
  for p in "${PIDS[@]}"; do
    # On Windows (Git Bash) kill the native process tree too; plain kill can leave it running.
    if [ -r "/proc/$p/winpid" ] && command -v taskkill >/dev/null; then
      taskkill //F //T //PID "$(cat "/proc/$p/winpid")" >/dev/null 2>&1 || true
    fi
    kill "$p" 2>/dev/null || true
  done
}
trap cleanup EXIT

wait_for() { # url, what
  for _ in $(seq 1 120); do
    if curl -sf -o /dev/null "$1" 2>/dev/null || curl -sf -o /dev/null -X POST -H 'content-type: application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' "$1" 2>/dev/null; then return 0; fi
    sleep 0.5
  done
  echo "$2 did not start; see $LOGS" >&2; exit 1
}

[ -d contracts/node_modules ] || (cd contracts && npm ci)
[ -d demo/node_modules ] || (cd demo && npm ci)

for port in 8545 "$SETTLER_PORT" "$MERCHANT_PORT"; do
  if curl -s -m 1 -o /dev/null "http://127.0.0.1:$port" 2>/dev/null; then
    echo "port $port is already in use (an earlier demo still running?); stop it first" >&2; exit 1
  fi
done

echo "› postgres"
docker compose up -d --wait db >/dev/null
docker compose exec -T db psql -qU postgres -c "DROP DATABASE IF EXISTS settler_local" -c "CREATE DATABASE settler_local" >/dev/null

echo "› hardhat node"
(cd contracts && exec node node_modules/hardhat/internal/cli/cli.js node >"$LOGS/node.log" 2>&1) & PIDS+=($!)
wait_for http://127.0.0.1:8545 "hardhat node"

echo "› deploy mock tUSD8 + router, fund owner, grant mandate"
(cd contracts && npx hardhat run scripts/local.js --network localhost)

echo "› settler"
(cd settler && go build -o "$LOGS/settler$(go env GOEXE)" ./cmd/settler)
(
  set -a; . "$ROOT/.env.local"; set +a
  export DATABASE_URL="postgres://postgres:postgres@localhost:5441/settler_local?sslmode=disable"
  export HTTP_ADDR="127.0.0.1:$SETTLER_PORT" LOG_FORMAT=text POLL_INTERVAL=500ms
  export WEBHOOK_URL="http://127.0.0.1:$MERCHANT_PORT/webhooks/settle" WEBHOOK_SECRET=$SECRET
  exec "$LOGS/settler$(go env GOEXE)" >"$LOGS/settler.log" 2>&1
) & PIDS+=($!)
wait_for "http://127.0.0.1:$SETTLER_PORT/healthz" settler
sleep 1
if grep -q "settler stopped" "$LOGS/settler.log"; then tail -3 "$LOGS/settler.log" >&2; exit 1; fi

echo "› merchant"
(rm -f "$LOGS"/merchant.db*; ENV_FILE="$ROOT/.env.local" MERCHANT_DB="$LOGS/merchant.db" SETTLER_URL="http://127.0.0.1:$SETTLER_PORT" WEBHOOK_SECRET=$SECRET MERCHANT_PORT=$MERCHANT_PORT \
  exec node demo/merchant.js >"$LOGS/merchant.log" 2>&1) & PIDS+=($!)
wait_for "http://127.0.0.1:$MERCHANT_PORT/healthz" merchant

echo "› agent"
status=0
ENV_FILE="$ROOT/.env.local" MERCHANT_URL="http://127.0.0.1:$MERCHANT_PORT" node demo/agent.js scenarios || status=$?

echo
echo "› MCP server (the wallet tools an LLM agent gets), driven by a real MCP client"
ENV_FILE="$ROOT/.env.local" MERCHANT_URL="http://127.0.0.1:$MERCHANT_PORT" node demo/test/mcp-smoke.mjs 2>/dev/null || status=$?

echo
echo "dashboard  http://localhost:$SETTLER_PORT    owner console  http://localhost:$SETTLER_PORT/console"
echo "merchant   http://localhost:$MERCHANT_PORT    logs           $LOGS/"
if [ "${1:-}" = "--keep" ]; then
  echo "running; Ctrl-C to stop"
  wait
fi
exit $status
