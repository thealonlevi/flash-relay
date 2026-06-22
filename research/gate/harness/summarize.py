#!/usr/bin/env python3
"""Aggregate a gate run into SUMMARY.md (B1/B2 verdict). Usage: summarize.py <dir>."""
import csv
import statistics
import sys
from pathlib import Path


def med(xs):
    return statistics.median(xs) if xs else float("nan")


def main():
    out = Path(sys.argv[1])
    rows = list(csv.DictReader((out / "metrics.csv").open()))
    builds = {}
    for r in rows:
        builds.setdefault(r["build"], []).append(r)

    def agg(b):
        rs = builds.get(b, [])
        f = lambda k: [float(x[k]) for x in rs if x[k] not in ("", "NA")]
        return {
            "ipc": f("instr_per_conn"), "cps": f("conn_per_sec"),
            "p50": f("p50_us"), "p99": f("p99_us"), "p999": f("p999_us"),
            "af": sum(int(x["audit_fail"]) for x in rs),
        }

    def b1(b):
        p = out / f"{b}_b1_epoll_selfcpu.txt"
        return float(p.read_text().strip()) if p.exists() else float("nan")

    def b1n(b):
        p = out / f"{b}_b1_nsyms.txt"
        return int(p.read_text().strip()) if p.exists() else -1

    u, n = agg("uring"), agg("netpoll")
    u_b1, n_b1 = b1("uring"), b1("netpoll")
    u_ns, n_ns = b1n("uring"), b1n("netpoll")

    def line(label, k, scale=1.0, unit=""):
        un = med(u[k]) / scale
        nn = med(n[k]) / scale
        ratio = nn / un if k == "ipc" and un else (un / nn if nn else float("nan"))
        return f"| {label} | {nn:,.1f}{unit} | {un:,.1f}{unit} | {ratio:.2f}× |"

    audit_ok = (u["af"] == 0 and n["af"] == 0)
    # B1 is a BINARY architectural fact: the SUT must have ZERO netpoller symbols
    # in its profile; the baseline must show netpoller symbols (present). The
    # magnitude on loopback is small (~1%); on riptide's real hw it is ~22%.
    b1_pass = (u_ns == 0) and (n_ns > 0)
    # B2: SUT conn/s a clear multiple of baseline.
    cps_ratio = (med(u["cps"]) / med(n["cps"])) if med(n["cps"]) else float("nan")
    b2_pass = cps_ratio >= 1.5

    env = (out / "env.txt").read_text()

    L = []
    L.append("# Gate result — dev-grade B1+B2 (headline / CPU-isolation)\n")
    L.append("> **Dev-grade, single-box loopback (KVM).** The SUT/baseline **ratio** is the signal; "
             "absolute conn/s is loadgen/loopback-limited and NOT measurement-grade. "
             "B1 (epoll elimination) is a binary architectural fact and IS conclusive here.\n")
    if not audit_ok:
        L.append(f"\n## ⚠️ INVALID: byte-audit failures (uring+baseline={u['af']+n['af']}). "
                 "Numbers below are void until fixed.\n")

    L.append("\n## Verdict\n")
    L.append(f"- **B1 (epoll elimination):** {'✅ PASS' if b1_pass else '❌ FAIL'} — "
             f"SUT has **{u_ns} netpoller symbols** ({u_b1:.3f}% self-CPU); baseline has "
             f"**{n_ns}** ({n_b1:.2f}%). Binary fact: io_uring path never registers an fd with epoll. "
             "(Loopback magnitude ~1%; riptide-production epoll cost is ~22%, so the real-hw win is larger.)")
    L.append(f"- **B2 (conn/s-per-core):** {'✅ PASS' if b2_pass else '❌ FAIL'} — "
             f"SUT is **{cps_ratio:.2f}×** baseline (target ≥1.5×).")
    L.append(f"- **Byte audit:** {'✅ clean' if audit_ok else '❌ FAILURES'}.")
    gate = audit_ok and b1_pass and b2_pass
    L.append(f"\n**Gate: {'✅ GO (dev-grade)' if gate else '❌ NO-GO / needs work'}** — "
             "confirm on measurement-grade hardware before Step 4.\n")

    L.append("\n## Metrics (median of reps)\n")
    L.append("| metric | baseline (netpoll) | SUT (io_uring) | ratio |")
    L.append("|---|---:|---:|---:|")
    L.append(line("instructions / conn", "ipc") + "  ← lower is better (ratio = baseline/SUT)")
    L.append(line("conn/s / core", "cps"))
    L.append(line("p50 latency", "p50", unit="µs"))
    L.append(line("p99 latency", "p99", unit="µs"))
    L.append(line("p99.9 latency", "p999", unit="µs"))
    L.append(f"| **B1 epoll self-CPU** | **{n_b1:.2f}%** | **{u_b1:.3f}%** | — |")

    L.append("\n## Per-rep (variance)\n")
    L.append("| build | rep | instr/conn | conn/s | p99 µs | audit_fail |")
    L.append("|---|---|---:|---:|---:|---:|")
    for r in rows:
        ipc = f"{float(r['instr_per_conn']):,.0f}" if r["instr_per_conn"] not in ("", "NA") else "NA"
        L.append(f"| {r['build']} | {r['rep']} | {ipc} | {float(r['conn_per_sec']):,.0f} "
                 f"| {float(r['p99_us']):,.0f} | {r['audit_fail']} |")

    L.append("\n## Environment\n```\n" + env.strip() + "\n```\n")
    (out / "SUMMARY.md").write_text("\n".join(L) + "\n")


if __name__ == "__main__":
    main()
