#!/usr/bin/env bash
# Dev-grade B1+B2 gate runner (gate/DESIGN.md §4 headline variant).
#
# Pins the SUT to exactly ONE core, drives a fixed in-flight storm from other
# cores, and measures per rep: instructions/conn (perf stat), conn/s/core +
# latency tuple (loadgen), and once per build the B1 epoll/netpoller self-CPU
# (perf record). Writes results/<ts>/ and a SUMMARY.md verdict.
#
# Run as root (needs perf_event_paranoid + taskset). Single-box loopback =>
# RATIO of SUT to baseline is the signal; absolute conn/s is loadgen/loopback-
# limited and NOT measurement-grade (stated in the summary).
set -euo pipefail
cd "$(dirname "$0")/.."   # -> gate/

CORE_SUT=${CORE_SUT:-6}
CORE_SINK=${CORE_SINK:-7,8}
CORE_LG=${CORE_LG:-9,10,11,12}
RPORT=${RPORT:-18000}
SPORT=${SPORT:-18100}
INFLIGHT=${INFLIGHT:-512}
DUR=${DUR:-8}
WARMUP=${WARMUP:-3}
REPS=${REPS:-3}
REQLEN=${REQLEN:-64}
REPLYLEN=${REPLYLEN:-256}
AUTHCPU=${AUTHCPU:-5us}
REALISTIC=${REALISTIC:-0}

BIN=$(pwd)/../bin
TS=$(date +%Y%m%d-%H%M%S)
OUT=results/$TS
mkdir -p "$OUT"

[ "$(id -u)" = 0 ] || { echo "must run as root (perf + taskset)"; exit 1; }

# perf knobs (save + restore).
OLD_PARANOID=$(cat /proc/sys/kernel/perf_event_paranoid)
OLD_KPTR=$(cat /proc/sys/kernel/kptr_restrict)
echo -1 > /proc/sys/kernel/perf_event_paranoid
echo 0  > /proc/sys/kernel/kptr_restrict

