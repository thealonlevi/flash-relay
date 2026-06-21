#!/usr/bin/env bash
# optimizer/score.sh — the FIXED referee. Scores the CURRENT working-tree io_uring
# relay (gate/cmd/relay-uring + gate/internal/uring) on a CPU-bound loopback churn
# workload. The optimizer calls this but never edits it. Prints score JSON on
# stdout; logs to stderr.
#
# SCORE = 1e9 / mean(instructions_per_connection)   (higher = fewer instr/conn).
# instr/conn is frequency-independent — riptide's exact question.
#
# ANTI-CHEAT GATE (any failure => score 0): the optimizer cannot win by cheating.
#   - ring unit tests pass            (didn't break the ring primitives)
#   - byte audit clean                (didn't truncate/corrupt the relay)
#   - sink actually served ~all conns (TWO-FD: didn't short-circuit the upstream dial)
#   - drop rate under ceiling         (didn't shed load)
#   - duplex echo smoke passes        (didn't break long-lived bidirectional relay)
# (Editing the LOCKED contract — hook/proto/sinksrv/harness — is prevented by the
#  loop, which reverts any change outside ALLOWED_PATHS.)
set -uo pipefail
cd "$(dirname "$0")/.."
. optimizer/config
export PATH="$PATH:/usr/local/go/bin"
log(){ echo "[score] $*" >&2; }
emit(){ echo "$1"; exit 0; }   # JSON to stdout, always exit 0 (loop reads score)

OLD_P=$(cat /proc/sys/kernel/perf_event_paranoid 2>/dev/null || echo 2)
OLD_K=$(cat /proc/sys/kernel/kptr_restrict 2>/dev/null || echo 1)
echo -1 > /proc/sys/kernel/perf_event_paranoid 2>/dev/null || true
echo 0  > /proc/sys/kernel/kptr_restrict 2>/dev/null || true
# Verified-bind port selection. A relay that wedges into D-state holds its ports
# in CLOSE-WAIT forever (invisible to `ss -ltn`), so the fixed config ports can be
# silently un-bindable -> EADDRINUSE that looks like a relay crash. Pick ports we
# just PROVED bindable, then reserve them so no ephemeral can grab them before the
# relay binds. (Picking unoccupied ports also avoids joining a poisoned
# SO_REUSEPORT group, which is itself a wedge trigger.) Override the config ports.
PORTS=$(python3 optimizer/pick-ports.py "${PORT_BASE:-40000}" 6 100) \
  || { log "no bindable ports (box port space exhausted -> reboot)"; emit '{"score":0,"reason":"no_ports"}'; }
read -r RPORT SPORT DPORT EPORT TPORT TEPORT <<<"$PORTS"
log "ports: relay=$RPORT sink=$SPORT duplex=$DPORT/$EPORT bulk=$TPORT/$TEPORT"
RES=$(cat /proc/sys/net/ipv4/ip_local_reserved_ports 2>/dev/null || echo "")
for p in $RPORT $SPORT $DPORT $EPORT $TPORT $TEPORT; do case ",$RES," in *",$p,"*) ;; *) RES="${RES:+$RES,}$p";; esac; done
sysctl -w net.ipv4.ip_local_reserved_ports="$RES" >/dev/null 2>&1 || true

