#!/usr/bin/env bash
# saturation-2box.sh — the horizontal-scaling sweep, over a REAL NIC.
#
# This is the cross of the two harnesses that already existed and did not meet:
#   saturation-sweep.sh  swept core counts, but LOOPBACK on one box (so absolute
#                        conn/s was loadgen-bound and the NIC was never in the path)
#   run-2box.sh          used a real NIC across two boxes, but pinned the SUT to
#                        ONE core (so it could not show a scaling curve at all)
#
# For each core count N in SWEEP it runs both builds against a remote storm driven
# on the loadgen box via loadgend, and tabulates per point:
#     conn/s, cores actually used, conn/s per core used, and the kernel/NIC
#     counters that NAME the ceiling (netprobe.sh)
#
# The question being answered is not "how fast" but "where does adding a core stop
# buying conn/s, and WHAT is the thing that stopped it". Candidates, in the order
# they usually bite on a real NIC: RSS/softirq placement, the SO_REUSEPORT accept
# lock, listen-queue overflow, the relay's own upstream-dial ephemeral ports, NUMA,
# and (always) the loadgen simply not supplying enough.
#
# PREREQ on box 2 (see DEPLOY-LOADGEN.md, and box2-setup.sh which generates it):
#   loadgend -control 0.0.0.0:9200 -sink 0.0.0.0:9100 -relay <BOX1_IP>:18000
#
# Usage on box 1 (the SUT):
#   DRYRUN=1 B2=<box2-ip> BOX1_IP=<box1-ip> bash research/gate/harness/saturation-2box.sh
#   sudo env B2=<box2-ip> BOX1_IP=<box1-ip> SWEEP="1 2 4 8 16" JUNK=93 INFLIGHT=8000 \
#        bash research/gate/harness/saturation-2box.sh
#
# ALWAYS DRYRUN FIRST: it prints every point's core placement and the storm URL and
# executes nothing.
set -uo pipefail
HARNESS_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$HARNESS_DIR/topology.sh"
cd "$HARNESS_DIR/.."   # -> gate/

B2=${B2:?set B2=<loadgen-box-ip> (runs loadgend)}
BOX1_IP=${BOX1_IP:?set BOX1_IP=<this box IP the storm dials>}
CONTROL=${CONTROL:-9200}       # loadgend control port on box 2
SPORT=${SPORT:-9100}           # sink port on box 2 (the relay dials this)
RPORT=${RPORT:-18000}          # relay listen port on box 1 (base of the span)
# PORTS widens the client's 4-tuple space. One (srcIP,dstIP,dstPort) tuple caps
# near ~64k ephemeral ports, so a loadgen box with ONE source IP tops out around
# ~50-60k conn/s no matter how many cores it has — well under a 20-core relay's
# capability, which would make the sweep measure the port allocator. Every worker
# binds every port, so the reuseport group per port stays N and the accept-path
# contention under test is unchanged.
PORTS=${PORTS:-16}
SWEEP=${SWEEP:-"1 2 4 8 16"}
JUNK=${JUNK:-93}               # connect-flood mix; 93 = the ISP incident profile
INFLIGHT=${INFLIGHT:-8000}
DUR=${DUR:-10}                 # seconds per measured rep
REPS=${REPS:-3}
RAMP=${RAMP:-8}
REQLEN=${REQLEN:-64}
REPLYLEN=${REPLYLEN:-256}
AUTHCPU=${AUTHCPU:-5us}
RELAY_NUMA=${RELAY_NUMA:-single}   # single = one socket (isolates the accept lock) | both = span sockets (NUMA)
RELAY_NODE=${RELAY_NODE:-0}
IFACE=${IFACE:-$(ip -o route get "$B2" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1)}' | head -1)}
OUT=${OUT:-results/sat2box-$(date +%Y%m%d-%H%M%S)}
DRYRUN=${DRYRUN:-0}

if [ "$DRYRUN" != 1 ]; then
  [ "$(id -u)" = 0 ] || { echo "run as root (perf + taskset)"; exit 1; }
fi
mkdir -p "$OUT"
RESULT="$OUT/SWEEP.txt"

