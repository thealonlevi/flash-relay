#!/usr/bin/env bash
# box2-setup.sh — run this ON THE LOADGEN BOX (box 2). One command, idempotent.
#
# Box 2 does two jobs for the 2-box saturation sweep: it is the connection-storm
# CLIENT and the upstream SINK the relay dials. Both legs therefore cross a real
# NIC, which is the whole point of running 2-box instead of loopback.
#
# It must OUT-SUPPLY the SUT. If it cannot, the sweep measures box 2's ceiling and
# every conclusion about the relay's scaling is wrong — so this script pins the
# storm and the sink to DISJOINT core sets and tells you what it gave each.
#
# Usage:
#   sudo env BOX1_IP=<sut-ip> LG_CORES=0-15 SINK_CORES=16-19 bash box2-setup.sh
#   sudo bash box2-setup.sh stop        # kill the daemon, leave sysctls in place
#
# Knobs: BOX1_IP (required), LG_CORES, SINK_CORES, SPORT, CONTROL, RPORT, BINDIR.
set -uo pipefail

if [ "${1:-}" = stop ]; then
  pkill -x loadgend 2>/dev/null && echo "stopped loadgend" || echo "loadgend not running"
  pkill -x sink 2>/dev/null && echo "stopped sink" || true
  exit 0
fi

BOX1_IP=${BOX1_IP:?set BOX1_IP=<the SUT box IP> (the storm dials it, and it dials our sink)}
LG_CORES=${LG_CORES:-0-15}      # storm cores. 10-20 is the ask; must out-supply the SUT.
SINK_CORES=${SINK_CORES:-16-19} # sink cores, DISJOINT from the storm's
SPORT=${SPORT:-9100}            # sink listen port (base of the span; the relay dials this)
# SINK_PORTS must match the relay's -sinkports. The relay dials upstream once per
# connection; with ONE destination port every dial shares a single
# (srcIP,dstIP,dstPort) 4-tuple. Without TIME_WAIT reuse that is 64511 ports over a
# 60s hold = ~1,075 conn/s, and a 2-box run measured exactly 1,086 before collapsing.
SINK_PORTS=${SINK_PORTS:-16}
CONTROL=${CONTROL:-9200}        # loadgend control port (box 1 drives the run through it)
RPORT=${RPORT:-18000}           # the relay's listen port on box 1 (base of the span)
PORTS=${PORTS:-16}              # relay listen-port span; must match the sweep's PORTS
BINDIR=${BINDIR:-/usr/local/bin}

