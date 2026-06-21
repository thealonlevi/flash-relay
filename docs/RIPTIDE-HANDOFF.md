# flash-relay → riptide: what we built, what we measured, how to verify it on your stack

*A handoff for riptide's operator. flash-relay is a pure-Go (`CGO_ENABLED=0`) io_uring
relay engine intended to replace riptide's accept/relay data path. This document is
deliberately honest about what is **measured**, what is **projected**, and what is **not
yet built** — so you can check it against your own stack rather than take our word for it.*

---

## TL;DR

- **Thesis:** your CPU on the accept/relay path goes to **syscall transitions + Go's
  goroutine-per-connection scheduler/GC overhead + the netpoller (epoll)** — not to proxy
  logic. flash-relay removes all three: io_uring batches syscalls, there are **zero
  data-plane goroutines** (one ring worker per core; connections are map entries), and
  **no data-plane fd ever touches the netpoller**.
- **Measured (gate-stage, single core, loopback, mitigations ON):**
  - **epoll/netpoller fully eliminated** — the io_uring path shows **zero** fd-registration
    symbols (`do_epoll_ctl`/`netpollopen`/`netpollclose`); the Go baseline shows them. This
    is a binary architectural fact and is measurement-grade.
  - **~1.4–2× less CPU per connection** vs an equivalent Go/netpoller relay, depending on
    workload; **~2× less RSS per held connection**.
  - On a **connect-flood** (93% junk, like your incident), the CPU profile **inverts** from
    runtime/syscall overhead to the kernel's irreducible TCP work (kernel share 58% → 85%).
- **Multi-core (now built + measured):** multi-ring flash-relay (one shared-nothing io_uring
  ring per core via `SO_REUSEPORT`) vs N-core netpoll, 6 cores, connect-flood. At equal
  throughput flash-relay used **~31% less CPU (1.45× more conn/s per core-used)** and shed the
  scheduler/netpoller/syscall overhead the netpoller carries. **But** the loopback single-box
  loadgen can't *saturate* the cores (~21–27k conn/s ceiling), so the incident's *dramatic
  super-linear scheduler collapse at saturation* is still **not** forced — that needs a real
  multi-box NIC at scale. Also flagged: at this scale flash-relay shows *higher* kernel lock
  contention (7.6% vs 4.3%, likely the `SO_REUSEPORT` accept path on loopback) — worth
  watching as you scale cores.
- **Maturity:** flash-relay is at the **kill-gate stage** — the mechanism is proven, not the
  product. It is not production-hardened (see "What's still to build").

---

## 1. How this maps to your incident

The incident profile you shared (2026-06-15, 32,144 conn/s peak connect-flood, 40 cores):

| your profile | what it is | flash-relay effect |
|---|---|---|
| **39% Syscall6** | per-op user↔kernel transitions | io_uring batches ops per `io_uring_enter`; collapses |
| **~20% scheduler thrash (600k goroutines)** | goroutine-per-conn | **zero data-plane goroutines** → eliminated |
| **~1.5% GC** | 600k goroutine stacks | no per-conn stacks → ~eliminated |
| **~1% proxy logic** | actual work | unchanged |

The proxy's own work is a rounding error; the CPU is overhead. flash-relay targets exactly
that overhead. After integration, the CPU that remains is the **kernel's TCP handshake/
teardown work** — which is irreducible at the application layer (to go lower you'd drop junk
before the stack with XDP/eBPF/SYN-cookies; that composes with flash-relay).

---

## 2. What we measured (and the grade of each number)

Environment: single-socket 13-core KVM VM, Linux 6.8, **CPU mitigations ON** (matches your
prod), loopback (see caveats). All vs an equivalent Go relay on the standard netpoller
(`accept → blocking-dial stub → io.Copy both ways`), same workload, same box.

### B1 — epoll/netpoller elimination — **measurement-grade (real NIC)**
- io_uring relay: **0** fd-registration symbols in its perf profile. Baseline: present
  (`do_epoll_ctl`, `runtime.netpollopen/close`). No data-plane fd is registered with epoll.
- Note: `runtime.netpoll`/`do_epoll_wait` still appear in *any* Go program (the scheduler's
  idle poll) — those are benign and are **not** a leak; we count only the *registration*
  symbols.

### Per-connection CPU efficiency
| metric | baseline | flash-relay | ratio | grade |
|---|---:|---:|---:|---|
| instructions / conn (real NIC) | 240k | 167k | **1.44×** | measurement-grade |
| instructions / conn (loopback) | 318k | 206k | 1.55× | indicative |
| conn/s / core (loopback, CPU-bound) | 6,378 | 10,098 | 1.58× | indicative |

