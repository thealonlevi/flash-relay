#!/usr/bin/env python3
"""Combine 2-box gate outputs into SUMMARY.md.

Usage: combine-2box.py <out-dir> <uring_loadgen.json> <netpoll_loadgen.json>

<out-dir> holds the SUT-side files from run-sut.sh (<build>_metrics.csv,
<build>_b1_nsyms.txt, <build>_b1_selfcpu.txt). The two JSONs are the loadgen-box
outputs for each build (latency + client-side conn/s + audit_fail).
"""
import csv
import json
import statistics
import sys
from pathlib import Path


def med(xs):
    return statistics.median(xs) if xs else float("nan")


def sut(out, build):
    p = out / f"{build}_metrics.csv"
    rows = list(csv.DictReader(p.open())) if p.exists() else []
    f = lambda k: [float(r[k]) for r in rows if r[k] not in ("", "NA")]
    return {"ipc": f("instr_per_conn"), "cps": f("conn_per_sec")}


def b1(out, build):
    n = out / f"{build}_b1_nsyms.txt"
    s = out / f"{build}_b1_selfcpu.txt"
    return (int(n.read_text()) if n.exists() else -1,
            float(s.read_text()) if s.exists() else float("nan"))


def main():
    out = Path(sys.argv[1])
    lg = {"uring": json.load(open(sys.argv[2])), "netpoll": json.load(open(sys.argv[3]))}
    u, n = sut(out, "uring"), sut(out, "netpoll")
    u_ns, u_self = b1(out, "uring")
    n_ns, n_self = b1(out, "netpoll")

    cps_ratio = med(u["cps"]) / med(n["cps"]) if med(n["cps"]) else float("nan")
    ipc_ratio = med(n["ipc"]) / med(u["ipc"]) if med(u["ipc"]) else float("nan")
    audit_ok = lg["uring"]["audit_fail"] == 0 and lg["netpoll"]["audit_fail"] == 0
    b1_pass = u_ns == 0 and n_ns > 0
    b2_pass = cps_ratio >= 1.5
    gate = audit_ok and b1_pass and b2_pass

    L = ["# Gate result — MEASUREMENT-GRADE (2-box, real NIC)\n"]
    L.append("\n## Verdict\n")
    L.append(f"- **B1 (epoll elimination):** {'✅ PASS' if b1_pass else '❌ FAIL'} — "
             f"SUT fd-registration symbols **{u_ns}** ({u_self:.3f}%); baseline **{n_ns}** ({n_self:.2f}%).")
    L.append(f"- **B2 (conn/s-per-core):** {'✅ PASS' if b2_pass else '❌ FAIL'} — "
             f"SUT **{cps_ratio:.2f}×** baseline conn/s, **{ipc_ratio:.2f}×** fewer instructions/conn.")
    L.append(f"- **Byte audit:** {'✅ clean' if audit_ok else '❌ FAILURES'}.")
    L.append(f"\n**Gate: {'✅ GO' if gate else '❌ NO-GO'}**\n")

    L.append("\n## Metrics\n")
    L.append("| metric | baseline | SUT (io_uring) | ratio |")
    L.append("|---|---:|---:|---:|")
    L.append(f"| instructions / conn | {med(n['ipc']):,.0f} | {med(u['ipc']):,.0f} | {ipc_ratio:.2f}× |")
    L.append(f"| conn/s / core | {med(n['cps']):,.0f} | {med(u['cps']):,.0f} | {cps_ratio:.2f}× |")
    for k, lab in [("p50_us", "p50"), ("p99_us", "p99"), ("p999_us", "p99.9")]:
        nn, uu = lg["netpoll"][k], lg["uring"][k]
        r = nn / uu if uu else float("nan")
        L.append(f"| {lab} latency | {nn:,.0f}µs | {uu:,.0f}µs | {r:.2f}× |")
    L.append(f"| B1 fd-registration symbols | {n_ns} | {u_ns} | — |")
    L.append("\n_conn/s + instructions/conn measured on the SUT box (perf + statsfile); "
             "latency measured on the loadgen box (client-side connect-to-first-byte)._\n")
    (out / "SUMMARY.md").write_text("\n".join(L) + "\n")
    print((out / "SUMMARY.md").read_text())


if __name__ == "__main__":
    main()
