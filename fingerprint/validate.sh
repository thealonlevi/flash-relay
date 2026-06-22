#!/usr/bin/env bash
# validate.sh — attach the rewrite eBPF and assert each fingerprint profile emits the
# right SYN (TTL + option layout + wscale [+ iOS DSCP/ECN]) AND that TTL + IP ID hold
# on EVERY packet of the flow (coherence), not just the SYN. Run on the egress box
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
# Enable real ECN so the Apple profiles request it (iOS [SEW] SYN bits); restore on exit.
OLD_ECN=$(cat /proc/sys/net/ipv4/tcp_ecn 2>/dev/null || echo 2)
sysctl -w net.ipv4.tcp_ecn=1 >/dev/null 2>&1 || true
tc qdisc del dev "$IFACE" clsact 2>/dev/null
tc qdisc add dev "$IFACE" clsact
tc filter add dev "$IFACE" egress bpf da obj "$OBJ" sec tc 2>/dev/null \
  || { echo "attach failed"; exit 1; }
trap 'tc qdisc del dev "$IFACE" clsact 2>/dev/null; sysctl -w net.ipv4.tcp_ecn=$OLD_ECN >/dev/null 2>&1' EXIT

freeport(){ python3 -c "import socket;s=socket.socket();s.bind(('127.0.0.1',0));print(s.getsockname()[1]);s.close()"; }
FAILED=0

# check <mark> <rcvbuf> <want_ttl> <want_wscale> <want_opt_regex> <label> [tos] [ipid]
# ipid: zero (assert SYN id 0) | nonzero (assert a nonzero id) | default nonzero.
# When tos>0 (iOS), also assert the DSCP byte (tos 0x50) and the ECN [SEW] SYN flags.
check(){
  local mark=$1 rcvbuf=$2 ttl=$3 ws=$4 re=$5 label=$6 tos=${7:-0} ipid=${8:-nonzero}
  local P; P=$(freeport)
  python3 -c "import socket;s=socket.socket();s.setsockopt(1,2,1);s.bind(('127.0.0.1',$P));s.listen(16);import time;time.sleep(10)" & local L=$!; sleep 0.5
  rm -f /tmp/v.txt
  timeout 5 tcpdump -i "$IFACE" -nvv \
    "tcp[tcpflags]&tcp-syn!=0 and tcp[tcpflags]&tcp-ack==0 and dst port $P" >/tmp/v.txt 2>/dev/null & local TD=$!; sleep 1.5
  python3 -c "
import socket,time
for _ in range(4):
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_MARK,$mark)
    if $rcvbuf>0: s.setsockopt(socket.SOL_SOCKET,socket.SO_RCVBUF,$rcvbuf)
    if $tos>0: s.setsockopt(socket.IPPROTO_IP,socket.IP_TOS,$tos)
    try: s.settimeout(2); s.connect(('127.0.0.1',$P))
    except: pass
    s.close(); time.sleep(0.1)" 2>/dev/null
  sleep 0.5; kill "$TD" "$L" 2>/dev/null; wait "$TD" 2>/dev/null
  local line; line=$(tr '\n' ' ' < /tmp/v.txt)
  local ok=1 why=""
  echo "$line" | grep -q "ttl $ttl" || { ok=0; why="$why ttl(want $ttl)"; }
  echo "$line" | grep -qE "wscale $ws" || { ok=0; why="$why wscale(want $ws)"; }
  echo "$line" | grep -qE "$re" || { ok=0; why="$why opts(want /$re/)"; }
  if [ "$ipid" = zero ]; then
    echo "$line" | grep -q "id 0," || { ok=0; why="$why ipid(want 0)"; }
    echo "$line" | grep -qE "id [1-9][0-9]*," && { ok=0; why="$why ipid(nonzero seen)"; }
  else
    echo "$line" | grep -qE "id [1-9][0-9]*," || { ok=0; why="$why ipid(want nonzero)"; }
  fi
  if [ "$tos" -gt 0 ]; then
    printf '0x%02x' "$tos" | { read -r h; echo "$line" | grep -q "tos $h" || { ok=0; why="$why tos(want $h)"; }; }
    echo "$line" | grep -q "Flags \[SEW\]" || { ok=0; why="$why ecn(want [SEW])"; }
  fi
  if [ "$ok" = 1 ]; then echo "  PASS  $label"; else echo "  FAIL  $label —$why"; FAILED=1; fi
}

