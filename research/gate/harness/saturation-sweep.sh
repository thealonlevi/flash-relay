#!/usr/bin/env bash
# saturation-sweep.sh — THE decisive experiment, as a scaling curve.
#
# The question: when flash-relay removes the Go-runtime doorways (scheduler,
# netpoller, GC), does the netpoller's super-linear collapse under overload
# actually DISAPPEAR, or does it just RELOCATE to the kernel's SO_REUSEPORT
# accept-path lock? A single core count cannot answer that — a collapse is a
# SHAPE, visible only as conn/s vs cores across the knee. So sweep it.
#
# For each core count N in SWEEP, this runs multicore.sh (netpoll baseline, then
# flash-relay) under a connect-flood and tabulates, per build:
#     conn/s, CPU cores actually used, conn/s per core used,
#     kernel lock contention %, Go scheduler %, netpoller %
# Reading the result:
#   * conn/s keeps climbing with N, lock% flat      -> collapse eliminated. Headline.
#   * conn/s flattens while lock% climbs            -> collapse RELOCATED to #3
#                                                      (reuseport accept lock). The
#                                                      real ceiling, now named.
#   * flash-relay's knee sits at a higher N than netpoll's -> quantifies the win.
#
# PLACEMENT (see topology.sh — this matters more than it looks): relay rings get
# one logical CPU per PHYSICAL core, all on ONE NUMA node by default. On an
# interleaved dual-socket box a naive `seq 0 N-1` list straddles both sockets and
# doubles up SMT siblings, which folds NUMA and SMT effects into the lock-
# contention signal and makes the result uninterpretable. Set RELAY_NUMA=both to
# deliberately span sockets — that is the #4 NUMA measurement, run as its own
# comparison against the single-node curve, not mixed into it.
#
# Usage:
#   sudo bash research/gate/harness/saturation-sweep.sh
#   sudo env SWEEP="1 2 4 8 16 24" JUNK=93 bash research/gate/harness/saturation-sweep.sh
#   sudo env RELAY_NUMA=both SWEEP="8 16 24 32" bash research/gate/harness/saturation-sweep.sh
#
# Knobs: SWEEP (core counts), JUNK (%% junk/reject mix), INFLIGHT, DUR, MEASURE,
#        RELAY_NUMA (single|both), RELAY_NODE, LOAD_NODE, OUT, DRYRUN.
#
# DRYRUN=1 prints the full core placement for every sweep point and executes
# NOTHING — run that first on a new box to confirm the relay, sink and loadgen
# land where you expect before committing to a real flood.
#
# READ BEFORE RUNNING ON A SHARED BOX: this saturates every core it is given and
# floods loopback. It is not safe to run beside production traffic.
set -uo pipefail
cd "$(dirname "$0")"
. ./topology.sh

[ "$(id -u)" = 0 ] || { echo "run as root (perf + taskset)"; exit 1; }

SWEEP=${SWEEP:-"1 2 4 8 16"}
JUNK=${JUNK:-90}
INFLIGHT=${INFLIGHT:-6000}
DUR=${DUR:-15}
MEASURE=${MEASURE:-12}
RELAY_NUMA=${RELAY_NUMA:-single}   # single = one socket (isolates #3) | both = span sockets (#4)
RELAY_NODE=${RELAY_NODE:-0}
LOAD_NODE=${LOAD_NODE:-1}
OUT=${OUT:-results/saturation-$(date +%Y%m%d-%H%M%S)}
mkdir -p "$OUT"

