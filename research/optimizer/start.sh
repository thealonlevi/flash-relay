#!/usr/bin/env bash
# research/optimizer/start.sh — launch the optimizer loop detached (survives the launching
# shell). Idempotent. Needs the Bash allow-rule in .claude/settings.local.json if
# launched from a Claude session (the loop spawns nested `claude -p`).
set -uo pipefail
cd "$(dirname "$0")/../.."
. research/optimizer/config
mkdir -p "$RESULTS_DIR/loop-logs"

# PID-file check (NOT pgrep -f — that self-matches any caller whose command line
# contains 'research/optimizer/loop.sh', e.g. a supervising shell).
PIDF="$RESULTS_DIR/loop.pid"
if [ -f "$PIDF" ] && kill -0 "$(cat "$PIDF" 2>/dev/null)" 2>/dev/null; then
  echo "already running (pid $(cat "$PIDF")). stop: touch $RESULTS_DIR/STOP"
  exit 0
fi
rm -f "$PIDF"
clickhouse-client --multiquery < research/optimizer/schema.sql 2>/dev/null || echo "warn: clickhouse schema not applied (continuing; CH logging best-effort)"

TS=$(date +%Y%m%d-%H%M%S); OUT="$RESULTS_DIR/loop-logs/loop.$TS.out"
nohup env MAX_ITERS="${MAX_ITERS:-1000000}" bash research/optimizer/loop.sh > "$OUT" 2>&1 &
PID=$!
ln -sfn "$(basename "$OUT")" "$RESULTS_DIR/loop-logs/loop.out"
sleep 1
echo "optimizer started  pid=$PID"
echo "  log:   $OUT  (symlink: $RESULTS_DIR/loop-logs/loop.out)"
echo "  iters: $RESULTS_DIR/iterations.csv  |  champion: $BEST"
echo "  stop:  touch $RESULTS_DIR/STOP   (graceful)  |  kill $PID  (hard)"
