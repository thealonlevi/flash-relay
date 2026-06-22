# Gate result — dev-grade B1+B2 (headline / CPU-isolation)

> **Dev-grade, single-box loopback (KVM).** The SUT/baseline **ratio** is the signal; absolute conn/s is loadgen/loopback-limited and NOT measurement-grade. B1 (epoll elimination) is a binary architectural fact and IS conclusive here.


## Verdict

- **B1 (epoll elimination):** ✅ PASS — SUT has **0 netpoller symbols** (0.000% self-CPU); baseline has **4** (0.56%). Binary fact: io_uring path never registers an fd with epoll. (Loopback magnitude ~1%; riptide-production epoll cost is ~22%, so the real-hw win is larger.)
- **B2 (conn/s-per-core):** ✅ PASS — SUT is **1.58×** baseline (target ≥1.5×).
- **Byte audit:** ✅ clean.

**Gate: ✅ GO (dev-grade)** — confirm on measurement-grade hardware before Step 4.


## Metrics (median of reps)

| metric | baseline (netpoll) | SUT (io_uring) | ratio |
|---|---:|---:|---:|
| instructions / conn | 318,234.4 | 205,584.3 | 1.55× |  ← lower is better (ratio = baseline/SUT)
| conn/s / core | 6,377.9 | 10,097.7 | 1.58× |
| p50 latency | 78,804.0µs | 46,625.0µs | 0.59× |
| p99 latency | 118,652.0µs | 103,508.0µs | 0.87× |
| p99.9 latency | 130,206.0µs | 123,385.0µs | 0.95× |
| **B1 epoll self-CPU** | **0.56%** | **0.000%** | — |

## Per-rep (variance)

| build | rep | instr/conn | conn/s | p99 µs | audit_fail |
|---|---|---:|---:|---:|---:|
| netpoll | 1 | 320,680 | 6,854 | 111,275 | 0 |
| netpoll | 2 | 318,234 | 6,348 | 119,283 | 0 |
| netpoll | 3 | 317,006 | 6,280 | 118,652 | 0 |
| netpoll | 4 | 317,800 | 6,412 | 117,365 | 0 |
| netpoll | 5 | 318,542 | 6,378 | 118,687 | 0 |
| uring | 1 | 205,658 | 9,666 | 112,506 | 0 |
| uring | 2 | 205,584 | 10,098 | 103,508 | 0 |
| uring | 3 | 205,497 | 9,957 | 99,524 | 0 |
| uring | 4 | 205,890 | 10,122 | 104,625 | 0 |
| uring | 5 | 205,035 | 11,005 | 101,227 | 0 |

## Environment
```
timestamp: 20260620-181521
kernel: 6.8.0-57-generic
go: go version go1.25.11 linux/amd64
cmdline: BOOT_IMAGE=/vmlinuz-6.8.0-57-generic root=LABEL=cloudimg-rootfs ro console=tty1 console=ttyS0
cores: SUT=6 SINK=7,8 LOADGEN=9,10,11,12
params: inflight=512 dur=10s warmup=3s reps=5 reqlen=64 replylen=256 authcpu=5us realistic=0
loopback: yes (single-box; ratio-based, not measurement-grade absolutes)
=== mitigations ===
spectre_v2:Mitigation: IBRS; IBPB: conditional; STIBP: disabled; RSB filling; PBRSB-eIBRS: Not affected; BHI: SW loop, KVM: SW loop
itlb_multihit:Not affected
mmio_stale_data:Mitigation: Clear CPU buffers; SMT Host state unknown
mds:Mitigation: Clear CPU buffers; SMT Host state unknown
reg_file_data_sampling:Not affected
l1tf:Mitigation: PTE Inversion; VMX: flush not necessary, SMT disabled
spec_store_bypass:Mitigation: Speculative Store Bypass disabled via prctl
tsx_async_abort:Mitigation: Clear CPU buffers; SMT Host state unknown
swapgs barriers and __user pointer sanitization
gather_data_sampling:Unknown: Dependent on hypervisor status
retbleed:Mitigation: IBRS
spec_rstack_overflow:Not affected
srbds:Not affected
meltdown:Mitigation: PTI
=== lscpu (sockets/numa/model) ===
CPU(s):                               13
Model name:                           Intel(R) Xeon(R) Gold 6154 CPU @ 3.00GHz
BIOS Model name:                      pc-i440fx-11.0  CPU @ 2.0GHz
Core(s) per socket:                   13
Socket(s):                            1
NUMA node(s):                         1
NUMA node0 CPU(s):                    0-12
```

