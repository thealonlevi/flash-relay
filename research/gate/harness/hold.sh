#!/usr/bin/env bash
# hold.sh — high-concurrency HELD-connection test (B3 / incident failure mode).
# Holds N concurrent long-lived relayed connections and measures, on ONE pinned
# core: relay RSS-per-conn and the CPU profile under the hold. This is where the
# netpoller's goroutine-per-conn model collapses (scheduler + GC over N*2 goroutine
# stacks) and flash-relay (one ring worker, conns as map entries) does not.
# Loopback; ratio + profile shape are the signal. Run as root (perf + taskset).
set -uo pipefail
cd "$(dirname "$0")/.."   # -> gate/
export PATH="$PATH:/usr/local/go/bin"

CORE_SUT=${CORE_SUT:-6}; CORE_SINK=${CORE_SINK:-7}; CORE_LG=${CORE_LG:-9,10,11,12}
RPORT=${RPORT:-22000}; SPORT=${SPORT:-22100}
N=${N:-100000}; RAMP=${RAMP:-15}; HOLD=${HOLD:-30}; MEASURE=${MEASURE:-12}
REQLEN=${REQLEN:-64}; BUFSIZE=${BUFSIZE:-4096}; MAXCONNS=${MAXCONNS:-200000}
OUT=${OUT:-results/hold-$(date +%Y%m%d-%H%M%S)}; CLK=$(getconf CLK_TCK)

[ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
mkdir -p "$OUT"
OLD_P=$(cat /proc/sys/kernel/perf_event_paranoid); OLD_K=$(cat /proc/sys/kernel/kptr_restrict)
echo -1 >/proc/sys/kernel/perf_event_paranoid; echo 0 >/proc/sys/kernel/kptr_restrict
RES=$(cat /proc/sys/net/ipv4/ip_local_reserved_ports 2>/dev/null||echo ""); for p in $RPORT $SPORT; do case ",$RES," in *",$p,"*) ;; *) RES="${RES:+$RES,}$p";; esac; done; sysctl -w net.ipv4.ip_local_reserved_ports="$RES" >/dev/null 2>&1||true

R=""; S=""; H=""
cleanup(){ for v in "$H" "$R" "$S"; do [ -n "$v" ]&&kill "$v" 2>/dev/null||true; done
  pkill -x holdgen 2>/dev/null||true; pkill -x relay-uring 2>/dev/null||true; pkill -x relay-netpoll 2>/dev/null||true; pkill -x sink 2>/dev/null||true
  echo "$OLD_P">/proc/sys/kernel/perf_event_paranoid; echo "$OLD_K">/proc/sys/kernel/kptr_restrict; }
trap cleanup EXIT
for n in holdgen relay-uring relay-netpoll sink; do pkill -x "$n" 2>/dev/null; done; sleep 1

( cd ../.. && for c in relay-uring relay-netpoll sink holdgen; do CGO_ENABLED=0 go build -o bin/$c ./research/gate/cmd/$c || exit 1; done )
BIN=$(cd ../.. && pwd)/bin
ticks(){ awk '{print $14+$15}' "/proc/$1/stat" 2>/dev/null||echo 0; }
rss(){ awk '/VmRSS/{print $2}' "/proc/$1/status" 2>/dev/null||echo 0; }
estab(){ ss -tanH state established "( sport = :$RPORT )" 2>/dev/null | wc -l; }