RELAY_PID=""; SINK_PID=""
cleanup(){
  [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null || true
  [ -n "$SINK_PID" ]  && kill "$SINK_PID"  2>/dev/null || true
  pkill -x relay-uring 2>/dev/null || true; pkill -x sink 2>/dev/null || true; pkill -x loadgen 2>/dev/null || true
  echo "$OLD_P" > /proc/sys/kernel/perf_event_paranoid 2>/dev/null || true
  echo "$OLD_K" > /proc/sys/kernel/kptr_restrict 2>/dev/null || true
}
trap cleanup EXIT
pkill -x relay-uring 2>/dev/null || true; pkill -x sink 2>/dev/null || true; pkill -x loadgen 2>/dev/null || true; sleep 0.5

# --- gate 0: ring unit tests (correctness of the mutated primitives) --------
if ! go test ./internal/uring/ >/tmp/opt-gotest.log 2>&1; then
  log "ring unit tests FAILED -> score 0"; emit '{"score":0,"reason":"ring_test_fail"}'
fi
# --- gate 1: build ----------------------------------------------------------
if ! CGO_ENABLED=0 go build -o bin/relay-uring ./gate/cmd/relay-uring 2>/tmp/opt-build.log; then
  log "build FAILED -> score 0"; emit '{"score":0,"reason":"build_fail"}'
fi
CGO_ENABLED=0 go build -o bin/sink ./gate/cmd/sink 2>/dev/null
CGO_ENABLED=0 go build -o bin/loadgen ./gate/cmd/loadgen 2>/dev/null
BIN=$(pwd)/bin
RSTAT=/tmp/opt-relay.stats; SSTAT=/tmp/opt-sink.stats; rm -f "$RSTAT" "$SSTAT"

# --- churn arm: sink + relay pinned ----------------------------------------
taskset -c "$SINK_CPU" "$BIN/sink" -addr 127.0.0.1:$SPORT -reqlen $REQLEN -replylen $REPLYLEN \
  -statsfile "$SSTAT" >/tmp/opt-sink.log 2>&1 & SINK_PID=$!
sleep 0.4
taskset -c "$SUT_CPU" env GOMAXPROCS=1 "$BIN/relay-uring" -addr 127.0.0.1 -port $RPORT \
  -sinkip 127.0.0.1 -sinkport $SPORT -reqlen $REQLEN -replylen $REPLYLEN -authcpu $AUTHCPU \
  -statsfile "$RSTAT" >/tmp/opt-relay.log 2>&1 & RELAY_PID=$!
sleep 1
kill -0 "$RELAY_PID" 2>/dev/null || { log "relay didn't start"; emit '{"score":0,"reason":"start_fail"}'; }

# smoke
"$BIN/loadgen" -relay 127.0.0.1:$RPORT -reqlen $REQLEN -replylen $REPLYLEN -inflight 32 \
  -warmup 1s -duration 2s >/tmp/opt-smoke.json 2>/dev/null
sc=$(python3 -c "import json;print(json.load(open('/tmp/opt-smoke.json'))['completed'])" 2>/dev/null || echo 0)
[ "${sc:-0}" -ge 1 ] || { log "smoke failed (completed=$sc)"; emit '{"score":0,"reason":"smoke_fail"}'; }

rdstat(){ grep -o '[0-9]\+' "$1" 2>/dev/null | head -1 || echo 0; }
declare -a IPC=() CPS=()
GATE_OK=1; REASON="ok"
for rep in $(seq 1 "$N"); do
  c0=$(rdstat "$RSTAT"); s0=$(rdstat "$SSTAT")
  taskset -c "$LG_CPUS" "$BIN/loadgen" -relay 127.0.0.1:$RPORT -reqlen $REQLEN -replylen $REPLYLEN \
    -inflight $MEASURE_CONNS -warmup ${WARMUP}s -duration ${MEASURE}s >/tmp/opt-load.$rep.json 2>/dev/null &
  LG=$!
  sleep "$WARMUP"
  perf stat -x, -e instructions -p "$RELAY_PID" -- sleep "$MEASURE" 2>/tmp/opt-perf.$rep.csv || true
  wait "$LG"
  c1=$(rdstat "$RSTAT"); s1=$(rdstat "$SSTAT")
  instr=$(grep -i instructions /tmp/opt-perf.$rep.csv | head -1 | cut -d, -f1 | tr -d ' ')
  read -r af errs lgcomp < <(python3 -c "import json;d=json.load(open('/tmp/opt-load.$rep.json'));print(d['audit_fail'],d['errors'],d['completed'])" 2>/dev/null || echo "1 1 0")
  # deltas measured on the relay/sink themselves (aligned to the perf window)
  res=$(REP_C0=$c0 REP_C1=$c1 REP_S0=$s0 REP_S1=$s1 INSTR="${instr:-0}" AF="$af" ERRS="$errs" \
        TOL="$TWO_FD_TOL" DROP="$DROP_CEILING" MEASURE="$MEASURE" python3 - <<'PY'
import os
e=os.environ
comp=int(e["REP_C1"])-int(e["REP_C0"]); served=int(e["REP_S1"])-int(e["REP_S0"])
instr=float(e["INSTR"]); af=int(e["AF"]); errs=int(e["ERRS"])
ok=1; why="ok"
if comp<=0 or instr<=0: ok,why=0,"no_progress"
elif af>0: ok,why=0,"audit_fail"
elif served<=0 or abs(served-comp)/comp > float(e["TOL"]): ok,why=0,"two_fd_fail"   # didn't dial upstream
elif errs/(comp+errs) > float(e["DROP"]): ok,why=0,"drop_high"
ipc = instr/comp if comp>0 else 0
print(f"{ok} {why} {ipc:.0f} {comp/float(e['MEASURE']):.0f} {served} {comp}")
PY
)
  read -r ok why ipc cps served comp <<<"$res"
  log "  rep $rep: instr/conn=$ipc conn/s=$cps served=$served completed=$comp gate=$ok ($why)"
  if [ "$ok" != "1" ]; then GATE_OK=0; REASON="$why"; break; fi
  IPC+=( "$ipc" ); CPS+=( "$cps" )
done
kill "$RELAY_PID" "$SINK_PID" 2>/dev/null; RELAY_PID=""; SINK_PID=""; sleep "$SETTLE"

# --- duplex correctness gate (don't let churn-optimization break long-lived) -
DUPLEX_OK=0
if [ "$GATE_OK" = 1 ]; then
  taskset -c "$SINK_CPU" "$BIN/sink" -addr 127.0.0.1:$EPORT -echo >/tmp/opt-echo.log 2>&1 & SINK_PID=$!
  sleep 0.3
  taskset -c "$SUT_CPU" env GOMAXPROCS=1 "$BIN/relay-uring" -addr 127.0.0.1 -port $DPORT \
    -sinkip 127.0.0.1 -sinkport $EPORT -duplex >/tmp/opt-duplex.log 2>&1 & RELAY_PID=$!
  sleep 0.8
  DUPLEX_OK=$(DPORT=$DPORT NC=$DUPLEX_SMOKE_CONNS python3 - <<'PY'
import socket,threading,os
port=int(os.environ["DPORT"]); n=int(os.environ["NC"]); ok=[0]
def one():
    pay=b'A'*64+(b'M'*64)*4
    try:
        s=socket.create_connection(("127.0.0.1",port),timeout=5)
        for o in range(0,len(pay),64): s.sendall(pay[o:o+64])
        got=b''; s.settimeout(5)
        while len(got)<len(pay):
            b=s.recv(4096)
            if not b: break
            got+=b
        s.close()
        if got==pay: ok[0]+=1
    except Exception: pass
ts=[threading.Thread(target=one) for _ in range(n)]
[t.start() for t in ts]; [t.join() for t in ts]
print(1 if ok[0]==n else 0)
PY
)
  kill "$RELAY_PID" "$SINK_PID" 2>/dev/null; RELAY_PID=""; SINK_PID=""
  [ "$DUPLEX_OK" = 1 ] || { GATE_OK=0; REASON="duplex_broken"; }
fi

# --- verdict ----------------------------------------------------------------
if [ "$GATE_OK" != 1 ]; then
  log "GATE FAIL ($REASON) -> score 0"; emit "{\"score\":0,\"reason\":\"$REASON\"}"
fi
python3 - "$REASON" "${IPC[*]}" "${CPS[*]}" <<'PY'
import sys
reason=sys.argv[1]
ipc=[float(x) for x in sys.argv[2].split()]; cps=[float(x) for x in sys.argv[3].split()]
mi=sum(ipc)/len(ipc); mc=sum(cps)/len(cps)
score=1e9/mi if mi>0 else 0
spread=(max(ipc)-min(ipc))/min(ipc)*100 if len(ipc)>1 and min(ipc)>0 else 0
import json
print(json.dumps({"score":round(score,1),"instr_pc":round(mi,0),"conn_s":round(mc,0),
                  "spread_pct":round(spread,1),"reps":len(ipc),"duplex_ok":1,"reason":"ok"}))
PY