[ "$(id -u)" = 0 ] || { echo "run as root (sysctl + ulimit + firewall)"; exit 1; }
for b in loadgend sink; do
  [ -x "$BINDIR/$b" ] || { echo "!! $BINDIR/$b missing — copy it from box 1 first:
     scp root@$BOX1_IP:/root/dev/flash-relay/bin/{loadgend,sink,loadgen} $BINDIR/"; exit 1; }
done

echo "=== 1. kernel tuning (connection storm) ==="
# A churn client burns through ephemeral ports, TIME_WAIT entries and fds faster
# than any normal workload. Untuned, box 2 throttles ITSELF and understates the SUT.
cat > /etc/sysctl.d/99-flashrelay-bench.conf <<EOF
net.ipv4.ip_local_port_range = 1024 65535
# Reserve the fixed ports we listen on, or the (now very wide) ephemeral allocator
# can grab 9100/9200 as a source port and the next bind fails with EADDRINUSE.
net.ipv4.ip_local_reserved_ports = $SPORT-$(( SPORT + SINK_PORTS - 1 )),$CONTROL,$RPORT-$(( RPORT + PORTS - 1 ))
# TIME_WAIT reuse needs timestamps on BOTH ends. If this box does not send them,
# the SUT cannot reuse TIME_WAIT for its upstream dials and is hard-capped at
# ~1,075 conn/s per destination port no matter how much CPU either box has.
net.ipv4.tcp_timestamps = 1
net.ipv4.tcp_tw_reuse = 1
net.ipv4.tcp_fin_timeout = 10
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535
net.core.netdev_max_backlog = 250000
net.ipv4.tcp_max_tw_buckets = 2000000
EOF
sysctl -p /etc/sysctl.d/99-flashrelay-bench.conf 2>&1 | sed 's/^/  /'

if lsmod 2>/dev/null | grep -q nf_conntrack; then
  echo "  conntrack IS loaded — raising max (overflow silently drops connections)"
  sysctl -w net.netfilter.nf_conntrack_max=2000000 2>&1 | sed 's/^/  /'
else
  echo "  conntrack not loaded (good)"
fi

echo
echo "=== 2. firewall: let box 1 reach the sink + control port ==="
for p in "$SPORT" "$CONTROL"; do
  iptables -C INPUT -p tcp --dport "$p" -s "$BOX1_IP" -j ACCEPT 2>/dev/null || \
    iptables -I INPUT -p tcp --dport "$p" -s "$BOX1_IP" -j ACCEPT 2>/dev/null && \
    echo "  opened $p from $BOX1_IP"
done

echo
echo "=== 3. core budget ==="
NLG=$(taskset -c "$LG_CORES" bash -c 'nproc' 2>/dev/null || echo "?")
echo "  storm cores : $LG_CORES  ($NLG cpus)"
echo "  sink  cores : $SINK_CORES"
echo "  total on box: $(nproc)"
# Overlap would put sink CPU inside the storm's budget and quietly cut supply.
python3 - "$LG_CORES" "$SINK_CORES" <<'PY'
import sys
def ex(s):
    out = set()
    for p in s.split(','):
        if '-' in p:
            a, b = p.split('-'); out |= set(range(int(a), int(b) + 1))
        elif p.strip():
            out.add(int(p))
    return out
a, b = ex(sys.argv[1]), ex(sys.argv[2])
common = sorted(a & b)
print(f"  !! storm and sink SHARE cores {common} — the sink will eat storm capacity"
      if common else "  storm/sink cores are disjoint")
PY

echo
echo "=== 3b. sink port availability (informational — busy ports are skipped) ==="
# The sink SCANS for free ports rather than demanding a contiguous span, so a busy
# port here is not fatal. It is still worth reporting: a box with many busy ports in
# range is running something that will also compete with the test for CPU and NIC.
BUSY=""
for p in $(seq "$SPORT" $(( SPORT + SINK_PORTS - 1 ))); do
  if ! python3 - "$p" <<'PY' 2>/dev/null
import socket, sys
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try:
    s.bind(("0.0.0.0", int(sys.argv[1]))); s.close()
except OSError:
    sys.exit(1)
PY
  then
    BUSY="$BUSY $p"
    # -tan, not -lntp: the holder is usually NOT a listener. Linux's ephemeral
    # allocator prefers ODD ports for connect(), so a previous storm's outbound
    # sockets squat exactly the odd ports of any span placed inside
    # ip_local_port_range, and a listening-only query reports "nothing holds it".
    holder=$(ss -tanp 2>/dev/null | awk -v p=":$p\$" '$4 ~ p {print $1, $NF; exit}')
    echo "  port $p busy (${holder:-unknown}) — will be skipped"
  fi
done
if [ -n "$BUSY" ]; then
  echo "  ^ the sink scans past these (-portwindow). Mostly ODD ports means another"
  echo "    process is churning outbound connections here: Linux prefers odd ephemeral"
  echo "    ports for connect(). If that process is a live proxy, this box is NOT"
  echo "    dedicated and the run will contend with its traffic."
else
  echo "  all $SINK_PORTS ports free from $SPORT"
fi

echo
echo "=== 4. start sink + loadgend (pinned, separate) ==="
pkill -x loadgend 2>/dev/null; pkill -x sink 2>/dev/null; sleep 1
ulimit -n 1048576

# The sink runs SEPARATELY (loadgend -sink "") so it gets its own cores. Hosting it
# in-process would put upstream-side work inside the storm's core budget.
taskset -c "$SINK_CORES" "$BINDIR/sink" -addr "0.0.0.0:$SPORT" -ports "$SINK_PORTS" \
  -portwindow 512 -reqlen 64 -replylen 256 > /var/log/flashrelay-sink.log 2>&1 &
sleep 2
RSPEC="$BOX1_IP:$RPORT"; [ "$PORTS" -gt 1 ] && RSPEC="$BOX1_IP:$RPORT-$(( RPORT + PORTS - 1 ))"
taskset -c "$LG_CORES" "$BINDIR/loadgend" -control "0.0.0.0:$CONTROL" -sink "" \
  -relay "$RSPEC" -reqlen 64 -replylen 256 -srcips auto \
  > /var/log/flashrelay-loadgend.log 2>&1 &
sleep 2

echo
echo "=== 5. verify ==="
pgrep -x sink     >/dev/null && echo "  sink     up (:$SPORT-$(( SPORT + SINK_PORTS - 1 )), cores $SINK_CORES)" || { echo "  !! sink failed:"; tail -5 /var/log/flashrelay-sink.log; }
BOUND=$(grep -o 'SINK_PORTS_BOUND=[0-9,]*' /var/log/flashrelay-sink.log 2>/dev/null | tail -1 | cut -d= -f2)
echo "  sink ports bound: $(echo "$BOUND" | tr ',' ' ' | wc -w) / $SINK_PORTS"
echo "  tcp_timestamps: $(cat /proc/sys/net/ipv4/tcp_timestamps) (must be 1, or the SUT cannot reuse TIME_WAIT on its upstream dials)"
pgrep -x loadgend >/dev/null && echo "  loadgend up (:$CONTROL, cores $LG_CORES)"    || { echo "  !! loadgend failed:"; tail -5 /var/log/flashrelay-loadgend.log; }
echo -n "  /health: "; curl -fs --max-time 3 "http://127.0.0.1:$CONTROL/health" || echo "NO RESPONSE"
echo -n "  storm source IPs: "; curl -fs --max-time 3 "http://127.0.0.1:$CONTROL/srcips" || echo "?"
echo
echo "Source-IP count matters: one (srcIP -> $BOX1_IP:$RPORT) 4-tuple caps near ~64k"
echo "ports, so each extra IP on this box multiplies the storm's connection-rate"
echo "headroom. With one IP, expect a hard ceiling around 60k conn/s regardless of cores."
echo
MYIP=$(ip -o route get "$BOX1_IP" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}' | head -1)
echo
echo "=== 6. upstream target list for box 1 ==="
echo "  Bound ports are NOT necessarily contiguous (busy ones were skipped), so the"
echo "  relay must be told each one. Paste into -sinkips, with -sinkports 1:"
echo
echo "    $(echo "$BOUND" | tr ',' '\n' | sed "s|^|${MYIP:-<this-box-ip>}:|" | paste -sd,)"
echo
echo "Box 2 is ready."
