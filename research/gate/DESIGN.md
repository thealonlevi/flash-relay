# Gate measurement design (Step 1 — the trustworthiness slice)

*Scope: just enough scoring rigor that the B1+B2 kill-gate produces a **trustworthy** number — not
the full anti-cheat scoring function, not the optimizer (those come post-gate, Step 4). The gate is
a mini scoring function; measured sloppily it produces a flattering-but-wrong result, which is the
exact failure the anti-fooling rules guard against.*

This document is the contract for what the gate measures and how. Code under `research/gate/` must implement
exactly this. Open decisions are resolved in §9.

---

## 1. Topology

```
 ┌────────────┐        ┌──────────────────────┐        ┌────────────┐
 │  loadgen   │  TCP   │   relay (SUT)         │  TCP   │  upstream  │
 │  (client)  ├───────►│  accept→hook→relay    ├───────►│   sink     │
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
   upstream fd → relays via **recv/send** (§9.1): forwards REQUEST client→upstream, forwards
   **REPLY** upstream→client.
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
`IN_FLIGHT` per run variant (§4). All configurable; record the values used in every result.

## 3. The decision hook (B7-lite — the most important anti-fooling control)

**Never a no-op/constant handler.** Between accept and relay, the hook simulates riptide's real
decision path and **runs off the ring** (async hand-off to a bounded worker-goroutine pool) so a
slow hook never stalls the ring. It has two independent cost components — **do not conflate them**:

### 3.1 Auth CPU — `HOOK_CPU_US` (default ~5 µs), a calibrated busy-spin
Models riptide's real auth + blacklist + IP-alloc + policy checks. **Verified against the live
system:** the auth path is `crypto/subtle` constant-time compare + a hash-map lookup + a few
in-memory policy checks — **not** bcrypt/argon/scrypt. The live perf profile corroborates: at 38k
samples there is **zero crypto in the top symbols** (if auth did per-connection bcrypt it would
dominate the profile and this whole io_uring exercise would be pointless). So auth is genuinely
single-digit µs; ~5 µs is defensible, maybe slightly generous.
- Implement as a **calibrated CPU busy-spin** that actually competes for the core. **Not a sleep** —
  a sleep yields and would understate the cost.

### 3.2 Dial latency — `DIAL_DELAY` (milliseconds, async, non-CPU)
Models the real egress dial: riptide is the exit node, so the "dial" is a real TCP handshake from
the egress IP to an arbitrary internet destination — **wall-clock RTT, not CPU**. Order of magnitude
(use real ClickHouse upstream-connect timing if available; otherwise this lognormal is a defensible
start): **p50 ~15–25 ms, p90 ~80 ms, p99 ~200 ms+**, with a small fraction hitting the **30 s dial
timeout** (those feed the IP-cooling path). Units are **milliseconds, not microseconds.**

Two hard requirements for the probe:
- **Model it as an async timer that parks the connection** — never a CPU spin, and **never a block
  on the io_uring worker**. In the real design the hook is a goroutine off the ring; a slow dial
  parks *that one connection* while the worker keeps accepting/relaying others. The realistic-dial
  variant's whole job is to **prove that parking is off-ring and cheap** — if the probe lets the
  dial block the worker, you will see a *false* throughput collapse.
- A **real raw blocking `connect()`** (via `syscall.Connect`, **never** `net.Dial`) to the local
  sink is still performed on the off-ring hook goroutine — that proves the dial path does not touch
  the netpoller (the architectural point). The *latency* itself is injected by the async timer
  (we can't get ms-scale RTT on loopback), not by the loopback connect.

## 4. Run variants (keep CPU isolation and dial-parking separate — the central trap)

The trap: running the headline with realistic dial latency at a fixed 512 in-flight measures an
**under-saturated core** and reports a misleadingly low conn/s. So split into two runs:

| variant | HOOK_CPU | DIAL_DELAY | IN_FLIGHT | what it measures |
|---|---|---|---|---|
| **Headline (CPU-isolation)** | ~5 µs spin | **0 / async-instant** | **512** | clean instructions/conn + conn/s/core vs baseline (B1 + B2). 512 saturates the core with tiny payloads (flashaccept saturated at 512). |
| **Realistic-dial (parking/concurrency)** | ~5 µs spin | **ms-scale dist (§3.2)** | auto-scaled* | the async-decision design holds: parked connections are off-ring and cheap. |

\* In the realistic-dial run, most of the 512 are parked waiting on the dial and the core idles, so
512 won't saturate it. Either **auto-scale offered concurrency until the core is CPU-bound again**,
or report this run as a **concurrency/parking test** — how many parked connections it holds, RSS,
and that **per-connection CPU is unchanged vs the headline** — rather than as a second CPU headline.
**Do not** report a realistic-dial run as the conn/s headline.

## 5. The three measured builds

| build | what | role |
|---|---|---|
| **io_uring relay (SUT)** | the hand-rolled pure-Go probe (one core, two fds, single-shot accept/recv/send, no big tables/multishot/drain) | the candidate |
| **netpoller baseline** | equivalent Go relay on the standard `net` netpoller: accept → blocking-dial stub → `io.Copy` both ways, same hook semantics | the bar to beat |
| **cgo/liburing ceiling** *(optional)* | flashaccept-style C relay via cgo — **measurement only, never shipped**, behind a build tag | measures the Go-binding tax |

All three serve the **identical** workload (§2) and hook (§3) and are measured identically (§6–7).

## 6. Metrics tuple (always reported together — never just the flattering one)

| metric | how measured |
|---|---|
| **instructions/conn** *(primary)* | `perf stat -e instructions` scoped to the SUT (cgroup/pinned core) over a fixed count of byte-audited completed conns; divide. Frequency-independent; matches flashaccept's metric. |
| **B1: epoll/netpoller self-CPU** | `perf record -g` on the SUT, then `perf report --stdio`; sum self-CPU of `do_epoll_ctl`, `osq_lock`, `runtime.netpollopen`/`runtime.netpollclose` (`runtime_pollOpen`/`Close`). |
| **conn/s/core** | byte-audited completed conns ÷ wall-clock ÷ cores (1). |
| **p50 / p99 / p99.9** | client-side connect-to-first-reply-byte (`t1 − t0`), HdrHistogram-style; full distribution, not mean. |
| **RSS** | `/proc/<pid>/VmRSS` sampled during steady state. (Gate sanity only; B3 is the real memory test. Primary readout for the realistic-dial parking run.) |

## 7. Environment lock (record all of this in every result)

- **CPU mitigations ON** — riptide's real state; the retpoline/Spectre tax is real cost on an
  ~85%-syscall workload. Do **not** boot `mitigations=off`. Record
  `/sys/devices/system/cpu/vulnerabilities/*` + `lscpu`.
- **Core pinning** — SUT pinned to exactly 1 core (cgroup v2 cpuset, or `taskset`); loadgen and sink
  pinned to *different* cores; record the full core→process map and NUMA node of each.
- **NUMA topology** recorded (`lscpu`, `numactl -H`); for the gate, keep all three on one node and
  note it (cross-NUMA is a post-gate B8 concern).
- **Loopback vs NIC** stated explicitly (gate = loopback single-box).
- **Reps** — N ≥ 5 per build/variant; report **median + spread** (min/max and stddev), never a
  single best run. SUT and baseline measured back-to-back under identical conditions.
- **Kernel + Go versions**, `uname -r`, recorded.

## 8. Pass / fail (the go/no-go)

Both B1 and B2 are evaluated on the **headline (CPU-isolation)** run.
- **B1 PASS** — io_uring SUT: summed self-CPU of `epoll_ctl` + `osq_lock` + `runtime_poll*` is
  **≈0** (target < 0.5%). Baseline: **meaningful** (expect the known ~20%+). If the io_uring path is
  non-zero, **a data-plane fd leaked into the netpoller** — find which (listener? client? upstream?
  the dial?) before proceeding; this is a correctness bug, not a tuning issue.
- **B2 PASS** — io_uring SUT: conn/s/core a **clear multiple** over baseline (target ≥ 1.5×, ideally
  ~2×) at the same `IN_FLIGHT`, with materially lower instructions/conn, **consistent across reps**.
- **Async-decision PASS (realistic-dial run)** — per-connection CPU unchanged vs headline and the
  core does not collapse: parking is off-ring. (Carries the B7 intent into the gate.)
- **cgo ceiling (optional, informative)** — pure-Go ÷ cgo conn/s/core: **≈90%** → Go-binding tax is
  cheap, ship; **≈50%** → the Go layer (GC, `LockOSThread` workers, `unsafe` ring) is eating the
  win — reconsider before building the full library.

**Both B1 and B2 must pass** (plus no async-decision collapse) to green-light Step 4. Any byte-audit
failure (§2) invalidates the run.

## 9. Resolved decisions

1. **recv/send at the gate; splice deferred to B4.** The gate workload is setup/teardown-bound, not
   data-bound (short conns, tiny payloads), so recv-vs-splice is nearly invisible to the gate number.
   recv/send is the **conservative** choice (if it already shows epoll=0 + a conn/s win, splice only
   widens it) and is the path riptide needs anyway for **per-direction byte counting and userspace
   throttling** — splice bypasses userspace, so you can't meter/pace in-process. Splice's real test
   is B4 (throughput), post-gate.
2. **Hook realism (confirmed, with unit fix):** `HOOK_CPU_US ≈ 5 µs` calibrated busy-spin (§3.1);
   `DIAL_DELAY` is a **ms-scale async park**, not a µs spin (§3.2). Pull the real dial distribution
   from ClickHouse upstream-connect timing if available.
3. **Single core for the headline (confirmed); in-flight decoupled from the dial (§4).** Headline =
   512 in-flight with DIAL_DELAY=0; realistic-dial = ms distribution with auto-scaled concurrency,
   reported as the parking/concurrency test. Multi-core/NUMA scaling is B8, separate.
