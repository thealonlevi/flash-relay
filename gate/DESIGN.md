# Gate measurement design (Step 1 — the trustworthiness slice)

*Scope: just enough scoring rigor that the B1+B2 kill-gate produces a **trustworthy** number — not
the full anti-cheat scoring function, not the optimizer (those come post-gate, Step 4). The gate is
a mini scoring function; measured sloppily it produces a flattering-but-wrong result, which is the
exact failure the anti-fooling rules guard against.*

This document is the contract for what the gate measures and how. Code under `gate/` must implement
exactly this.

---

## 1. Topology

```
 ┌────────────┐        ┌──────────────────────┐        ┌────────────┐
 │  loadgen   │  TCP   │   relay (SUT)         │  TCP   │  upstream  │
 │  (client)  ├───────►│  accept→hook→splice   ├───────►│   sink     │
 │            │◄───────┤  (1 pinned core)      │◄───────┤            │
 └────────────┘        └──────────────────────┘        └────────────┘
   pinned to              the thing measured              pinned to
   its own core(s)        (B1 perf, B2 conn/s)            its own core(s)
```

Three processes, distinct CPU cores. **The SUT is the only thing measured.** Loadgen and sink are
infrastructure and must never share a core with the SUT (their CPU would pollute the perf counts).

- **Single-box / loopback** is acceptable for the gate: we measure the **ratio** of SUT to baseline
  on the *same box, same loadgen, same sink, same core layout*, so shared overheads cancel. Absolute
  conn/s is loadgen-limited and **not** measurement-grade — state this in every result. (Two-box +
  real NIC is for the post-gate full suite.)

## 2. The relay workload (two-fd, byte-audited)

One connection = a full two-fd relay. **Never accept-reply-close** — that's the toy we're avoiding.

Per connection:
1. Client connects to the relay's `SO_REUSEPORT` listen port; records `t0`.
2. Client writes a fixed **REQUEST** (`REQ_LEN` bytes, a known pattern).
3. Relay accepts → reads the initial request bytes → runs the **decision hook** (§3) → obtains an
   upstream fd → relays: forwards REQUEST client→upstream, forwards **REPLY** upstream→client.
4. Upstream sink reads exactly `REQ_LEN` request bytes, writes a fixed **REPLY** (`REPLY_LEN`
   bytes, a known pattern), closes its side.
5. Client reads until close; records `t1` at the **first reply byte** (connect-to-first-byte) and
   verifies it received **exactly `REPLY_LEN` bytes matching the known pattern**.
6. Relay observes either half-close and tears down both fds (correct half-close).

**Byte audit (makes the two-fd relay non-fakeable):** the sink asserts it received exactly `REQ_LEN`
request bytes; the client asserts it received exactly `REPLY_LEN` reply bytes of the known pattern.
A relay that truncates, short-circuits (replies without dialing upstream), or drops is caught here
and the run is **invalid** (not just slow). This is the gate-level stand-in for the full anti-cheat
gate.

Defaults (churn / B2): `REQ_LEN = 64`, `REPLY_LEN = 256`, short-lived (close after one reply),
fixed in-flight `IN_FLIGHT = 512`. All configurable; record the values used in every result.

## 3. The realistic decision hook (B7-lite — the most important anti-fooling control)

**Never a no-op/constant handler.** Between accept and relay, the hook simulates riptide's real
decision path and must run **off the ring** (async hand-off to a bounded worker-goroutine pool) so a
blocking hook never stalls the ring:

1. **Auth CPU burn** — a fixed, calibrated amount of CPU work (`HOOK_CPU_US`, default ~5 µs;
   represents auth + blacklist + IP-alloc). Calibrated busy work, not a sleep.
2. **Real blocking dial** — a raw blocking `connect()` (via `syscall.Connect`, **never**
   `net.Dial`) to the upstream sink. The blocking syscall parks the worker's M via the Go scheduler
   — it must **not** touch the netpoller. To approximate riptide's real dial latency (hundreds of
   µs over a network), the sink can apply a fixed `DIAL_DELAY_US` before completing; record actual
   observed dial latency regardless.

The returned upstream fd is handed back to the ring, registered as a direct descriptor, and spliced.
Success criterion carried from B7: **throughput must not collapse** with the realistic hook — that
proves the async-decision design works. The gate runs *with* the hook; a no-op variant may be run
only as a separately-labeled diagnostic, never as the headline number.

## 4. The three measured builds