SINK_PID=""; RELAY_PID=""
cleanup() {
  [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null || true
  [ -n "$SINK_PID" ]  && kill "$SINK_PID"  2>/dev/null || true
  pkill -x relay-uring 2>/dev/null || true
  pkill -x relay-netpoll 2>/dev/null || true
  echo "$OLD_PARANOID" > /proc/sys/kernel/perf_event_paranoid
  echo "$OLD_KPTR"     > /proc/sys/kernel/kptr_restrict
}
trap cleanup EXIT

echo "building..."
( cd .. && CGO_ENABLED=0 go build -o bin/sink ./gate/cmd/sink \
  && CGO_ENABLED=0 go build -o bin/loadgen ./gate/cmd/loadgen \
  && CGO_ENABLED=0 go build -o bin/relay-uring ./gate/cmd/relay-uring \
  && CGO_ENABLED=0 go build -o bin/relay-netpoll ./gate/cmd/relay-netpoll )

# --- environment lock (DESIGN §7) ---
{
  echo "timestamp: $TS"
  echo "kernel: $(uname -r)"
  echo "go: $(cd .. && go version)"
  echo "cmdline: $(cat /proc/cmdline)"
  echo "cores: SUT=$CORE_SUT SINK=$CORE_SINK LOADGEN=$CORE_LG"
  echo "params: inflight=$INFLIGHT dur=${DUR}s warmup=${WARMUP}s reps=$REPS reqlen=$REQLEN replylen=$REPLYLEN authcpu=$AUTHCPU realistic=$REALISTIC"
  echo "loopback: yes (single-box; ratio-based, not measurement-grade absolutes)"
  echo "=== mitigations ==="
  grep -r . /sys/devices/system/cpu/vulnerabilities/ 2>/dev/null | sed 's#/sys.*/##'
  echo "=== lscpu (sockets/numa/model) ==="
  lscpu | grep -iE 'socket|numa|model name|^cpu\(s\)'
} > "$OUT/env.txt"

REAL_FLAG=""; [ "$REALISTIC" = 1 ] && REAL_FLAG="-realistic"

# CSV header.
echo "build,rep,instructions,completed,instr_per_conn,conn_per_sec,p50_us,p99_us,p999_us,audit_fail" > "$OUT/metrics.csv"

start_sink() {
  taskset -c "$CORE_SINK" env GOMAXPROCS=2 "$BIN/sink" -addr 127.0.0.1:$SPORT \
    -reqlen $REQLEN -replylen $REPLYLEN > "$OUT/sink.log" 2>&1 &
  SINK_PID=$!
  sleep 0.5
}

# run_build <name> <relay-cmd...>
run_build() {
  local name=$1; shift
  echo ">>> build=$name"
  taskset -c "$CORE_SUT" env GOMAXPROCS=1 "$@" > "$OUT/${name}.log" 2>&1 &
  RELAY_PID=$!
  sleep 1.0
  if ! kill -0 "$RELAY_PID" 2>/dev/null; then
    echo "!! $name relay failed to start"; cat "$OUT/${name}.log"; exit 1
  fi

  local rep instr completed connps p50 p99 p999 af
  for rep in $(seq 1 "$REPS"); do
    taskset -c "$CORE_LG" "$BIN/loadgen" -relay 127.0.0.1:$RPORT \
      -reqlen $REQLEN -replylen $REPLYLEN -inflight $INFLIGHT \
      -warmup ${WARMUP}s -duration ${DUR}s > "$OUT/${name}_load_${rep}.json" 2>/dev/null &
    local lg=$!
    sleep "$WARMUP"
    perf stat -x, -e instructions -p "$RELAY_PID" -- sleep "$DUR" 2> "$OUT/${name}_perf_${rep}.csv" || true
    wait "$lg"
    instr=$(grep -i instructions "$OUT/${name}_perf_${rep}.csv" | head -1 | cut -d, -f1 | tr -d ' ')
    completed=$(jq -r .completed "$OUT/${name}_load_${rep}.json")
    connps=$(jq -r .conn_per_sec "$OUT/${name}_load_${rep}.json")
    p50=$(jq -r .p50_us "$OUT/${name}_load_${rep}.json")
    p99=$(jq -r .p99_us "$OUT/${name}_load_${rep}.json")
    p999=$(jq -r .p999_us "$OUT/${name}_load_${rep}.json")
    af=$(jq -r .audit_fail "$OUT/${name}_load_${rep}.json")
    local ipc="NA"
    if [ -n "$instr" ] && [ "$completed" != "0" ]; then
      ipc=$(python3 -c "print(int($instr)/int($completed))")
    fi
    echo "$name,$rep,$instr,$completed,$ipc,$connps,$p50,$p99,$p999,$af" >> "$OUT/metrics.csv"
    echo "  rep $rep: instr/conn=$ipc conn/s=$connps p99=${p99}us audit_fail=$af"
  done

  # B1: epoll/netpoller self-CPU profile.
  taskset -c "$CORE_LG" "$BIN/loadgen" -relay 127.0.0.1:$RPORT \
    -reqlen $REQLEN -replylen $REPLYLEN -inflight $INFLIGHT \
    -warmup ${WARMUP}s -duration ${DUR}s > /dev/null 2>&1 &
  local lg=$!
  sleep "$WARMUP"
  perf record -g -p "$RELAY_PID" -o "$OUT/${name}.perfdata" -- sleep "$DUR" >/dev/null 2>&1 || true
  wait "$lg"
  perf report -i "$OUT/${name}.perfdata" --stdio --no-children 2>/dev/null > "$OUT/${name}_report.txt" || true
  # Match the SYMBOL column ($NF) only — NOT comm/dso (the comm "relay-netpoll"
  # contains the substring "netpoll" and would match every line).
  #
  # B1 is about fd REGISTRATION with epoll (epoll_ctl / netpollopen / netpollclose
  # / osq_lock) — these fire per-connection only if a data-plane fd is handed to
  # the netpoller. runtime.netpoll + do_epoll_wait are the Go scheduler's idle
  # poll, present in EVERY Go program with zero registered fds, so they are NOT
  # counted as a leak (only reported informationally).
  local epoll nsyms
  epoll=$(awk '/^[ ]+[0-9]/ { sym=$NF; pct=$1; gsub(/%/,"",pct);
    if (sym ~ /epoll_ctl/ || sym ~ /netpollopen/ || sym ~ /netpollclose/ || sym=="osq_lock") s+=pct }
    END{printf "%.3f", s+0}' "$OUT/${name}_report.txt")
  nsyms=$(awk '/^[ ]+[0-9]/ {sym=$NF; if (sym ~ /epoll_ctl/ || sym ~ /netpollopen/ || sym ~ /netpollclose/) print sym}' \
    "$OUT/${name}_report.txt" | sort -u | wc -l)
  echo "$epoll" > "$OUT/${name}_b1_epoll_selfcpu.txt"
  echo "$nsyms" > "$OUT/${name}_b1_nsyms.txt"
  echo "  B1 fd-registration self-CPU=${epoll}% across ${nsyms} distinct registration symbols"

  kill "$RELAY_PID" 2>/dev/null || true
  wait "$RELAY_PID" 2>/dev/null || true
  RELAY_PID=""
  sleep 0.5
}

start_sink
run_build netpoll "$BIN/relay-netpoll" -addr 127.0.0.1:$RPORT -sink 127.0.0.1:$SPORT \
  -reqlen $REQLEN -authcpu $AUTHCPU $REAL_FLAG
run_build uring "$BIN/relay-uring" -addr 127.0.0.1 -port $RPORT -sinkip 127.0.0.1 -sinkport $SPORT \
  -reqlen $REQLEN -replylen $REPLYLEN -authcpu $AUTHCPU $REAL_FLAG

python3 harness/summarize.py "$OUT"
echo
echo "=== results in $OUT/ ==="
cat "$OUT/SUMMARY.md"
