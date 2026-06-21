#!/usr/bin/env bash
# optimizer/gate-suite.sh — the PROMOTION gate suite. The inner loop maximizes ONE
# objective (instr_pc OR bytes_per_cpu); this suite is what makes us "optimize on
# everything" without a magic weighted score: a candidate that beats the champion
# on the objective must ALSO pass every cross-workload no-regress gate here before
# it is promoted. Runs only on a champion-beating candidate (the expensive checks
# stay off the per-iteration hot loop).
#
# Gates (any failure => not promotable):
#   1. flood-survive : a junk connect-flood must NOT wedge the relay (the multishot
#                      lesson — a closed-loop CPU win that deadlocks under flood is
#                      worthless). Relay must stay alive, non-D-state, responsive.
#   2. rss-slope     : held-connection RSS per conn must stay under RSS_SLOPE_CEIL
#                      (no per-conn memory leak at scale).
#   3. no-regress    : the OTHER objective (the one we're NOT maximizing) must not
#                      drop more than REGRESS_TOL vs the champion's stored value —
#                      measured by simply re-running score.sh with that objective.
#
# Prints JSON verdict on stdout: {"pass":1|0,"reason":...,"flood_ok":..,
# "rss_per_conn":..,"cross_obj":..,"cross_score":..,"cross_base":..}. Logs to stderr.
# Env: CROSS_BASELINE (champion's other-objective score; "" => skip gate 3).
set -uo pipefail
cd "$(dirname "$0")/.."
. optimizer/config
export PATH="$PATH:/usr/local/go/bin" CGO_ENABLED=0
log(){ echo "[gate-suite] $*" >&2; }
emit(){ echo "$1"; exit 0; }

OLD_P=$(cat /proc/sys/kernel/perf_event_paranoid 2>/dev/null || echo 2)
echo -1 > /proc/sys/kernel/perf_event_paranoid 2>/dev/null || true

RELAY_PID=""; SINK_PID=""
cleanup(){
  [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null || true
  [ -n "$SINK_PID" ]  && kill "$SINK_PID"  2>/dev/null || true
  pkill -x relay-uring 2>/dev/null || true; pkill -x sink 2>/dev/null || true
  pkill -x loadgen 2>/dev/null || true; pkill -x holdgen 2>/dev/null || true
  echo "$OLD_P" > /proc/sys/kernel/perf_event_paranoid 2>/dev/null || true
}
trap cleanup EXIT
pkill -x relay-uring 2>/dev/null || true; pkill -x sink 2>/dev/null || true; sleep 0.5

# verified-bind ports + reserve (same hardening as score.sh)
PORTS=$(python3 optimizer/pick-ports.py "${PORT_BASE:-43000}" 4 100) \
  || emit '{"pass":0,"reason":"no_ports"}'
read -r FRPORT FSPORT HRPORT HEPORT <<<"$PORTS"
RES=$(cat /proc/sys/net/ipv4/ip_local_reserved_ports 2>/dev/null || echo "")
for p in $FRPORT $FSPORT $HRPORT $HEPORT; do case ",$RES," in *",$p,"*) ;; *) RES="${RES:+$RES,}$p";; esac; done
sysctl -w net.ipv4.ip_local_reserved_ports="$RES" >/dev/null 2>&1 || true

CGO_ENABLED=0 go build -o bin/relay-uring ./gate/cmd/relay-uring 2>/dev/null || emit '{"pass":0,"reason":"build_fail"}'
CGO_ENABLED=0 go build -o bin/sink ./gate/cmd/sink 2>/dev/null
CGO_ENABLED=0 go build -o bin/loadgen ./gate/cmd/loadgen 2>/dev/null
CGO_ENABLED=0 go build -o bin/holdgen ./gate/cmd/holdgen 2>/dev/null
BIN=$(pwd)/bin
rss_of(){ awk '/VmRSS/{print $2*1024}' /proc/"$1"/status 2>/dev/null || echo 0; }

# ---- gate 1: flood-survive -------------------------------------------------
log "gate 1: flood-survive ($FLOOD_CONNS conns, ${FLOOD_SECS}s junk=93%)"
taskset -c "$SINK_CPU" "$BIN/sink" -addr 127.0.0.1:$FSPORT -reqlen $REQLEN -replylen $REPLYLEN >/tmp/gs-sink.log 2>&1 & SINK_PID=$!
sleep 0.4
taskset -c "$SUT_CPU" env GOMAXPROCS=1 "$BIN/relay-uring" -addr 127.0.0.1 -port $FRPORT \
  -sinkip 127.0.0.1 -sinkport $FSPORT -reqlen $REQLEN -replylen $REPLYLEN -authcpu $AUTHCPU >/tmp/gs-relay.log 2>&1 & RELAY_PID=$!
sleep 1
kill -0 "$RELAY_PID" 2>/dev/null || emit '{"pass":0,"reason":"flood_start_fail"}'
taskset -c "$LG_CPUS" "$BIN/loadgen" -relay 127.0.0.1:$FRPORT -reqlen $REQLEN -replylen $REPLYLEN \
  -inflight $FLOOD_CONNS -junkpct 93 -warmup 1s -duration ${FLOOD_SECS}s >/tmp/gs-flood.json 2>/dev/null || true