# coherence <mark> <rcvbuf> <want_ttl> <ipid_mode> <label>
# Exchanges DATA and captures EVERY client egress packet (not just the SYN), asserting
# TTL holds on all of them (the Windows-TTL-on-data fix) and the IP ID mode holds
# (zero | rand | keep). This is what proves the every-packet rewrite, not SYN-only.
coherence(){
  local mark=$1 rcvbuf=$2 ttl=$3 ipid=$4 label=$5
  local P; P=$(freeport)
  python3 -c "
import socket,threading,time
s=socket.socket();s.setsockopt(1,2,1);s.bind(('127.0.0.1',$P));s.listen(4)
def h(c):
    try:
        while True:
            b=c.recv(2048)
            if not b: break
            c.sendall(b)
    except: pass
    c.close()
end=time.time()+8
while time.time()<end:
    s.settimeout(1)
    try: c,_=s.accept(); threading.Thread(target=h,args=(c,),daemon=True).start()
    except: pass" & local L=$!; sleep 0.5
  rm -f /tmp/c.txt
  # capture every client->server packet (dst port P) = SYN + data + ACKs from the client.
  timeout 6 tcpdump -i "$IFACE" -nvv "dst port $P" >/tmp/c.txt 2>/dev/null & local TD=$!; sleep 1.5
  python3 -c "
import socket,time
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_MARK,$mark)
if $rcvbuf>0: s.setsockopt(socket.SOL_SOCKET,socket.SO_RCVBUF,$rcvbuf)
try:
    s.settimeout(3); s.connect(('127.0.0.1',$P))
    for i in range(10):
        s.sendall(b'x'*800)
        try: s.settimeout(1); s.recv(4096)
        except: pass
        time.sleep(0.08)
except: pass
s.close()" 2>/dev/null
  sleep 0.5; kill "$TD" "$L" 2>/dev/null; wait "$TD" 2>/dev/null
  python3 - "$ttl" "$ipid" "$label" <<'PY' || FAILED=1
import sys,re
ttl,ipid,label=sys.argv[1],sys.argv[2],sys.argv[3]
ips=re.findall(r'ttl (\d+), id (\d+)', open('/tmp/c.txt').read())
ttls=set(t for t,_ in ips); ids=[int(i) for _,i in ips]
ok=True; why=[]
if len(ips)<3: ok=False; why.append(f'too few packets ({len(ips)})')
if ttls and ttls!={ttl}: ok=False; why.append(f'ttl={sorted(ttls)} want [{ttl}] (TTL not coherent on data!)')
if ipid=='zero' and any(i!=0 for i in ids): ok=False; why.append('ipid not all 0 on data')
if ipid=='rand' and len(set(ids))<3: ok=False; why.append(f'ipid not random ({len(set(ids))} distinct)')
if ipid=='keep' and not ids: ok=False; why.append('no ids')
print(('  PASS  ' if ok else '  FAIL  ')+f'{label}  [{len(ips)} pkts, ttl={sorted(ttls)}, {len(set(ids))} distinct ipids]'+('' if ok else ' — '+'; '.join(why)))
sys.exit(0 if ok else 1)
PY
}

echo "=== SYN fingerprint validation on $IFACE ==="
check 1 8388608  128 8 'mss [0-9]+,nop,wscale [0-9]+,nop,nop,sackOK\]'         "Windows 10/11"               0  nonzero
check 2 2097152  64  6 'mss [0-9]+,nop,wscale [0-9]+,nop,nop,TS.*sackOK,eol\]' "macOS (IPID 0)"              0  zero
check 4 2097152  64  6 'mss [0-9]+,nop,wscale [0-9]+,nop,nop,TS.*sackOK,eol\]' "iOS (mark4: DSCP 0x50, ECN, rand IPID)" 80 nonzero
check 3 16777216 64  9 'mss [0-9]+,sackOK,TS.*nop,wscale [0-9]+\]'             "Android (Linux layout, rand IPID)" 0 nonzero

echo "=== per-packet coherence (TTL + IP ID on DATA packets) ==="
coherence 1 8388608  128 keep "Windows: TTL 128 holds on data"
coherence 2 2097152  64  zero "macOS: IPID 0 holds on data"
coherence 4 2097152  64  rand "iOS: random IPID on data"
coherence 3 16777216 64  rand "Android: random IPID on data"

echo "=============================================="
[ "$FAILED" = 0 ] && echo "ALL PROFILES PASS" || { echo "SOME PROFILES FAILED"; exit 1; }
echo
echo "On a real NIC, also run p0f to see the OS label:  p0f -i $IFACE"
