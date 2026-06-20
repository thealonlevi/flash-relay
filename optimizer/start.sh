#!/usr/bin/env bash
# optimizer/start.sh — launch the optimizer loop detached (survives the launching
# shell). Idempotent. Needs the Bash allow-rule in .claude/settings.local.json if
# launched from a Claude session (the loop spawns nested `claude -p`).
set -uo pipefail
cd "$(dirname "$0")/.."
. optimizer/config
mkdir -p "$RESULTS_DIR/loop-logs"

if pgrep -f 'optimizer/loop.sh' >/dev/null; then
  echo "already running (pid $(pgrep -f optimizer/loop.sh | tr '\n' ' ')). stop: touch $RESULTS_DIR/STOP"
  exit 0
fi
clickhouse-client --multiquery < optimizer/schema.sql 2>/dev/null || echo "warn: clickhouse schema not applied (continuing; CH logging best-effort)"

TS=$(date +%Y%m%d-%H%M%S); OUT="$RESULTS_DIR/loop-logs/loop.$TS.out"
nohup env MAX_ITERS="${MAX_ITERS:-1000000}" bash optimizer/loop.sh > "$OUT" 2>&1 &
PID=$!
ln -sfn "$(basename "$OUT")" "$RESULTS_DIR/loop-logs/loop.out"
sleep 1
echo "optimizer started  pid=$PID"
echo "  log:   $OUT  (symlink: $RESULTS_DIR/loop-logs/loop.out)"
echo "  iters: $RESULTS_DIR/iterations.csv  |  champion: $BEST"
echo "  stop:  touch $RESULTS_DIR/STOP   (graceful)  |  kill $PID  (hard)"