run_build(){
  local name=$1; shift
  echo ">>> $name : holding N=$N"
  taskset -c "$CORE_SINK" env GOMAXPROCS=2 "$BIN/sink" -addr 127.0.0.1:$SPORT -echo >"$OUT/$name.sink.log" 2>&1 & S=$!
  sleep 0.4
  taskset -c "$CORE_SUT" env GOMAXPROCS=1 "$@" >"$OUT/$name.relay.log" 2>&1 & R=$!
  sleep 1; kill -0 "$R" 2>/dev/null || { echo "!! relay failed"; cat "$OUT/$name.relay.log"; return 1; }
  taskset -c "$CORE_LG" "$BIN/holdgen" -relay 127.0.0.1:$RPORT -n "$N" -reqlen $REQLEN \
    -ramp ${RAMP}s -hold ${HOLD}s -keepalive ${KEEPALIVE:-2s} >"$OUT/$name.hold.log" 2>&1 & H=$!
  sleep "$RAMP"; sleep 2   # let connections establish
  local live; live=$(estab); local rss_kb; rss_kb=$(rss "$R")
  echo "   established ~$live conns, relay RSS ${rss_kb}KB; measuring ${MEASURE}s..."
  local t0; t0=$(ticks "$R")
  perf record -g -e cpu-clock -F 997 -p "$R" -o "$OUT/$name.data" -- sleep "$MEASURE" >/dev/null 2>&1 || true
  local t1; t1=$(ticks "$R"); local rss2; rss2=$(rss "$R"); local live2; live2=$(estab)
  perf report -i "$OUT/$name.data" --stdio --no-children 2>/dev/null > "$OUT/$name.report.txt" || true
  [ "$rss2" -gt "$rss_kb" ] && rss_kb=$rss2; [ "$live2" -gt 0 ] && live=$live2
  local cores; cores=$(awk -v a="$t0" -v b="$t1" -v c="$CLK" -v d="$MEASURE" 'BEGIN{printf "%.3f",(b-a)/c/d}')
  COMM="$name" REP="$OUT/$name.report.txt" RSS="$rss_kb" LIVE="$live" CORES="$cores" python3 - <<'PY' | tee "$OUT/$name.summary.txt"
import os,re
e=os.environ; comm=e["COMM"].replace("netpoll","relay-netpoll").replace("uring","relay-uring")
rss=int(e["RSS"]); live=max(int(e["LIVE"]),1); cores=float(e["CORES"])
cats={k:0.0 for k in["scheduler","gc","netpoll","syscall_trans","io_uring","kernel_net","user","other"]}
SCHED=("schedul","findrunnable","runq","stopm","startm","notewake","notesleep","mcall","park","goready",".ready","execute","casgstatus","wakep","retake","sysmon","morestack")
GC=("gcbgmark","gcdrain","scanobject","mallocgc","markroot","sweep","writebarrier","gcmark","span")
rx=re.compile(r'^\s*([\d.]+)%\s+\S+\s+(\S+)\s+\[([k.])\]\s+(.+?)\s*$')
for ln in open(e["REP"]):
    m=rx.match(ln)
    if not m: continue
    pct,dso,kd,sym=float(m.group(1)),m.group(2),m.group(3),m.group(4); s=sym.lower()
    if "epoll" in s or "netpoll" in s: c="netpoll"
    elif "io_uring" in s: c="io_uring"
    elif "futex" in s: c="scheduler"
    elif s.startswith("runtime.") and any(x in s for x in SCHED): c="scheduler"
    elif s.startswith("runtime.") and any(x in s for x in GC): c="gc"
    elif any(x in s for x in("syscall6","entersyscall","exitsyscall","entry_syscall","sysret","do_syscall","syscall_return")): c="syscall_trans"
    elif kd=="k": c="kernel_net"
    elif comm in dso: c="user"
    else: c="other"
    cats[c]+=pct
print(f"   live={live:,}  relay RSS={rss/1024:.0f} MB  -> RSS/conn={rss/live:.1f} KB  CPU={cores} cores")
print("   CPU profile (self%):")
lab={"scheduler":"Go scheduler thrash","gc":"GC (scan goroutine stacks)","netpoll":"netpoller/epoll","syscall_trans":"syscall transition","io_uring":"io_uring","kernel_net":"kernel TCP/net","user":"relay's own code","other":"other"}
for k in["scheduler","gc","netpoll","syscall_trans","io_uring","kernel_net","user","other"]:
    print(f"     {lab[k]:<28} {cats[k]:5.1f}%  {'#'*int(cats[k]/2)}")
PY
  kill "$H" 2>/dev/null; H=""; sleep 1; kill "$R" "$S" 2>/dev/null; R=""; S=""; sleep 3
}

{
echo "HELD-CONNECTION TEST  N=$N  ramp=${RAMP}s hold=${HOLD}s  1 core (loopback)  $(date -u +%FT%TZ)"
echo "memAvail before: $(awk '/MemAvailable/{print $2/1024" MB"}' /proc/meminfo)"
echo
run_build netpoll "$BIN/relay-netpoll" -addr 127.0.0.1:$RPORT -sink 127.0.0.1:$SPORT -reqlen $REQLEN
echo
run_build uring "$BIN/relay-uring" -addr 127.0.0.1 -port $RPORT -sinkip 127.0.0.1 -sinkport $SPORT -reqlen $REQLEN -duplex -bufsize $BUFSIZE -maxconns $MAXCONNS
} 2>&1 | tee "$OUT/RESULT.txt"
echo "=== results in $OUT ==="
