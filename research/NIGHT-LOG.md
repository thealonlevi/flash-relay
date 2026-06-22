# NIGHT-LOG — autonomous overnight build (started 2026-06-20)

Working autonomously per Alon's "leave you running all night". Sequence A
(scale-first). Checkpoint convention: each task → tests + a real run where
possible → small commit → tick here. Resumable from this file + git log.

## Status legend: [ ] todo  [~] in progress  [x] done  [!] blocked (needs human)

## Plan (ordered)
- [~] **T1. Continuous bidirectional relay.** Real duplex relay (both directions
      until half-close), idle timeout, per-direction byte counters. Foundation for
      B3 and the real library. (Current probe is one-shot churn.)
- [ ] **T2. Long-lived client + streaming sink.** loadgen "hold N, trickle data"
      mode + sink echo/stream mode, for B3.
- [ ] **T3. B3 run (box 1 loopback).** Ramp to 50–100k concurrent tunnels, hold
      ≥30 min, measure RSS/conn + leak (RSS slope ≈ 0). Adoption criterion #3.
- [ ] **T4. Direct descriptors at scale.** Sparse register + FILE_INDEX_ALLOC,
      table sized conns/worker×2 + headroom; re-run B3 to confirm flat RSS.
- [ ] **T5. Multishot accept** (graceful fallback) + re-measure B1/B2 loopback.
- [ ] **T6. Clean drain / SO_REUSEPORT zero-downtime handoff** + multi-worker.
- [ ] **T7. B9 reuseport fairness** (per-ring distribution under load).
- [ ] **T8. IPv6 + bind-specific-IP + configurable tables.**
- [ ] **T9. Optimizer rig scaffolding** (control/treatment/referee + ungameable
      relay scoring fn + anti-cheat gate). BUILT, not run.
- [ ] **T10. Public Go API proposal** (async-hook + externally-dialed-fd
      contract) — design doc for Alon's sign-off, not locked.

## Blocked (need human) — teed up, not guessed
- [!] **Run the optimizer loop** — needs Anthropic API key + ClickHouse + OK to
      spend API budget. T9 builds up to this.
- [!] **B4 throughput** — needs 10GbE / 3rd box.
- [!] **B8 NUMA scaling** — needs a dual-socket box.
- [!] **conn/s-over-NIC (B2)** — provider connection-rate throttle (documented in
      results/2box-20260620-200121/NOTES.md).
- [!] **riptide hook/fd API sign-off** — T10 proposal awaits review.

## Log
- 2026-06-20 ~20:30 — kicked off. Gate (B1+B2) PASSED earlier: B1 epoll=0
  (measurement-grade), instr/conn 1.44× real NIC, 1.58× conn/s loopback. Starting T1.