| build | what | role |
|---|---|---|
| **io_uring relay (SUT)** | the hand-rolled pure-Go probe (one core, two fds, single-shot accept/recv/send/splice, no big tables/multishot/drain) | the candidate |
| **netpoller baseline** | equivalent Go relay on the standard `net` netpoller: accept → blocking-dial stub → `io.Copy` both ways, same hook semantics | the bar to beat |
| **cgo/liburing ceiling** *(optional)* | flashaccept-style C relay via cgo — **measurement only, never shipped**, behind a build tag | measures the Go-binding tax |

All three serve the **identical** workload (§2) and hook (§3) and are measured identically (§5–6).

## 5. Metrics tuple (always reported together — never just the flattering one)

| metric | how measured |
|---|---|
| **instructions/conn** *(primary)* | `perf stat -e instructions` scoped to the SUT (cgroup/pinned core) over a fixed count of byte-audited completed conns; divide. Frequency-independent; matches flashaccept's metric. |
| **B1: epoll/netpoller self-CPU** | `perf record -g` on the SUT, then `perf report --stdio`; sum self-CPU of `do_epoll_ctl`, `osq_lock`, `runtime.netpollopen`/`runtime.netpollclose` (`runtime_pollOpen`/`Close`). |
| **conn/s/core** | byte-audited completed conns ÷ wall-clock ÷ cores (1). |
| **p50 / p99 / p99.9** | client-side connect-to-first-reply-byte (`t1 − t0`), HdrHistogram-style; full distribution, not mean. |
| **RSS** | `/proc/<pid>/VmRSS` sampled during steady state. (Gate sanity only; B3 is the real memory test.) |

## 6. Environment lock (record all of this in every result)

- **CPU mitigations ON** — riptide's real state; the retpoline/Spectre tax is real cost on an
  ~85%-syscall workload. Do **not** boot `mitigations=off`. Record
  `/sys/devices/system/cpu/vulnerabilities/*` + `lscpu`.
- **Core pinning** — SUT pinned to exactly 1 core (cgroup v2 cpuset, or `taskset`); loadgen and sink
  pinned to *different* cores; record the full core→process map and NUMA node of each.
- **NUMA topology** recorded (`lscpu`, `numactl -H`); for the gate, keep all three on one node and
  note it (cross-NUMA is a post-gate B8 concern).
- **Loopback vs NIC** stated explicitly (gate = loopback single-box).
- **Reps** — N ≥ 5 per build; report **median + spread** (min/max and stddev), never a single best
  run. SUT and baseline measured back-to-back under identical conditions.
- **Kernel + Go versions**, `uname -r`, recorded.

## 7. Pass / fail (the go/no-go)

- **B1 PASS** — io_uring SUT: summed self-CPU of `epoll_ctl` + `osq_lock` + `runtime_poll*` is
  **≈0** (target < 0.5%). Baseline: **meaningful** (expect the known ~20%+). If the io_uring path is
  non-zero, **a data-plane fd leaked into the netpoller** — find which (listener? client? upstream?
  the dial?) before proceeding; this is a correctness bug, not a tuning issue.
- **B2 PASS** — io_uring SUT: conn/s/core a **clear multiple** over baseline (target ≥ 1.5×, ideally
  ~2×) at the same `IN_FLIGHT`, with materially lower instructions/conn, **consistent across reps**.
- **cgo ceiling (optional, informative)** — pure-Go ÷ cgo conn/s/core: **≈90%** → Go-binding tax is
  cheap, ship; **≈50%** → the Go layer (GC, `LockOSThread` workers, `unsafe` ring) is eating the
  win — reconsider before building the full library.

**Both B1 and B2 must pass** to green-light Step 4. Any byte-audit failure (§2) invalidates the run.

## 8. Results

Each run writes `gate/results/<timestamp>-<build>.md` (or `.json`) capturing: the full env lock
(§6), the workload params (§2), the hook params (§3), and the metrics tuple (§5) with per-rep values
+ median + spread. A short `gate/results/SUMMARY.md` tabulates SUT vs baseline (vs ceiling) and
states the B1/B2 verdict.

## 9. Open decisions to confirm before coding the probe

1. **Splice vs recv/send for the gate probe** — splice (socket→pipe→socket) is zero-copy but adds
   pipe fds; recv/send copies through a per-conn buffer. The gate is short-lived churn where fd
   count barely matters, so **recv/send is the simpler, sufficient choice for B1/B2**; splice's
   real test is B4 (throughput) post-gate. Proposed: gate probe uses recv/send; note it.
2. **Hook latency realism** — confirm `HOOK_CPU_US` (~5 µs) and `DIAL_DELAY_US` against riptide's
   actual measured auth+dial cost, so the control isn't accidentally trivial.
3. **In-flight / core budget** — confirm `IN_FLIGHT = 512` and single-core pinning match how we want
   the headline stated.
