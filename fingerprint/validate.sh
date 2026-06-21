#!/usr/bin/env bash
# validate.sh — attach the SYN-rewrite eBPF and assert each fingerprint profile
# emits the right TTL + TCP option layout + window scale. Run on the egress box
# (real NIC) to additionally confirm the window value and the p0f OS label.
#
# Usage: sudo bash fingerprint/validate.sh [iface]   (default iface: lo)
# Needs: CAP_NET_ADMIN, the built bpf/syn_rewrite.bpf.o, tcpdump, python3.
set -uo pipefail
cd "$(dirname "$0")"
IFACE="${1:-lo}"
OBJ="bpf/syn_rewrite.bpf.o"
[ -f "$OBJ" ] || { echo "build first: ./build.sh"; exit 1; }

sysctl -w net.core.rmem_max=16777216 >/dev/null 2>&1 || true
tc qdisc del dev "$IFACE" clsact 2>/dev/null
tc qdisc add dev "$IFACE" clsact
tc filter add dev "$IFACE" egress bpf da obj "$OBJ" sec tc 2>/dev/null \
  || { echo "attach failed"; exit 1; }
trap 'tc qdisc del dev "$IFACE" clsact 2>/dev/null' EXIT

freeport(){ python3 -c "import socket;s=socket.socket();s.bind(('127.0.0.1',0));print(s.getsockname()[1]);s.close()"; }

# check <mark> <rcvbuf> <want_ttl> <want_wscale> <want_opt_regex> <label>
check(){
  local mark=$1 rcvbuf=$2 ttl=$3 ws=$4 re=$5 label=$6
  local P; P=$(freeport)
  python3 -c "import socket;s=socket.socket();s.setsockopt(1,2,1);s.bind(('127.0.0.1',$P));s.listen(16);import time;time.sleep(10)" & local L=$!; sleep 0.5
  rm -f /tmp/v.txt
  # capture ALL SYNs to P (no -c1 race), then fire several marked connects.
  timeout 5 tcpdump -i "$IFACE" -nvv \
    "tcp[tcpflags]&tcp-syn!=0 and tcp[tcpflags]&tcp-ack==0 and dst port $P" >/tmp/v.txt 2>/dev/null & local TD=$!; sleep 1.5
  python3 -c "
import socket,time
for _ in range(4):
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_MARK,$mark)
    if $rcvbuf>0: s.setsockopt(socket.SOL_SOCKET,socket.SO_RCVBUF,$rcvbuf)
    try: s.settimeout(2); s.connect(('127.0.0.1',$P))
    except: pass
    s.close(); time.sleep(0.1)" 2>/dev/null
  sleep 0.5; kill "$TD" "$L" 2>/dev/null; wait "$TD" 2>/dev/null
  local line; line=$(tr '\n' ' ' < /tmp/v.txt) # all captured SYNs (IP ttl lines + Flags/options lines)
  local ok=1 why=""
  echo "$line" | grep -q "ttl $ttl" || { ok=0; why="$why ttl(want $ttl)"; }
  echo "$line" | grep -qE "wscale $ws" || { ok=0; why="$why wscale(want $ws)"; }
  echo "$line" | grep -qE "$re" || { ok=0; why="$why opts(want /$re/)"; }
  if [ "$ok" = 1 ]; then echo "  PASS  $label"; else echo "  FAIL  $label —$why"; FAILED=1; fi
}

FAILED=0
echo "=== fingerprint profile validation on $IFACE ==="
check 1 8388608 128 8 'mss [0-9]+,nop,wscale [0-9]+,nop,nop,sackOK\]'              "Windows 10/11"
check 2 2097152 64  6 'mss [0-9]+,nop,wscale [0-9]+,nop,nop,TS.*sackOK,eol\]'      "macOS"
check 2 4194304 64  7 'mss [0-9]+,nop,wscale [0-9]+,nop,nop,TS.*sackOK,eol\]'      "iOS (eBPF mark 2 == macOS layout)"
check 3 8388608 64  8 'mss [0-9]+,sackOK,TS.*nop,wscale [0-9]+\]'                  "Android"
echo "=============================================="
[ "$FAILED" = 0 ] && echo "ALL PROFILES PASS" || { echo "SOME PROFILES FAILED"; exit 1; }
echo
echo "On a real NIC, also run p0f to see the OS label:  p0f -i $IFACE"