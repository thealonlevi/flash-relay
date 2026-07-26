#!/usr/bin/env bash
# multicore.sh — the test that hits the incident's actual failure mode.
# N-ring flash-relay (one shared-nothing io_uring ring per core, SO_REUSEPORT) vs
# N-core netpoller relay (GOMAXPROCS=N, one shared Go scheduler) under a
# high-concurrency connect-flood across N cores. Measures aggregate conn/s, total
# CPU, and — the key signal — the **cross-core scheduler + lock contention**
# (osq_lock / queued-spinlock / runtime scheduler / futex) that collapses the
# netpoller at scale and that flash-relay's per-core design avoids.
# Loopback; run as root (perf + taskset). Ephemeral range must be separated from
# listen ports (set net.ipv4.ip_local_port_range="40000 60999").
set -uo pipefail
cd "$(dirname "$0")/.."   # -> gate/
export PATH="$PATH:/usr/local/go/bin"

NCORE=${NCORE:-6}                 # relay cores 0..NCORE-1
SINK_CORE=${SINK_CORE:-6}
LG_CORES=${LG_CORES:-7,8,9,10,11,12}
JUNK=${JUNK:-90}; INFLIGHT=${INFLIGHT:-500}; DUR=${DUR:-15}; MEASURE=${MEASURE:-12}
# PORTS: listen on a SPAN of ports, and point the loadgen at the whole span.
# Measured on the 2x20-core box: with ONE destination port an 8-core loadgen tops
# out at ~36k conn/s and leaves the 8-core relay at 22% CPU -- the client's
# ephemeral-port allocation on a single (srcIP,dstIP,dstPort) tuple is the limit,
# not the relay. Spanning 16 ports gave 134k conn/s and 76% relay CPU on the same
# hardware: 3.75x. Without this the sweep pins BOTH builds at the loadgen's ceiling
# and every curve comes out flat, which is not a result about the relay at all.
PORTS=${PORTS:-16}
OUT=${OUT:-results/multicore-$(date +%Y%m%d-%H%M%S)}; CLK=$(getconf CLK_TCK)

[ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
mkdir -p "$OUT"
# Wide ephemeral range so the loadgen has ~64k source ports (connect-flood needs
# them); listen ports are picked verified-free and added to ip_local_reserved_ports
# below so the ephemeral allocator skips them (no self-collision).
sysctl -w net.ipv4.ip_local_port_range="1024 65535" >/dev/null 2>&1 || true
OLD_P=$(cat /proc/sys/kernel/perf_event_paranoid); OLD_K=$(cat /proc/sys/kernel/kptr_restrict)
echo -1 >/proc/sys/kernel/perf_event_paranoid; echo 0 >/proc/sys/kernel/kptr_restrict

# verified-free listen ports below the ephemeral range (avoid wedged-relay pollution)
# Need a CONTIGUOUS free block of PORTS for the relay span, plus one for the sink.
read RPORT SPORT < <(PORTS=$PORTS python3 -c "
import os, socket
n=int(os.environ['PORTS'])
def free(p):
 s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
 try: s.bind(('127.0.0.1',p));s.close();return True
 except: return False
f=[p for p in range(31000,39000) if free(p)]
fs=set(f)
base=next(p for p in f if all(q in fs for q in range(p,p+n)))
print(base, next(p for p in f if p >= base+n+100))")
RES=$(cat /proc/sys/net/ipv4/ip_local_reserved_ports 2>/dev/null||echo ""); sysctl -w net.ipv4.ip_local_reserved_ports="${RES:+$RES,}$RPORT-$((RPORT+PORTS-1)),$SPORT" >/dev/null 2>&1||true
RSPEC="127.0.0.1:$RPORT"; [ "$PORTS" -gt 1 ] && RSPEC="127.0.0.1:$RPORT-$((RPORT+PORTS-1))"

R=""; S=""
cleanup(){ [ -n "$R" ]&&kill "$R" 2>/dev/null||true; [ -n "$S" ]&&kill "$S" 2>/dev/null||true
  pkill -x relay-uring 2>/dev/null||true; pkill -x relay-netpoll 2>/dev/null||true; pkill -x sink 2>/dev/null||true; pkill -x loadgen 2>/dev/null||true
  echo "$OLD_P">/proc/sys/kernel/perf_event_paranoid; echo "$OLD_K">/proc/sys/kernel/kptr_restrict; }
trap cleanup EXIT
for n in relay-uring relay-netpoll sink loadgen; do pkill -x "$n" 2>/dev/null; done; sleep 1
( cd ../.. && for c in relay-uring relay-netpoll sink loadgen; do CGO_ENABLED=0 go build -o bin/$c ./research/gate/cmd/$c||exit 1; done )
BIN=$(cd ../.. && pwd)/bin
ticks(){ awk '{print $14+$15}' "/proc/$1/stat" 2>/dev/null||echo 0; }
# Relay core list. Default keeps the historical seq 0..N-1 for continuity with the
# single-socket box the earlier results came from; on a multi-socket machine pass
# a topology-aware list (see harness/topology.sh) so the run is not silently
# straddling sockets and SMT siblings. saturation-sweep.sh always passes one.
RELCORES=${RELCORES:-$(seq -s, 0 $((NCORE-1)))}

run_build(){
  local name=$1; shift
  echo ">>> $name : $NCORE cores, flood inflight=$INFLIGHT junk=${JUNK}%"
  taskset -c "$SINK_CORE" "$BIN/sink" -addr 127.0.0.1:$SPORT >"$OUT/$name.sink.log" 2>&1 & S=$!
  sleep 0.4
  taskset -c "$RELCORES" env GOMAXPROCS=$NCORE "$@" >"$OUT/$name.relay.log" 2>&1 & R=$!
  sleep 1.5; kill -0 "$R" 2>/dev/null || { echo "!! relay failed"; cat "$OUT/$name.relay.log"; return 1; }
  taskset -c "$LG_CORES" "$BIN/loadgen" -relay "$RSPEC" -inflight $INFLIGHT -junkpct $JUNK \
    -warmup 2s -duration $((DUR+4))s >"$OUT/$name.load.json" 2>/dev/null & local lg=$!
  sleep 2
  local t0; t0=$(ticks "$R")
  perf record -g -e cpu-clock -F 997 -p "$R" -o "$OUT/$name.data" -- sleep "$DUR" >/dev/null 2>&1 || true
  local t1; t1=$(ticks "$R"); wait "$lg"
  perf report -i "$OUT/$name.data" --stdio --no-children 2>/dev/null > "$OUT/$name.report.txt" || true
  local cores; cores=$(awk -v a="$t0" -v b="$t1" -v c="$CLK" -v d="$DUR" 'BEGIN{printf "%.2f",(b-a)/c/d}')
  COMM="$name" REP="$OUT/$name.report.txt" CORES="$cores" LOAD="$OUT/$name.load.json" NC="$NCORE" python3 - <<'PY' | tee "$OUT/$name.summary.txt"
import os,re,json
e=os.environ; comm=e["COMM"].replace("netpoll","relay-netpoll").replace("uring","relay-uring")
cats={k:0.0 for k in["scheduler","lock_contention","netpoll","syscall_trans","io_uring","gc","kernel_net","user","other"]}
SCHED=("schedul","findrunnable","runq","stopm","startm","notewake","notesleep","mcall","park","goready",".ready","execute","casgstatus","wakep","retake","sysmon","morestack","futex")
LOCK=("osq_lock","queued_spin","spin_lock","spinlock","__pv_queued")
GC=("gcbgmark","gcdrain","scanobject","mallocgc","markroot","sweep","writebarrier","gcmark")
rx=re.compile(r'^\s*([\d.]+)%\s+\S+\s+(\S+)\s+\[([k.])\]\s+(.+?)\s*$')
for ln in open(e["REP"]):
    m=rx.match(ln);
    if not m: continue
    pct,dso,kd,sym=float(m.group(1)),m.group(2),m.group(3),m.group(4); s=sym.lower()
    if "epoll" in s or "netpoll" in s: c="netpoll"
    elif "io_uring" in s: c="io_uring"
    elif any(x in s for x in LOCK): c="lock_contention"
    elif s.startswith("runtime.") and any(x in s for x in SCHED) or "futex" in s: c="scheduler"
    elif s.startswith("runtime.") and any(x in s for x in GC): c="gc"
    elif any(x in s for x in("syscall6","entersyscall","exitsyscall","entry_syscall","sysret","do_syscall","syscall_return")): c="syscall_trans"
    elif kd=="k": c="kernel_net"
    elif comm in dso: c="user"
    else: c="other"
    cats[c]+=pct
d=json.load(open(e["LOAD"])); tot=d.get("junk",0)+d.get("completed",0)
cps=tot/d.get("duration_sec",1) if d.get("duration_sec") else 0
cores=float(e["CORES"]); nc=int(e["NC"])
print(f"   {nc} cores: CPU={cores} cores used, conn/s={cps:,.0f} (junk={d.get('junk',0):,} real={d.get('completed',0):,} errs={d.get('errors',0):,})")
print(f"   conn/s per core used = {cps/max(cores,0.01):,.0f}")
lab={"scheduler":"Go scheduler","lock_contention":"kernel lock contention (osq/spin)","netpoll":"netpoller/epoll","syscall_trans":"syscall transition","io_uring":"io_uring","gc":"GC","kernel_net":"kernel TCP/net","user":"relay code","other":"other"}
for k in["scheduler","lock_contention","netpoll","syscall_trans","io_uring","gc","kernel_net","user","other"]:
    print(f"     {lab[k]:<34} {cats[k]:5.1f}%  {'#'*int(cats[k]/2)}")
PY
  kill "$R" "$S" 2>/dev/null; R=""; S=""; sleep 2
}

{
echo "MULTI-CORE TEST  N=$NCORE cores  flood inflight=$INFLIGHT junk=${JUNK}%  loopback  $(date -u +%FT%TZ)"
echo "relay cores=$RELCORES sink=$SINK_CORE loadgen=$LG_CORES  ports r=$RSPEC s=$SPORT (span=$PORTS) inflight=$INFLIGHT"
if [ -r "$(dirname "$0")/topology.sh" ]; then . "$(dirname "$0")/topology.sh"; topology_summary; fi
echo
run_build netpoll "$BIN/relay-netpoll" -addr 127.0.0.1:$RPORT -sink 127.0.0.1:$SPORT -ports $PORTS
echo
run_build uring "$BIN/relay-uring" -addr 127.0.0.1 -port $RPORT -sinkip 127.0.0.1 -sinkport $SPORT -workers $NCORE -cores "$RELCORES" -ports $PORTS
} 2>&1 | tee "$OUT/RESULT.txt"
echo "=== results in $OUT ==="
