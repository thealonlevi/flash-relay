#!/usr/bin/env bash
# flood.sh — connect-flood CPU-profile measurement (models the ISP incident:
# a 93%-junk zero-byte connect-flood). For each relay build it pins the relay to
# ONE core, drives the flood, and captures WHERE the CPU goes (scheduler / GC /
# netpoll / syscall-transition / io_uring / kernel-TCP / user) plus conn/s/core
# and CPU cores consumed. Answers: "after flash-relay, what consumes the CPU?"
#
# Loopback (the public-IP path throttles new-conn rate); ratio + profile shape are
# the signal, not absolute conn/s. Run as root (perf + taskset). Builds relay-uring
# from the working tree (= the multishot champion on optimizer-run).
set -uo pipefail
cd "$(dirname "$0")/.."   # -> gate/
export PATH="$PATH:/usr/local/go/bin"

CORE_SUT=${CORE_SUT:-6}
CORE_SINK=${CORE_SINK:-7}
CORE_LG=${CORE_LG:-9,10,11,12}
RPORT=${RPORT:-18000}; SPORT=${SPORT:-18100}
REQLEN=${REQLEN:-64}; REPLYLEN=${REPLYLEN:-256}
JUNK=${JUNK:-93}            # % zero-byte junk (incident was 93%)
INFLIGHT=${INFLIGHT:-2000}  # high enough to saturate the 1-core relay
DUR=${DUR:-15}; WARMUP=${WARMUP:-4}
OUT=${OUT:-results/flood-$(date +%Y%m%d-%H%M%S)}
CLK=$(getconf CLK_TCK)

[ "$(id -u)" = 0 ] || { echo "run as root (perf + taskset)"; exit 1; }
mkdir -p "$OUT"
OLD_P=$(cat /proc/sys/kernel/perf_event_paranoid); OLD_K=$(cat /proc/sys/kernel/kptr_restrict)
echo -1 > /proc/sys/kernel/perf_event_paranoid; echo 0 > /proc/sys/kernel/kptr_restrict
RES=$(cat /proc/sys/net/ipv4/ip_local_reserved_ports 2>/dev/null||echo "")
for p in $RPORT $SPORT; do case ",$RES," in *",$p,"*) ;; *) RES="${RES:+$RES,}$p";; esac; done
sysctl -w net.ipv4.ip_local_reserved_ports="$RES" >/dev/null 2>&1||true

RELAY_PID=""; SINK_PID=""
cleanup(){ [ -n "$RELAY_PID" ]&&kill "$RELAY_PID" 2>/dev/null||true; [ -n "$SINK_PID" ]&&kill "$SINK_PID" 2>/dev/null||true
  pkill -x relay-uring 2>/dev/null||true; pkill -x relay-netpoll 2>/dev/null||true; pkill -x sink 2>/dev/null||true; pkill -x loadgen 2>/dev/null||true
  echo "$OLD_P">/proc/sys/kernel/perf_event_paranoid; echo "$OLD_K">/proc/sys/kernel/kptr_restrict; }
trap cleanup EXIT
for n in relay-uring relay-netpoll sink loadgen; do pkill -x "$n" 2>/dev/null; done; sleep 1

( cd .. && CGO_ENABLED=0 go build -o bin/relay-uring ./gate/cmd/relay-uring \
  && CGO_ENABLED=0 go build -o bin/relay-netpoll ./gate/cmd/relay-netpoll \
  && CGO_ENABLED=0 go build -o bin/sink ./gate/cmd/sink \
  && CGO_ENABLED=0 go build -o bin/loadgen ./gate/cmd/loadgen )
BIN=$(cd .. && pwd)/bin
cpu_ticks(){ awk '{print $14+$15}' "/proc/$1/stat" 2>/dev/null||echo 0; }

run_build(){
  local name=$1; shift
  echo ">>> $name"
  taskset -c "$CORE_SINK" "$BIN/sink" -addr 127.0.0.1:$SPORT -reqlen $REQLEN -replylen $REPLYLEN >"$OUT/${name}.sink.log" 2>&1 & SINK_PID=$!
  sleep 0.4
  taskset -c "$CORE_SUT" env GOMAXPROCS=1 "$@" >"$OUT/${name}.log" 2>&1 & RELAY_PID=$!
  sleep 1
  kill -0 "$RELAY_PID" 2>/dev/null || { echo "!! $name failed"; cat "$OUT/${name}.log"; return 1; }
  # warmup flood
  taskset -c "$CORE_LG" "$BIN/loadgen" -relay 127.0.0.1:$RPORT -reqlen $REQLEN -replylen $REPLYLEN \
    -inflight $INFLIGHT -junkpct $JUNK -warmup 1s -duration ${WARMUP}s >/dev/null 2>&1 || true
  # measured window: loadgen flood (bg) + perf record + cpu-ticks delta
  taskset -c "$CORE_LG" "$BIN/loadgen" -relay 127.0.0.1:$RPORT -reqlen $REQLEN -replylen $REPLYLEN \
    -inflight $INFLIGHT -junkpct $JUNK -warmup 1s -duration $((DUR+3))s >"$OUT/${name}.load.json" 2>/dev/null & local lg=$!
  sleep 1
  local t0; t0=$(cpu_ticks "$RELAY_PID")
  perf record -g -e cpu-clock -F 997 -p "$RELAY_PID" -o "$OUT/${name}.data" -- sleep "$DUR" >/dev/null 2>&1 || true
  local t1; t1=$(cpu_ticks "$RELAY_PID")
  wait "$lg"
  perf report -i "$OUT/${name}.data" --stdio --no-children 2>/dev/null > "$OUT/${name}.report.txt" || true
  local cores; cores=$(awk -v a="$t0" -v b="$t1" -v c="$CLK" -v d="$DUR" 'BEGIN{printf "%.3f",(b-a)/c/d}')
  python3 harness/flood-profile.py "$OUT/${name}.report.txt" "$name" "$OUT/${name}.load.json" "$cores" "$DUR" | tee "$OUT/${name}.summary.txt"
  kill "$RELAY_PID" "$SINK_PID" 2>/dev/null; RELAY_PID=""; SINK_PID=""; sleep 0.5
}

{
echo "FLOOD MEASUREMENT  junk=${JUNK}%  inflight=$INFLIGHT  dur=${DUR}s  1 core (loopback)"
echo "kernel=$(uname -r)  $(date -u +%FT%TZ)"
echo
run_build netpoll "$BIN/relay-netpoll" -addr 127.0.0.1:$RPORT -sink 127.0.0.1:$SPORT -reqlen $REQLEN
echo
run_build uring "$BIN/relay-uring" -addr 127.0.0.1 -port $RPORT -sinkip 127.0.0.1 -sinkport $SPORT -reqlen $REQLEN -replylen $REPLYLEN
} 2>&1 | tee "$OUT/RESULT.txt"
echo
echo "=== results in $OUT/ ==="
