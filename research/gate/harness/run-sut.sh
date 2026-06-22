#!/usr/bin/env bash
# 2-box gate, SUT side (runs on THIS box). Starts ONE relay build pinned to a
# core, pointed at the REMOTE sink on the loadgen box, and measures it with perf
# while the loadgen box drives a steady storm. conn/s is read from the relay's
# -statsfile (no netpoller); instructions/conn from perf; B1 from perf record.
# Latency (p50/p99/p99.9) is client-side and comes from the loadgen box's JSON.
#
# Usage (run once per build, with load running on the loadgen box):
#   sudo env BUILD=uring   SINK=10.0.0.2:9100 bash research/gate/harness/run-sut.sh
#   sudo env BUILD=netpoll SINK=10.0.0.2:9100 bash research/gate/harness/run-sut.sh
# (use `sudo env VAR=…`; plain `sudo VAR=… bash` drops the vars.)
# Then combine with the loadgen JSONs (see DEPLOY-LOADGEN.md).
set -euo pipefail
cd "$(dirname "$0")/.."   # -> gate/

BUILD=${BUILD:?set BUILD=uring or BUILD=netpoll}
SINK=${SINK:?set SINK=<loadgen-box-ip>:<sink-port>, e.g. 10.0.0.2:9100}
SINK_IP=${SINK%%:*}; SINK_PORT=${SINK##*:}
CORE_SUT=${CORE_SUT:-6}
RPORT=${RPORT:-18000}      # relay listen port (open this inbound from the loadgen box)
DUR=${DUR:-10}
REPS=${REPS:-5}
RAMP=${RAMP:-8}            # seconds to let load reach steady state before measuring
REQLEN=${REQLEN:-64}
REPLYLEN=${REPLYLEN:-256}
AUTHCPU=${AUTHCPU:-5us}
REALISTIC=${REALISTIC:-0}
OUT=${OUT:-results/2box-$(date +%Y%m%d-%H%M%S)}
STATS=/tmp/flashrelay-sut.stats

[ "$(id -u)" = 0 ] || { echo "run as root (perf + taskset)"; exit 1; }
mkdir -p "$OUT"

OLD_P=$(cat /proc/sys/kernel/perf_event_paranoid); OLD_K=$(cat /proc/sys/kernel/kptr_restrict)
echo -1 > /proc/sys/kernel/perf_event_paranoid; echo 0 > /proc/sys/kernel/kptr_restrict
RELAY_PID=""
cleanup() {
  [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null || true
  echo "$OLD_P" > /proc/sys/kernel/perf_event_paranoid; echo "$OLD_K" > /proc/sys/kernel/kptr_restrict
}
trap cleanup EXIT

( cd ../.. && CGO_ENABLED=0 go build -o bin/relay-uring ./research/gate/cmd/relay-uring \
  && CGO_ENABLED=0 go build -o bin/relay-netpoll ./research/gate/cmd/relay-netpoll )
BIN=$(cd ../.. && pwd)/bin
REAL_FLAG=""; [ "$REALISTIC" = 1 ] && REAL_FLAG="-realistic"
rm -f "$STATS"

echo "starting $BUILD relay on :$RPORT (pinned core $CORE_SUT) -> sink $SINK"
if [ "$BUILD" = uring ]; then
  taskset -c "$CORE_SUT" env GOMAXPROCS=1 "$BIN/relay-uring" -addr 0.0.0.0 -port $RPORT \
    -sinkip "$SINK_IP" -sinkport "$SINK_PORT" -reqlen $REQLEN -replylen $REPLYLEN \
    -authcpu $AUTHCPU -statsfile "$STATS" $REAL_FLAG > "$OUT/${BUILD}.log" 2>&1 &
else
  taskset -c "$CORE_SUT" env GOMAXPROCS=1 "$BIN/relay-netpoll" -addr 0.0.0.0:$RPORT \
    -sink "$SINK" -reqlen $REQLEN -authcpu $AUTHCPU -statsfile "$STATS" $REAL_FLAG \
    > "$OUT/${BUILD}.log" 2>&1 &
fi
RELAY_PID=$!
sleep 1
kill -0 "$RELAY_PID" 2>/dev/null || { echo "relay failed:"; cat "$OUT/${BUILD}.log"; exit 1; }

echo ">>> START THE LOADGEN on the other box now (target $(hostname -I | awk '{print $1}'):$RPORT)."
echo ">>> Measuring after ${RAMP}s ramp; press Enter to start sooner."
read -t "$RAMP" -r _ || true

read_completed() { grep -o '[0-9]\+' "$STATS" 2>/dev/null || echo 0; }
echo "build,rep,instructions,completed,instr_per_conn,conn_per_sec" > "$OUT/${BUILD}_metrics.csv"
for rep in $(seq 1 "$REPS"); do
  c0=$(read_completed)
  perf stat -x, -e instructions -p "$RELAY_PID" -- sleep "$DUR" 2> "$OUT/${BUILD}_perf_${rep}.csv" || true
  c1=$(read_completed)
  instr=$(grep -i instructions "$OUT/${BUILD}_perf_${rep}.csv" | head -1 | cut -d, -f1 | tr -d ' ')
  dc=$((c1 - c0))
  ipc=NA; cps=0
  if [ "$dc" -gt 0 ]; then
    ipc=$(python3 -c "print(int($instr)/$dc)")
    cps=$(python3 -c "print($dc/$DUR)")
  fi
  echo "$BUILD,$rep,$instr,$dc,$ipc,$cps" >> "$OUT/${BUILD}_metrics.csv"
  echo "  rep $rep: instr/conn=$ipc conn/s=$cps (completed +$dc)"
done

# B1 profile.
perf record -g -p "$RELAY_PID" -o "$OUT/${BUILD}.perfdata" -- sleep "$DUR" >/dev/null 2>&1 || true
perf report -i "$OUT/${BUILD}.perfdata" --stdio --no-children 2>/dev/null > "$OUT/${BUILD}_report.txt" || true
nsyms=$(awk '/^[ ]+[0-9]/ {s=$NF; if (s ~ /epoll_ctl/ || s ~ /netpollopen/ || s ~ /netpollclose/) print s}' \
  "$OUT/${BUILD}_report.txt" | sort -u | wc -l)
self=$(awk '/^[ ]+[0-9]/ {s=$NF; p=$1; gsub(/%/,"",p);
  if (s ~ /epoll_ctl/ || s ~ /netpollopen/ || s ~ /netpollclose/ || s=="osq_lock") x+=p} END{printf "%.3f",x+0}' \
  "$OUT/${BUILD}_report.txt")
echo "$nsyms" > "$OUT/${BUILD}_b1_nsyms.txt"; echo "$self" > "$OUT/${BUILD}_b1_selfcpu.txt"
echo "  B1: $BUILD fd-registration symbols=$nsyms (self-CPU ${self}%)  [0 = epoll eliminated]"
echo
echo "=== $BUILD done. results in $OUT/ ==="
echo "Pair with the loadgen-box JSON for latency. After running BOTH builds, run:"
echo "  python3 research/gate/harness/combine-2box.py $OUT <uring_loadgen.json> <netpoll_loadgen.json>"
