#!/usr/bin/env bash
# 2-box gate ORCHESTRATOR (runs on the SUT box, box 1). Drives the whole run from
# here: for each build it fires the remote storm via the loadgend control daemon
# on box 2 (curl), runs the local SUT harness (perf + statsfile), collects the
# loadgen JSON, then combines into a measurement-grade SUMMARY.md. No SSH, no
# manual coordination.
#
# Prereq on box 2 (once): loadgend running, e.g.
#   loadgend -control 0.0.0.0:9200 -sink 0.0.0.0:9100 -relay <BOX1_IP>:18000
#
# Usage on box 1:
#   sudo env B2=203.0.113.20 BOX1_IP=203.0.113.10 bash gate/harness/run-2box.sh
set -euo pipefail
cd "$(dirname "$0")/.."   # -> gate/

B2=${B2:?set B2=<loadgen-box-ip> (runs loadgend)}
CONTROL=${CONTROL:-9200}      # loadgend control port on box 2
SPORT=${SPORT:-9100}          # sink port on box 2 (relay dials this)
RPORT=${RPORT:-18000}         # relay listen port on box 1
BOX1_IP=${BOX1_IP:-$(curl -s --max-time 5 ipinfo.io/ip)}   # storm dials BOX1_IP:RPORT
CORE_SUT=${CORE_SUT:-6}
DUR=${DUR:-10}
REPS=${REPS:-5}
INFLIGHT=${INFLIGHT:-512}
WARMUP=${WARMUP:-5s}
STORM_DUR=${STORM_DUR:-100s}  # must outlast run-sut (RAMP + REPS*DUR + B1 record)
REQLEN=${REQLEN:-64}
REPLYLEN=${REPLYLEN:-256}
AUTHCPU=${AUTHCPU:-5us}
REALISTIC=${REALISTIC:-0}
OUT=${OUT:-results/2box-$(date +%Y%m%d-%H%M%S)}

[ "$(id -u)" = 0 ] || { echo "run as root (perf + taskset + sysctl/iptables)"; exit 1; }
mkdir -p "$OUT"
echo "SUT=$BOX1_IP:$RPORT  loadgen-box=$B2 (control :$CONTROL, sink :$SPORT)  out=$OUT"

# Prep: reserve the listen port (avoid ephemeral self-collision) + allow box 2 in.
RES=$(cat /proc/sys/net/ipv4/ip_local_reserved_ports 2>/dev/null || echo "")
case ",$RES," in *",$RPORT,"*) ;; *) sysctl -w net.ipv4.ip_local_reserved_ports="${RES:+$RES,}$RPORT" >/dev/null;; esac
iptables -C INPUT -p tcp --dport "$RPORT" -s "$B2" -j ACCEPT 2>/dev/null || \
  iptables -I INPUT -p tcp --dport "$RPORT" -s "$B2" -j ACCEPT 2>/dev/null || true

# Sanity: control daemon reachable?
curl -fs --max-time 5 "http://$B2:$CONTROL/health" >/dev/null \
  && echo "loadgend reachable" || { echo "!! cannot reach loadgend at $B2:$CONTROL — start it on box 2"; exit 1; }

REAL_ARG=""; [ "$REALISTIC" = 1 ] && REAL_ARG="realistic=1"

for BUILD in uring netpoll; do
  echo "=== build=$BUILD ==="
  url="http://$B2:$CONTROL/run?relay=$BOX1_IP:$RPORT&inflight=$INFLIGHT&warmup=$WARMUP&duration=$STORM_DUR&reqlen=$REQLEN&replylen=$REPLYLEN"
  curl -fs --max-time 300 "$url" > "$OUT/${BUILD}_loadgen.json" &   # storm retries until relay is up
  CURL=$!
  env BUILD="$BUILD" SINK="$B2:$SPORT" RPORT="$RPORT" CORE_SUT="$CORE_SUT" \
    DUR="$DUR" REPS="$REPS" REQLEN="$REQLEN" REPLYLEN="$REPLYLEN" AUTHCPU="$AUTHCPU" \
    REALISTIC="$REALISTIC" OUT="$OUT" RAMP="${RAMP:-8}" \
    bash harness/run-sut.sh </dev/null
  wait "$CURL" || { echo "!! storm curl failed for $BUILD"; cat "$OUT/${BUILD}_loadgen.json" 2>/dev/null; exit 1; }
  echo "  loadgen JSON: $(python3 -c "import json;d=json.load(open('$OUT/${BUILD}_loadgen.json'));print('completed=%d conn/s=%.0f p99=%.0fus auditFail=%d'%(d['completed'],d['conn_per_sec'],d['p99_us'],d['audit_fail']))")"
done

echo "=== combine ==="
python3 harness/combine-2box.py "$OUT" "$OUT/uring_loadgen.json" "$OUT/netpoll_loadgen.json"
echo "=== results in $OUT/ ==="