sleep 1
# survival: alive, NOT in D (uninterruptible — the wedge), and answers a fresh conn.
if ! kill -0 "$RELAY_PID" 2>/dev/null; then emit '{"pass":0,"reason":"flood_died"}'; fi
st=$(ps -o stat= -p "$RELAY_PID" 2>/dev/null | tr -d ' ')
case "$st" in *D*) log "relay in D-state ($st) -> WEDGED"; emit '{"pass":0,"reason":"flood_wedge"}';; esac
"$BIN/loadgen" -relay 127.0.0.1:$FRPORT -reqlen $REQLEN -replylen $REPLYLEN -inflight 4 -warmup 0s -duration 2s >/tmp/gs-after.json 2>/dev/null || true
resp=$(python3 -c "import json;print(json.load(open('/tmp/gs-after.json'))['completed'])" 2>/dev/null || echo 0)
[ "${resp:-0}" -ge 1 ] || emit '{"pass":0,"reason":"flood_unresponsive"}'
kill "$RELAY_PID" "$SINK_PID" 2>/dev/null; RELAY_PID=""; SINK_PID=""; sleep "$SETTLE"
log "gate 1 PASS (responsive after flood: $resp)"

# ---- gate 2: rss-slope -----------------------------------------------------
log "gate 2: rss-slope ($HOLD_CONNS held conns, ceil ${RSS_SLOPE_CEIL} B/conn)"
taskset -c "$SINK_CPU" "$BIN/sink" -addr 127.0.0.1:$HEPORT -echo >/tmp/gs-echo.log 2>&1 & SINK_PID=$!
sleep 0.4
taskset -c "$SUT_CPU" env GOMAXPROCS=1 "$BIN/relay-uring" -addr 127.0.0.1 -port $HRPORT \
  -sinkip 127.0.0.1 -sinkport $HEPORT -duplex -maxconns $((HOLD_CONNS*2)) >/tmp/gs-hrelay.log 2>&1 & RELAY_PID=$!
sleep 1
kill -0 "$RELAY_PID" 2>/dev/null || emit '{"pass":0,"reason":"rss_start_fail"}'
rss_idle=$(rss_of "$RELAY_PID")
taskset -c "$LG_CPUS" "$BIN/holdgen" -relay 127.0.0.1:$HRPORT -n $HOLD_CONNS -reqlen $REQLEN \
  -ramp 8s -hold 10s -keepalive 2s >/tmp/gs-hold.log 2>&1 & HG=$!
sleep 12   # past ramp, mid-hold
rss_load=$(rss_of "$RELAY_PID")
estab=$(grep -oE 'established=[0-9]+' /tmp/gs-hold.log 2>/dev/null | tail -1 | grep -oE '[0-9]+' || echo 0)
wait "$HG" 2>/dev/null || true
kill "$RELAY_PID" "$SINK_PID" 2>/dev/null; RELAY_PID=""; SINK_PID=""; sleep "$SETTLE"
conns=${estab:-0}; [ "$conns" -lt 1 ] && conns=$HOLD_CONNS
RSS_PC=$(python3 -c "print(int(max(0,($rss_load-$rss_idle))/$conns))" 2>/dev/null || echo 999999)
log "gate 2: idle=${rss_idle}B load=${rss_load}B conns=$conns -> ${RSS_PC} B/conn"
if [ "$RSS_PC" -gt "$RSS_SLOPE_CEIL" ]; then emit "{\"pass\":0,\"reason\":\"rss_leak\",\"rss_per_conn\":$RSS_PC}"; fi
log "gate 2 PASS (${RSS_PC} B/conn <= $RSS_SLOPE_CEIL)"

# ---- gate 3: cross-objective no-regress ------------------------------------
OTHER=instr_pc; [ "$OBJECTIVE" = instr_pc ] && OTHER=bytes_per_cpu
CROSS_SCORE=0; CROSS_BASE="${CROSS_BASELINE:-}"
if [ -z "$CROSS_BASE" ]; then
  log "gate 3: no CROSS_BASELINE -> skip (will be recorded for next time)"
else
  log "gate 3: no-regress of '$OTHER' (champion=$CROSS_BASE, tol=$REGRESS_TOL)"
  CROSS_SCORE=$(OBJECTIVE=$OTHER PORT_BASE=44000 bash optimizer/score.sh 2>>/tmp/gs-cross.log \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('score',0))" 2>/dev/null || echo 0)
  floor=$(awk -v b="$CROSS_BASE" -v t="$REGRESS_TOL" 'BEGIN{printf "%.4f",b*(1-t)}')
  log "gate 3: $OTHER candidate=$CROSS_SCORE floor=$floor"
  if awk -v s="$CROSS_SCORE" -v f="$floor" 'BEGIN{exit !(s < f)}'; then
    emit "{\"pass\":0,\"reason\":\"cross_regress\",\"cross_obj\":\"$OTHER\",\"cross_score\":$CROSS_SCORE,\"cross_base\":$CROSS_BASE}"
  fi
fi

emit "{\"pass\":1,\"reason\":\"ok\",\"flood_ok\":1,\"rss_per_conn\":${RSS_PC},\"cross_obj\":\"$OTHER\",\"cross_score\":${CROSS_SCORE:-0},\"cross_base\":\"${CROSS_BASE}\"}"