# Relay core pool: one logical CPU per PHYSICAL core. Default one socket, so the
# curve isolates the accept path from cross-socket traffic; RELAY_NUMA=both is the
# separate NUMA curve and must be compared against the single-node one, never mixed.
NODES=$(numa_nodes | tr '\n' ' '); NNODES=$(echo "$NODES" | wc -w)
RELAY_POOL=$(node_primaries "$RELAY_NODE")
if [ "$RELAY_NUMA" = both ] && [ "$NNODES" -gt 1 ]; then
  RELAY_POOL=$(python3 -c "
a='''$(node_primaries "$RELAY_NODE")'''.split()
b='''$(node_primaries $(( (RELAY_NODE + 1) % NNODES )) )'''.split()
out=[]
for x, y in zip(a, b): out += [x, y]
print(' '.join(out))")
fi
MAXN=$(echo "$RELAY_POOL" | wc -w)

{
echo "2-BOX SATURATION SWEEP  $(date -u +%FT%TZ)"
echo "sweep=[$SWEEP]  junk=${JUNK}%  inflight=$INFLIGHT  dur=${DUR}s x ${REPS}  relay_numa=$RELAY_NUMA"
echo "SUT=$BOX1_IP:$RPORT via $IFACE   loadgen box=$B2 (control :$CONTROL, sink :$SPORT)"
topology_summary
echo "  relay pool (node$RELAY_NODE, physical cores): $MAXN available"
echo "  REAL NIC: unlike saturation-sweep.sh this is not loopback; the NIC, its RSS"
echo "  queues and softirq placement are all in the path and are candidate ceilings."
echo
} | tee "$RESULT"

# Reserve the listen port so the (wide) ephemeral allocator cannot self-collide.
if [ "$DRYRUN" != 1 ]; then
  # Reserve the WHOLE listen span: with a 1024-65535 ephemeral range, an outbound
  # dial can otherwise grab one of our listen ports as a source port and the next
  # bind fails with EADDRINUSE mid-sweep.
  RES=$(cat /proc/sys/net/ipv4/ip_local_reserved_ports 2>/dev/null || echo "")
  SPAN="$RPORT-$(( RPORT + PORTS - 1 ))"
  case ",$RES," in *",$SPAN,"*) ;; *) sysctl -w net.ipv4.ip_local_reserved_ports="${RES:+$RES,}$SPAN" >/dev/null 2>&1 || true;; esac

  curl -fs --max-time 5 "http://$B2:$CONTROL/health" >/dev/null \
    && echo "loadgend reachable at $B2:$CONTROL" | tee -a "$RESULT" \
    || { echo "!! cannot reach loadgend at $B2:$CONTROL — start it on box 2 (see box2-setup.sh)" | tee -a "$RESULT"; exit 1; }

  # The storm spreads across every source IP on box 2; open the relay port to all.
  SRCIPS=$(curl -fs --max-time 5 "http://$B2:$CONTROL/srcips" 2>/dev/null \
    | python3 -c "import sys,json; print(' '.join(json.load(sys.stdin)))" 2>/dev/null || true)
  [ -z "$SRCIPS" ] && SRCIPS="$B2"
  echo "opening $RPORT:$(( RPORT + PORTS - 1 )) to storm sources: $SRCIPS" | tee -a "$RESULT"
  for ip in $SRCIPS; do
    iptables -C INPUT -p tcp --dport "$RPORT:$(( RPORT + PORTS - 1 ))" -s "$ip" -j ACCEPT 2>/dev/null || \
      iptables -I INPUT -p tcp --dport "$RPORT:$(( RPORT + PORTS - 1 ))" -s "$ip" -j ACCEPT 2>/dev/null || true
  done
  echo | tee -a "$RESULT"
fi

# The storm must outlast the SUT's whole measured window (ramp + reps + B1 record).
STORM_DUR=${STORM_DUR:-$(( RAMP + 5 + DUR * REPS + DUR + 20 ))}