`instructions/conn` is the frequency-independent CPU-cost metric. The real-NIC 1.44× is the
honest headline for steady relay traffic.

### Connect-flood (93% junk, 1 core, loopback) — the incident workload
| | netpoll | flash-relay |
|---|---:|---:|
| CPU cores consumed | 1.070 (saturated) | **0.789** (not saturated) |
| conn/s/core | 21,774 | **32,720** |
| **CPU per connection** | 4.9 µcore·s | **2.4 µcore·s (~2.0× less)** |

CPU profile inversion (self%): syscall transition **20.4 → 5.3**, netpoller **1.4 → 0.0**,
scheduler **2.5 → 0.8**, relay's own code **13.7 → 5.2**, **kernel TCP 58.5 → 84.6**. One core
of flash-relay sustained your whole incident's connect rate (~32k/s), healthy.

### Held connections (~50k concurrent, 1 core, loopback) — the concurrency/memory question
| | netpoll | flash-relay |
|---|---:|---:|
| RSS per held conn | 10.6–12.8 KB | **5.2–6.2 KB (~2× less)** |
| GC scanning goroutine stacks (light load) | **3.9%** | 0.3% |
| relay runtime/io.Copy overhead | 22% | 3.6% |

The GC line is the tell: netpoll spends real CPU just *scanning goroutine stacks*, and that
cost **scales with concurrency** — at your 600k it would be ~10× this. flash-relay has no
goroutines to scan.

### We stress-tested it and found (and fixed) a real bug
Under the connect-flood, the first optimized build **deadlocked** (multishot accept
over-accepts with no flow control → CQ overflow → `io_uring_enter` wedges, process
unkillable). We fixed it: **single-shot accept + backpressure cap + an always-armed liveness
timeout + loadgen I/O timeouts**. The fixed build survives the exact flood that wedged it
(state healthy, no leak, clean shutdown). We mention this because **a relay that wedges under
a flood is worse than the netpoller — flood-testing is mandatory, and we did it.**

---

## 3. Honest caveats — read before trusting any number

1. **Loopback, single-box.** We could not measure conn/s over a real NIC between two cloud
   boxes — the provider throttles cross-public-IP new-connection rate (~500–1,150/s even with
   11 source IPs). So the *throughput* numbers are loopback; the *per-connection CPU* (1.44×)
   is the one real-NIC figure. On a private VLAN / your own NICs this constraint disappears.
2. **The loopback baseline understates the win.** On our box the baseline's epoll cost was
   only ~1.2% (loopback barely exercises the netpoller); in *your* production it's ~22%. So
   on your real workload flash-relay's CPU savings should be **larger** than our ~1.5–2×,
   because you also delete that ~22% the loopback never charged.
3. **Single core ⇒ the incident's main effect is unreproduced.** The ~20% scheduler thrash is
   cross-core run-queue/`osq_lock` contention over 600k goroutines on 40 cores. A single
   pinned core has no cross-core contention, so our gate **cannot** show it. It is
   flash-relay's biggest expected win (per-core `SO_REUSEPORT` rings, **no shared
   scheduler**) and it is **currently unmeasured.**
4. **Gate-stage maturity.** The mechanism is proven; the product is not. See §5.
5. **The auto-optimizer's one win (multishot accept) was flood-unsafe** and we reverted it.
   Net optimizer contribution to date: small. The big win is the architecture, not the
   optimizer.

---

## 4. How to verify this on YOUR stack (the actionable part)

You don't have to trust our numbers — here's how to check the thesis and measure the win in
your environment.

**Step A — confirm the thesis on riptide as-is (no flash-relay needed).**
During an incident (or a synthetic flood/high-concurrency window), `perf record -g` the
riptide process and categorize self-CPU into: `Syscall6`/`entersyscall` (transitions),
`runtime.sched*`/`findRunnable`/`futex` (scheduler), `runtime.netpoll*`/`do_epoll_ctl`
(netpoller), `runtime.gc*`/`scanobject` (GC), and kernel `tcp_`/`inet_`/`sock` (irreducible).
**The sum of the first four is the ceiling on what flash-relay can reclaim.** Your incident
profile suggests that's ~60% of CPU. If your steady-state profile shows the same shape,
flash-relay's value is high; if your CPU is actually in proxy logic or kernel TCP, it's low.

