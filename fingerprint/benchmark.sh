#!/usr/bin/env bash
# benchmark.sh — measure the TCP-fingerprint feature's per-connection cost using the
# project referee (optimizer/score.sh: instr/conn on pinned cores, with anti-cheat
# gates that also prove the relay still correctly relays under load — two-fd upstream
# dial, duplex, drop rate). Runs baseline (eBPF detached, no mark) then all 4 profiles
# (eBPF attached, FP_MARK=n) and tabulates instr/conn + the cost delta.
#
# On loopback the tc-egress eBPF runs inline in the relay's process context, so
# `perf stat -p relay` (what score.sh uses) captures the eBPF cost too.
#
# Usage: sudo bash fingerprint/benchmark.sh [iface]   (default lo)
set -uo pipefail
cd "$(dirname "$0")/.."
IFACE="${1:-lo}"
OBJ="fingerprint/bpf/syn_rewrite.bpf.o"
[ -f "$OBJ" ] || { (cd fingerprint && ./build.sh); }
sysctl -w net.core.rmem_max=16777216 >/dev/null 2>&1 || true

detach(){ tc qdisc del dev "$IFACE" clsact 2>/dev/null; }
attach(){ detach; tc qdisc add dev "$IFACE" clsact; tc filter add dev "$IFACE" egress bpf da obj "$OBJ" sec tc 2>/dev/null || { echo "attach failed"; exit 1; }; }
trap detach EXIT

OUT=/tmp/fp-bench.jsonl; : > "$OUT"
run(){ # $1 label  $2 fpmark
  local label=$1 mark=$2 j
  echo "[bench] running $label (FP_MARK=$mark) ..." >&2
  j=$(FP_MARK=$mark bash research/optimizer/score.sh 2>/tmp/bench_$label.log | tail -1)
  echo "{\"label\":\"$label\",\"fpmark\":$mark,\"r\":$j}" >> "$OUT"
  echo "[bench] $label => $j" >&2
}

detach;  run baseline 0
attach
run windows 1
run macos   2
run android 3
run ios     4
detach

echo "===================== FINGERPRINT BENCHMARK ====================="
python3 - "$OUT" <<'PY'
import json,sys
rows=[json.loads(l) for l in open(sys.argv[1]) if l.strip()]
base=next((x for x in rows if x["label"]=="baseline"), None)
b_ipc=base["r"].get("instr_pc",0) if base else 0
print(f'{"profile":<10}{"instr/conn":>12}{"conn/s":>10}{"spread%":>9}{"vs base":>10}  gate')
for x in rows:
    r=x["r"]; ipc=r.get("instr_pc",0); cps=r.get("conn_s",0); sp=r.get("spread_pct",0)
    d = f'{(ipc-b_ipc)/b_ipc*100:+.1f}%' if b_ipc and x["label"]!="baseline" else "—"
    print(f'{x["label"]:<10}{ipc:>12,.0f}{cps:>10,.0f}{sp:>8.1f}%{d:>10}  {r.get("reason","?")}')
print("="*64)
print("Lower instr/conn is better. 'gate'=ok means the relay correctly relayed under")
print("load (upstream dialed, duplex intact, drop rate under ceiling) with that profile.")
PY