for N in $SWEEP; do
  if [ "$N" -gt "$MAXN" ]; then
    echo "skip N=$N (relay pool has only $MAXN physical cores)" | tee -a "$RESULT"
    continue
  fi
  RELCORES=$(take "$N" "$RELAY_POOL")
  echo "=== N=$N  relay cores=[$RELCORES]  workers=$N ===" | tee -a "$RESULT"

  # STEER=1 gives the point N NIC queues IRQ-pinned to the SAME N cores, so the NIC
  # scales with the relay and softirq is inside the measured cores. Without it this
  # box spreads RX over 80 queues/all cores: perf counts none of that softirq, which
  # flatters low-N points and hides where the real ceiling is. Opt-in because it
  # reconfigures the NIC and stops irqbalance (nic-steer.sh restore undoes both).
  if [ "${STEER:-0}" = 1 ] && [ -n "$IFACE" ] && [ "$DRYRUN" != 1 ]; then
    bash "$HARNESS_DIR/nic-steer.sh" steer "$IFACE" "$RELCORES" 2>&1 \
      | sed 's/^/  /' | tail -3 | tee -a "$RESULT"
  fi

  # Storm warmup must cover the relay's startup (build + listen + RAMP), otherwise
  # the dials that fail before the relay is listening land in the measured window and
  # inflate client errors — which is one of the signals used to judge the point.
  SWARM=$(( RAMP + 5 ))
  RPORT_END=$(( RPORT + PORTS - 1 ))
  RSPEC="$BOX1_IP:$RPORT"; [ "$PORTS" -gt 1 ] && RSPEC="$BOX1_IP:$RPORT-$RPORT_END"
  url="http://$B2:$CONTROL/run?relay=$RSPEC&inflight=$INFLIGHT&junkpct=$JUNK"
  url="$url&warmup=${SWARM}s&duration=${STORM_DUR}s&reqlen=$REQLEN&replylen=$REPLYLEN"
  if [ "$DRYRUN" = 1 ]; then
    echo "  storm: $url" | tee -a "$RESULT"
    echo "  (dry run: placement + URL only, nothing executed)" | tee -a "$RESULT"
    continue
  fi

  for BUILD in netpoll uring; do
    RUNOUT="$OUT/n$N/$BUILD"; mkdir -p "$RUNOUT"
    echo "  --- $BUILD ---" | tee -a "$RESULT"
    curl -fs --max-time $(( STORM_DUR + 120 )) "$url" > "$RUNOUT/loadgen.json" &
    CURL=$!
    env BUILD="$BUILD" SINK="$B2:$SPORT" RPORT="$RPORT" RELCORES="$RELCORES" WORKERS="$N" \
      IFACE="$IFACE" PORTS="$PORTS" DUR="$DUR" REPS="$REPS" RAMP="$RAMP" REQLEN="$REQLEN" REPLYLEN="$REPLYLEN" \
      AUTHCPU="$AUTHCPU" OUT="$RUNOUT" \
      bash "$HARNESS_DIR/run-sut.sh" </dev/null > "$RUNOUT/run-sut.log" 2>&1
    wait "$CURL" || echo "  !! storm curl failed (N=$N $BUILD)" | tee -a "$RESULT"

    N="$N" BUILD="$BUILD" RUNOUT="$RUNOUT" python3 - <<'PY' | tee -a "$RESULT"
import csv, json, os, re
n, build, d = os.environ["N"], os.environ["BUILD"], os.environ["RUNOUT"]
cps = percore = cores = ipc = 0.0
try:
    rows = list(csv.DictReader(open(f"{d}/{build}_metrics.csv")))
    good = [r for r in rows if r.get("instr_per_conn") not in (None, "NA")]
    if good:
        cps     = sum(float(r["conn_per_sec"]) for r in good) / len(good)
        percore = sum(float(r["conn_per_sec_per_core"]) for r in good) / len(good)
        cores   = sum(float(r["cores_used"]) for r in good) / len(good)
        ipc     = sum(float(r["instr_per_conn"]) for r in good) / len(good)
except (OSError, KeyError, ValueError):
    pass
lg = {}
try:
    lg = json.load(open(f"{d}/loadgen.json"))
except (OSError, ValueError):
    pass
# The client's own view: if it reports heavy errors the "ceiling" may be the path.
print(f"  {build:<8} N={n:<3} conn/s={cps:>10,.0f}  cpu={cores:>5.2f}  per-core={percore:>9,.0f}  "
      f"instr/conn={ipc:>9,.0f}  client:p99={lg.get('p99_us',0)/1000:.1f}ms errs={lg.get('errors',0):,}")
verdicts = []
try:
    txt = open(f"{d}/{build}_bottleneck.txt").read()
    verdicts = re.findall(r'^  \d+\. (.+)$', txt.split("-- VERDICT")[-1], re.M)
except (OSError, IndexError):
    pass
for v in verdicts:
    print(f"        > {v[:150]}")
PY
  done
  echo | tee -a "$RESULT"
done

{
echo "=== how to read this ==="
echo "Scaling is the SHAPE of conn/s vs N, and 'per-core' is the honest column: if"
echo "conn/s rises while per-core falls, cores are being added but not converted into"
echo "work — something serialized. The '>' lines under each point name the suspect."
echo "Compare RELAY_NUMA=single vs =both to separate NUMA from the accept-path lock."
echo "netpoll must show its OWN knee in the same sweep; if it does not, box 2 is not"
echo "supplying enough load and NEITHER curve means anything."
} | tee -a "$RESULT"
if [ "${STEER:-0}" = 1 ] && [ -n "$IFACE" ] && [ "$DRYRUN" != 1 ]; then
  echo "NIC still steered to the last point's cores. Undo with:"
  echo "  sudo bash $HARNESS_DIR/nic-steer.sh restore $IFACE"
fi
echo "=== results in $OUT (table: $RESULT) ==="
