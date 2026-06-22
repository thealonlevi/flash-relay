#!/usr/bin/env python3
"""Categorize a perf report into where the relay's CPU goes under the connect-flood.
Usage: flood-profile.py <report.txt> <comm> <load.json> <cpu_cores> <dur_s>"""
import json
import re
import sys

report, comm, loadjson, cores, dur = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], float(sys.argv[5])

cats = {k: 0.0 for k in
        ["netpoll", "scheduler", "gc", "syscall_trans", "io_uring", "kernel_net", "user", "other"]}
line_re = re.compile(r'^\s*([\d.]+)%\s+\S+\s+(\S+)\s+\[([k.])\]\s+(.+?)\s*$')
SCHED = ("schedul", "findrunnable", "runq", "stopm", "startm", "notewake", "notesleep",
         "mcall", "park", "goready", ".ready", "execute", "casgstatus", "wakep", "retake",
         "sysmon", "morestack", "newproc", "goexit")
GC = ("gcbgmark", "gcdrain", "scanobject", "mallocgc", "markroot", "sweep", "writebarrier",
      "heapbits", "gcmark", "span", "memclr", "memmove")
for ln in open(report):
    m = line_re.match(ln)
    if not m:
        continue
    pct, dso, kd, sym = float(m.group(1)), m.group(2), m.group(3), m.group(4)
    s = sym.lower()
    if "epoll" in s or "netpoll" in s:
        c = "netpoll"
    elif "io_uring" in s:
        c = "io_uring"
    elif "futex" in s:
        c = "scheduler"
    elif s.startswith("runtime.") and any(x in s for x in SCHED):
        c = "scheduler"
    elif s.startswith("runtime.") and any(x in s for x in GC):
        c = "gc"
    elif ("syscall6" in s or "entersyscall" in s or "exitsyscall" in s or "entry_syscall" in s
          or "sysret" in s or "do_syscall" in s or "syscall_return" in s or "syscall_exit" in s
          or "syscall_enter" in s):
        c = "syscall_trans"
    elif kd == "k":
        c = "kernel_net"
    elif comm in dso:
        c = "user"
    else:
        c = "other"
    cats[c] += pct

tot = sum(cats.values()) or 1.0
d = json.load(open(loadjson))
total_conns = d.get("completed", 0) + d.get("junk", 0)
cps = total_conns / d.get("duration_sec", dur) if d.get("duration_sec") else 0

print(f"  CPU cores consumed: {cores}   conn/s/core: {cps:,.0f}  "
      f"(junk={d.get('junk',0):,} real={d.get('completed',0):,} errs={d.get('errors',0):,} auditfail={d.get('audit_fail',0)})")
print(f"  where the CPU went (self%, normalized to {tot:.0f}% sampled):")
order = ["scheduler", "gc", "netpoll", "syscall_trans", "io_uring", "kernel_net", "user", "other"]
label = {"scheduler": "Go scheduler thrash", "gc": "GC", "netpoll": "netpoller/epoll",
         "syscall_trans": "syscall transition", "io_uring": "io_uring",
         "kernel_net": "kernel TCP/net (irreducible)", "user": "relay's own code", "other": "other"}
for k in order:
    bar = "#" * int(cats[k] / 2)
    print(f"    {label[k]:<30} {cats[k]:5.1f}%  {bar}")