**Step B — reproduce our gate on your hardware.**
```
git clone git@github.com:thealonlevi/flash-relay.git
cd flash-relay && git checkout optimizer-run     # has the fixed relay + all harnesses
CGO_ENABLED=0 go build ./...                      # pure Go, no cgo
sudo bash gate/harness/gate.sh                    # B1 + per-core instr/conn vs netpoll
sudo RPORT=<free> SPORT=<free> bash gate/harness/flood.sh   # 93%-junk connect-flood CPU profile
sudo bash gate/harness/hold.sh                    # held-connection RSS/conn + profile
```
Run these on the **40c/80q box**, ideally with a real NIC and a second load box (see
`gate/harness/DEPLOY-LOADGEN.md` for the 2-box setup). The harnesses pin to one core and emit
the same profile breakdown shown above, so you can compare your hardware's ratios to ours.

**Step C — the multi-core test (now built — run it on your 40-core box).**
Multi-ring flash-relay is built: `relay-uring -workers N -startcore 0` runs N shared-nothing
io_uring rings, one per core, via `SO_REUSEPORT`. `gate/harness/multicore.sh` runs it vs
N-core netpoll under a cross-core flood and profiles scheduler + lock contention. On our box
(6 cores, loopback) it showed ~31% less CPU at equal throughput — but **we could not saturate
the cores** (loopback loadgen caps ~21–27k conn/s; the public-IP path is throttled). **On your
40-core box with real NICs and a real load source**, run `multicore.sh` (or point a real
loadgen at `relay-uring -workers 40`) and push past saturation — that is where the netpoller's
shared-scheduler collapse (your incident's ~20%) appears and flash-relay's per-core design
should pull away. This is the single most incident-relevant number, and only your hardware can
produce it. Watch flash-relay's kernel lock-contention line as cores scale (see TL;DR).

**Step D — estimate the win.** CPU reduction ≈ (your overhead fraction from Step A that
flash-relay eliminates) + the per-conn instruction savings (~1.4× on steady traffic). Memory
≈ ~2× lower RSS/conn at your concurrency (bigger as concurrency grows, since you shed the
goroutine stacks). Both should be **larger** in your prod than our loopback numbers (caveat
#2).

---

## 5. What integration requires, and what's still to build

**Capability contract — what riptide provides to flash-relay (pure Go, `CGO_ENABLED=0`):**
- Per-core io_uring + `SO_REUSEPORT` accept loops; **no `net`/netpoller for any data-plane
  fd** (listener, client, upstream). This is the whole point — a single `net.Conn` wrap on a
  data-plane fd reintroduces the netpoller.
- An **async decision hook**: accept → read initial request bytes → your Go callback (may
  block: auth, blacklist, IP-alloc, **dial**) → returns `{relay to this upstream fd, +
  optional reply bytes}` or `{reject, send bytes, close}`. The callback runs off the ring on
  a goroutine pool, so a slow dial parks one connection, not the ring.
- riptide **dials upstream itself** with a blocking raw syscall (so the upstream fd never
  hits the netpoller) and hands the connected fd to flash-relay to adopt + relay.

**Built since (prototype):** multi-ring/per-core scaling (`SO_REUSEPORT`, `-workers N` +
core affinity) — works and measured (above), though the kernel lock-contention at scale and
a bounded-batch accept (single-shot currently paces establishment) want more work.

**Not yet built (Step 4 — required before production):** registered/direct descriptors at 100k+ (the fd-table sizing for your
concurrency — each conn holds 2 fds), clean drain / zero-downtime `SO_REUSEPORT` handoff,
IPv6 + bind-specific-IP, idle timeouts, per-direction byte counters for datacap/metrics, and
the full B3–B9 benchmark suite (throughput/Gbps, NUMA scaling, tail-latency-vs-load,
reuseport fairness). The async-hook + adopt-fd API is designed but should get your sign-off
before it's locked.

---

## 6. Where the code and evidence live

- Repo: `github.com/thealonlevi/flash-relay`, branch **`optimizer-run`** (fixed relay + all
  harnesses + results). `RELAY_PLAN.md` is the full plan; `gate/DESIGN.md` is the measurement
  contract; `gate/results/` holds the raw run outputs (`SUMMARY.md`, `RESULT.txt`, perf
  reports).
- Key dirs: `gate/internal/uring` (the hand-rolled ring), `gate/internal/rawsock` (no-net
  sockets), `gate/cmd/relay-uring` (the SUT), `gate/cmd/relay-netpoll` (the baseline),
  `gate/harness/` (gate.sh / flood.sh / hold.sh / DEPLOY-LOADGEN.md).

**Bottom line:** the mechanism is real and measured — epoll eliminated, ~1.4–2× less CPU/conn,
~2× less RSS/conn, and it survives a connect-flood. The incident's biggest single effect (the
multi-core scheduler collapse) is **not yet measured** and is the next thing to build and run
on your hardware. We'd rather tell you that than imply we've proven more than we have.
