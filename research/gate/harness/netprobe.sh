#!/usr/bin/env bash
# netprobe.sh — sourceable kernel/NIC bottleneck instrumentation for the gate.
#
# WHY THIS EXISTS. conn/s tells you the relay stopped scaling; it never tells you
# WHICH ceiling you hit. On a real NIC there are at least six candidates, and they
# are indistinguishable from the throughput number alone:
#
#   1. accept path      — SYN/accept queue overflow (ListenOverflows/ListenDrops)
#   2. reuseport lock   — kernel spin/osq contention (that one is perf's job)
#   3. softirq/RSS      — NIC RX processing pinned to too few cores, or spread onto
#                         cores the SUT does not own (which ALSO voids the perf count)
#   4. NIC hardware     — ring exhaustion, rx_missed / no_buff drops
#   5. ephemeral ports  — the relay dials upstream once per conn; one
#                         (srcIP,dstIP,dstPort) tuple caps near ~64k and TIME_WAIT
#                         holds them. Looks exactly like a relay ceiling.
#   6. the loadgen      — box 2 simply not supplying enough (SynRetrans + client errs)
#
# Each of these has a counter that names it. Snapshot before, snapshot after, diff.
#
# Usage (sourced):
#   . "$(dirname "$0")/netprobe.sh"
#   netprobe_snap  "$OUT" pre
#   ... run the measured window ...
#   netprobe_snap  "$OUT" post
#   netprobe_report "$OUT" "$SUT_CORES_CSV" "$DUR" "$IFACE" | tee "$OUT/bottleneck.txt"
#
# Standalone:
#   bash netprobe.sh snap  /tmp/probe pre
#   bash netprobe.sh report /tmp/probe 20,21,22 10 eno1np0

# netprobe_snap <dir> <tag> — dump every raw counter source for one point in time.
netprobe_snap() {
  local dir=$1 tag=$2 iface=${3:-}
  mkdir -p "$dir"
  cat /proc/net/netstat  > "$dir/netstat.$tag"   2>/dev/null
  cat /proc/net/snmp     > "$dir/snmp.$tag"      2>/dev/null
  cat /proc/softirqs     > "$dir/softirqs.$tag"  2>/dev/null
  cat /proc/stat         > "$dir/stat.$tag"      2>/dev/null
  cat /proc/interrupts   > "$dir/interrupts.$tag" 2>/dev/null
  cat /proc/net/sockstat > "$dir/sockstat.$tag"  2>/dev/null
  if [ -n "$iface" ]; then
    ethtool -S "$iface" > "$dir/ethtool.$tag" 2>/dev/null || true
  fi
  date -u +%FT%T.%NZ > "$dir/ts.$tag"
}