NODES=$(numa_nodes | tr '\n' ' ')
NNODES=$(echo "$NODES" | wc -w)
RELAY_POOL=$(node_primaries "$RELAY_NODE")
if [ "$RELAY_NUMA" = both ] && [ "$NNODES" -gt 1 ]; then
  # Interleave the two nodes' primaries so ring i lands on alternating sockets —
  # the placement that actually exercises cross-node behavior at every N.
  RELAY_POOL=$(python3 -c "
import sys
a='''$(node_primaries "$RELAY_NODE")'''.split()
b='''$(node_primaries $(( (RELAY_NODE+1) % NNODES )) )'''.split()
out=[]
for x,y in zip(a,b): out += [x,y]
print(' '.join(out))")
fi
# Load generation lives on the OTHER node so it never steals the relay's cores or
# its local memory bandwidth. With one node it necessarily shares — noted below.
if [ "$NNODES" -gt 1 ]; then LOAD_POOL=$(node_primaries "$LOAD_NODE"); else LOAD_POOL=$(node_primaries "$RELAY_NODE"); fi
ALL_PRIM=""
for n in $NODES; do ALL_PRIM="$ALL_PRIM $(node_primaries "$n")"; done
ALL_PRIM=$(echo "$ALL_PRIM" | xargs)

MAXN=$(echo "$RELAY_POOL" | wc -w)
RESULT="$OUT/SWEEP.txt"

{
echo "SATURATION SWEEP  $(date -u +%FT%TZ)"
echo "sweep=[$SWEEP]  junk=${JUNK}%  inflight=$INFLIGHT  dur=${DUR}s  relay_numa=$RELAY_NUMA"
topology_summary
if [ "$NNODES" -le 1 ]; then
  echo "  NOTE: single NUMA node — loadgen SHARES the relay's node; treat cross-node claims as unmeasured."
fi
echo "  loopback: this measures the reuseport/NUMA scaling shape, NOT real-NIC saturation."
echo
} | tee "$RESULT"

for N in $SWEEP; do
  if [ "$N" -gt "$MAXN" ]; then
    echo "skip N=$N (only $MAXN physical cores on the relay pool)" | tee -a "$RESULT"
    continue
  fi
  RELCORES=$(take "$N" "$RELAY_POOL")
  # Infrastructure cores must be DISJOINT from the relay's. In RELAY_NUMA=both the
  # relay pool interleaves both nodes, so it overlaps the load node — carve the
  # relay's cores out of the available pool rather than assuming separate nodes.
  AVAIL=$(subtract "$LOAD_POOL" "$RELCORES")
  if [ "$RELAY_NUMA" = both ] && [ "$NNODES" -gt 1 ]; then
    AVAIL=$(subtract "$ALL_PRIM" "$RELCORES")
  fi
  NAVAIL=$(echo "$AVAIL" | wc -w)
  if [ "$NAVAIL" -lt 2 ]; then
    echo "skip N=$N (only $NAVAIL cores left for sink+loadgen)" | tee -a "$RESULT"
    continue
  fi
  # Loadgen must out-supply the relay or the "ceiling" measured is the loadgen's.
  # Give it up to 2x the relay's cores; the sink gets one of its own.
  LGN=$(( N * 2 )); [ "$LGN" -gt $(( NAVAIL - 1 )) ] && LGN=$(( NAVAIL - 1 ))
  SINK_CORE=$(take 1 "$AVAIL")
  LG_CORES=$(take "$LGN" "$(drop 1 "$AVAIL" | tr ',' ' ')")
  assert_disjoint "relay" "$RELCORES" "sink+loadgen" "$SINK_CORE,$LG_CORES" || exit 1
  if [ "$LGN" -lt "$N" ]; then
    echo "  WARNING: only $LGN loadgen cores for $N relay cores — the measured ceiling may be the LOADGEN's, not the relay's" | tee -a "$RESULT"
  fi
  RUNOUT="$OUT/n$N"

  echo "=== N=$N  relay=[$RELCORES]  sink=$SINK_CORE  loadgen=[$LG_CORES]" | tee -a "$RESULT"
  if [ "${DRYRUN:-0}" = 1 ]; then
    echo "  (dry run: placement only, nothing executed)" | tee -a "$RESULT"
    continue
  fi
  NCORE=$N RELCORES="$RELCORES" SINK_CORE="$SINK_CORE" LG_CORES="$LG_CORES" \
    JUNK="$JUNK" INFLIGHT="$INFLIGHT" DUR="$DUR" MEASURE="$MEASURE" OUT="$RUNOUT" \
    bash ./multicore.sh >"$OUT/n$N.log" 2>&1 || echo "  !! run N=$N failed (see $OUT/n$N.log)" | tee -a "$RESULT"

  for build in netpoll uring; do
    f="$RUNOUT/$build.summary.txt"
    [ -r "$f" ] || { echo "  $build: no summary" | tee -a "$RESULT"; continue; }
    python3 - "$f" "$build" "$N" <<'PY' | tee -a "$RESULT"
import re, sys
txt = open(sys.argv[1]).read()
def num(pat, default=0.0):
    m = re.search(pat, txt)
    return float(m.group(1).replace(',', '')) if m else default
cps   = num(r'conn/s=([\d,\.]+)')
cpu   = num(r'CPU=([\d\.]+) cores used')
percore = num(r'conn/s per core used = ([\d,\.]+)')
lock  = num(r'kernel lock contention \(osq/spin\)\s+([\d\.]+)%')
sched = num(r'Go scheduler\s+([\d\.]+)%')
npoll = num(r'netpoller/epoll\s+([\d\.]+)%')
print(f"  {sys.argv[2]:<8} N={sys.argv[3]:<3} conn/s={cps:>10,.0f}  cpu={cpu:>5.2f}  "
      f"per-core={percore:>9,.0f}  lock={lock:>5.1f}%  sched={sched:>5.1f}%  netpoll={npoll:>5.1f}%")
PY
  done
  echo | tee -a "$RESULT"
done

{
echo "=== how to read this ==="
echo "flash-relay(uring) conn/s should keep climbing with N while netpoll's flattens."
echo "If uring's per-core ALSO decays as N grows AND lock% climbs, the collapse relocated"
echo "to the SO_REUSEPORT accept path (#3) — that is the real ceiling, and the next"
echo "target. Compare RELAY_NUMA=single vs =both to separate NUMA (#4) from the lock."
echo "Loopback caveat stands: absolute conn/s is loadgen-bound, the SHAPE is the result."
} | tee -a "$RESULT"
echo "=== sweep results in $OUT (table: $RESULT) ==="