# netprobe_report <dir> <sut-cores-csv> <duration-sec> [iface]
netprobe_report() {
  local dir=$1 sut=$2 dur=$3 iface=${4:-}
  # PYTHONIOENCODING: these boxes can run with a non-UTF-8 locale, where python's
  # stdout defaults to latin-1 and the report's typography raises UnicodeEncodeError
  # mid-report — losing the verdict, which is the part that matters.
  NP_DIR="$dir" NP_SUT="$sut" NP_DUR="$dur" NP_IFACE="$iface" \
    PYTHONIOENCODING=utf-8 python3 - <<'PY'
import os, re, sys

d    = os.environ["NP_DIR"]
sut  = {c.strip() for c in os.environ["NP_SUT"].replace(',', ' ').split() if c.strip()}
dur  = float(os.environ["NP_DUR"]) or 1.0
ifc  = os.environ.get("NP_IFACE", "")

def read(name):
    try:
        return open(os.path.join(d, name)).read()
    except OSError:
        return ""

# ---- /proc/net/netstat + /proc/net/snmp: paired header/value lines ------------
def kv_pairs(txt):
    out, lines = {}, txt.splitlines()
    for i in range(0, len(lines) - 1, 2):
        h, v = lines[i].split(), lines[i + 1].split()
        if not h or not v or h[0] != v[0]:
            continue
        pfx = h[0].rstrip(':')
        for k, val in zip(h[1:], v[1:]):
            try:
                out[f"{pfx}.{k}"] = int(val)
            except ValueError:
                pass
    return out

pre  = kv_pairs(read("netstat.pre"));  pre.update(kv_pairs(read("snmp.pre")))
post = kv_pairs(read("netstat.post")); post.update(kv_pairs(read("snmp.post")))
def dk(k):
    return post.get(k, 0) - pre.get(k, 0)

# ---- /proc/softirqs and /proc/interrupts: per-CPU columns ---------------------
def percpu(txt, want):
    """rows matching `want` -> {cpu_index: count}, summed over matching rows."""
    out, ncpu = {}, 0
    for ln in txt.splitlines():
        if ln.strip().startswith("CPU"):
            ncpu = len(ln.split())
            continue
        m = re.match(r'\s*([A-Za-z0-9_\-]+):\s+(.*)$', ln)
        if not m or not want(m.group(1), ln):
            continue
        vals = m.group(2).split()[:ncpu]
        for i, v in enumerate(vals):
            try:
                out[i] = out.get(i, 0) + int(v)
            except ValueError:
                pass
    return out

def diff_percpu(a, b):
    return {c: b.get(c, 0) - a.get(c, 0) for c in set(a) | set(b)}

netrx = diff_percpu(percpu(read("softirqs.pre"),  lambda n, l: n == "NET_RX"),
                    percpu(read("softirqs.post"), lambda n, l: n == "NET_RX"))
nicirq = {}
if ifc:
    nicirq = diff_percpu(percpu(read("interrupts.pre"),  lambda n, l: ifc in l),
                         percpu(read("interrupts.post"), lambda n, l: ifc in l))

# ---- /proc/stat: per-core jiffies, split user/sys/softirq --------------------
def cpustat(txt):
    out = {}
    for ln in txt.splitlines():
        m = re.match(r'cpu(\d+)\s+(.*)$', ln)
        if not m:
            continue
        f = [int(x) for x in m.group(2).split()]
        # user nice system idle iowait irq softirq steal ...
        out[int(m.group(1))] = {
            "user": f[0] + f[1], "sys": f[2], "idle": f[3],
            "irq": f[5] if len(f) > 5 else 0,
            "softirq": f[6] if len(f) > 6 else 0,
            "total": sum(f),
        }
    return out

sa, sb = cpustat(read("stat.pre")), cpustat(read("stat.post"))
CLK = os.sysconf("SC_CLK_TCK")

def core_busy(c):
    """(busy_cores, softirq_cores) for logical cpu c over the window."""
    if c not in sa or c not in sb:
        return 0.0, 0.0
    a, b = sa[c], sb[c]
    busy = ((b["user"] - a["user"]) + (b["sys"] - a["sys"]) +
            (b["irq"] - a["irq"]) + (b["softirq"] - a["softirq"])) / CLK / dur
    soft = ((b["softirq"] - a["softirq"]) + (b["irq"] - a["irq"])) / CLK / dur
    return busy, soft

sut_busy = sut_soft = off_busy = off_soft = 0.0
off_hot = []
for c in sa:
    busy, soft = core_busy(c)
    if str(c) in sut:
        sut_busy += busy; sut_soft += soft
    else:
        off_busy += busy; off_soft += soft
        if soft > 0.05:
            off_hot.append((c, soft))
off_hot.sort(key=lambda t: -t[1])

# ---- ethtool -S deltas -------------------------------------------------------
def ethstats(tag):
    out = {}
    for ln in read(f"ethtool.{tag}").splitlines():
        m = re.match(r'\s*([\w.\-]+):\s+(\d+)\s*$', ln)
        if m:
            out[m.group(1)] = int(m.group(2))
    return out

ea, eb = ethstats("pre"), ethstats("post")
eth_drop = {k: eb[k] - ea.get(k, 0) for k in eb
            if eb[k] - ea.get(k, 0) > 0
            and re.search(r'drop|miss|no_buf|err|over|discard', k, re.I)}

# ---- sockstat ----------------------------------------------------------------
def sockstat(tag):
    out = {}
    for ln in read(f"sockstat.{tag}").splitlines():
        parts = ln.split()
        if not parts:
            continue
        pfx = parts[0].rstrip(':')
        for i in range(1, len(parts) - 1, 2):
            try:
                out[f"{pfx}.{parts[i]}"] = int(parts[i + 1])
            except ValueError:
                pass
    return out

ssa, ssb = sockstat("pre"), sockstat("post")

W = 34
def line(label, val, note=""):
    print(f"  {label:<{W}} {val:>14}  {note}")

print("=== BOTTLENECK PROBE ===")
print(f"  window {dur:.0f}s   SUT cores [{','.join(sorted(sut, key=int)) or '-'}]   iface {ifc or '-'}")
print()

# 1. accept path
print("-- 1. accept path (SYN + accept queue) --")
ovf, drp = dk("TcpExt.ListenOverflows"), dk("TcpExt.ListenDrops")
line("ListenOverflows", f"{ovf:,}", "!! accept queue FULL — the app is not accepting fast enough" if ovf else "clean")
line("ListenDrops",     f"{drp:,}", "!! SYNs dropped at the listener" if drp else "clean")
line("SyncookiesSent",  f"{dk('TcpExt.SyncookiesSent'):,}",
     "syncookies masking overflow — disable to measure" if dk('TcpExt.SyncookiesSent') else "")
line("TCPReqQFullDrop", f"{dk('TcpExt.TCPReqQFullDrop'):,}",
     "!! SYN backlog full" if dk('TcpExt.TCPReqQFullDrop') else "clean")
line("PassiveOpens",    f"{dk('Tcp.PassiveOpens'):,}", f"{dk('Tcp.PassiveOpens')/dur:,.0f}/s accepted")
line("AttemptFails",    f"{dk('Tcp.AttemptFails'):,}", "outbound (upstream dial) failures" if dk('Tcp.AttemptFails') else "")
print()

# 2. relay's own outbound dial — ephemeral port pressure
print("-- 2. upstream dial / ephemeral ports --")
ao = dk("Tcp.ActiveOpens")
line("ActiveOpens (upstream dials)", f"{ao:,}", f"{ao/dur:,.0f}/s")
line("TW recycled (TWRecycled)", f"{dk('TcpExt.TimeWaitRecycled'):,}")
line("TW reused (TCPTimeWaitReuse)", f"{dk('TcpExt.TCPTimeWaitReuse'):,}")
line("sockets in TIME_WAIT (end)", f"{ssb.get('TCP.tw', 0):,}",
     "near the 64k/tuple ceiling — add sink ports/IPs" if ssb.get('TCP.tw', 0) > 50000 else "")
line("TCP orphans (end)", f"{ssb.get('TCP.orphan', 0):,}")
line("sockets alloc (end)", f"{ssb.get('TCP.alloc', 0):,}")
print()

# 3. softirq / RSS placement
print("-- 3. softirq / RSS placement --")
line("SUT cores busy", f"{sut_busy:,.2f}", "cores of CPU")
line("SUT cores softirq+irq", f"{sut_soft:,.2f}", "cores")
line("NON-SUT cores busy", f"{off_busy:,.2f}", "cores")
line("NON-SUT softirq+irq", f"{off_soft:,.2f}",
     "!! NIC processing OUTSIDE the SUT's cores — perf UNDERCOUNTS the SUT and this "
     "steals CPU the measurement does not attribute" if off_soft > 0.10 else "clean")
if off_hot:
    top = "  ".join(f"cpu{c}={s:.2f}" for c, s in off_hot[:8])
    print(f"  {'hottest non-SUT softirq cores':<{W}} {top}")
if netrx:
    tot = sum(v for v in netrx.values() if v > 0)
    on = sum(v for c, v in netrx.items() if str(c) in sut and v > 0)
    pct = (100.0 * on / tot) if tot else 0.0
    line("NET_RX softirqs on SUT cores", f"{pct:,.1f}%",
         "RSS is spraying RX onto foreign cores — steer the queues" if pct < 60 else "")
    nz = sorted(((c, v) for c, v in netrx.items() if v > 0), key=lambda t: -t[1])
    line("cores taking NET_RX", f"{len(nz)}", f"top: " + " ".join(f"cpu{c}:{v:,}" for c, v in nz[:6]))
if nicirq:
    nz = sorted(((c, v) for c, v in nicirq.items() if v > 0), key=lambda t: -t[1])
    line(f"{ifc} IRQs (cores active)", f"{len(nz)}",
         " ".join(f"cpu{c}:{v:,}" for c, v in nz[:6]))
print()

# 4. NIC hardware
print("-- 4. NIC hardware drops --")
if not ifc:
    print("  (no iface given — pass one to capture ethtool -S)")
elif eth_drop:
    for k, v in sorted(eth_drop.items(), key=lambda t: -t[1])[:12]:
        line(k, f"{v:,}", "!! hardware/driver drop")
else:
    print("  no nonzero drop/miss/error deltas — NIC is not the ceiling")
print()

# 5. loss / retransmit — is box 2 even reaching us?
print("-- 5. path loss / supply --")
_syn_rt = dk('TcpExt.TCPSynRetrans')
_syn_pct = (100.0 * _syn_rt / dk('Tcp.PassiveOpens')) if dk('Tcp.PassiveOpens') > 0 else 0.0
line("TCPSynRetrans", f"{_syn_rt:,}",
     f"!! {_syn_pct:.1f}% of accepts — SYNs dropped or the path is rate-limiting"
     if _syn_pct > 2 else (f"{_syn_pct:.2f}% of accepts (normal background)" if _syn_rt else "clean"))
# Half-open handshakes that died back to LISTEN. High here with a CLEAN listen queue
# means the SYN-ACK or the final ACK is being lost in the PATH, not at either app.
_af = dk('Tcp.AttemptFails')
line("AttemptFails (half-open died)", f"{_af:,}",
     "!! handshakes started and never completed — suspect the path, not the endpoints"
     if _af > 0.05 * max(dk('Tcp.PassiveOpens'), 1) else "")
line("RetransSegs", f"{dk('Tcp.RetransSegs'):,}")
line("EstabResets", f"{dk('Tcp.EstabResets'):,}")
line("InErrs", f"{dk('Tcp.InErrs'):,}")
line("IpExt.InNoRoutes", f"{dk('IpExt.InNoRoutes'):,}")
print()

# ---- verdict -----------------------------------------------------------------
print("-- VERDICT (ranked) --")
v = []
if ovf or drp:
    v.append(f"ACCEPT PATH: {ovf+drp:,} listen overflow/drop — the relay could not "
             f"accept fast enough. This is a REAL relay ceiling.")
if off_soft > 0.10:
    v.append(f"SOFTIRQ PLACEMENT: {off_soft:.2f} cores of NIC processing landed on "
             f"non-SUT cores. Fix RSS/IRQ steering before trusting conn/s-per-core.")
if eth_drop:
    v.append(f"NIC: {sum(eth_drop.values()):,} hardware drops — the card/ring is the "
             f"ceiling, not the relay.")
if ssb.get('TCP.tw', 0) > 50000:
    v.append(f"EPHEMERAL PORTS: {ssb['TCP.tw']:,} TIME_WAIT. The relay's upstream dial "
             f"is near the ~64k/tuple ceiling — add sink ports or IPs, else you are "
             f"measuring the port allocator.")
if _syn_pct > 2:
    v.append(f"PATH: {_syn_rt:,} SYN retransmits ({_syn_pct:.1f}% of accepts) — either "
             f"this box is dropping SYNs or the path is rate-limiting. Re-run the "
             f"bare-sink isolation test.")
if _af > 0.05 * max(dk('Tcp.PassiveOpens'), 1) and not (ovf or drp):
    v.append(f"PATH (handshake): {_af:,} connections reached SYN_RCVD and died without "
             f"completing, while the listen queue stayed CLEAN. The SYN-ACK or the final "
             f"ACK is being dropped between the boxes — an in-path device, not either "
             f"endpoint. A connect-flood pattern is the usual trigger.")
if not v:
    v.append("No kernel-side ceiling tripped. If conn/s flattened anyway, the limit is "
             "CPU inside the relay (read the perf profile) or the loadgen's supply.")
for i, s in enumerate(v, 1):
    print(f"  {i}. {s}")
PY
}

# Standalone entry point.
case "${1:-}" in
  snap)   netprobe_snap "$2" "$3" "${4:-}" ;;
  report) netprobe_report "$2" "$3" "$4" "${5:-}" ;;
esac
