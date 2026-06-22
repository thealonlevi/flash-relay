# research/ — how flash-relay was found

The library at the repo root is the **distilled artifact**. This folder is **how it was
found and tuned**: a measurement rig that scores the io_uring relay against an idiomatic
netpoller baseline, and an autonomous optimizer that mutated the engine in a closed loop
and kept only changes that lowered measured **instructions per connection**.

Same idea as [`flashaccept`](https://github.com/thealonlevi/flashaccept)'s research rig,
applied to the relay (two-fd) workload.

```
research/
├── gate/         the measurement rig — SUT, baseline, decision model, harness
│   ├── DESIGN.md       the measurement contract (read this first)
│   ├── cmd/
│   │   ├── relay-uring/    the SUT: the io_uring relay probe (the thing measured)
│   │   ├── relay-netpoll/  the baseline: a netpoller relay (net + io.Copy) — the bar to beat
│   │   ├── sink/           upstream sink server
│   │   ├── loadgen/        one-shot client connection storm
│   │   ├── loadgend/       loadgen control daemon for the 2-box run (HTTP /run)
│   │   ├── bulkgen/        bulk-throughput (B4) load generator
│   │   └── holdgen/        held-connection (RSS-slope) load generator
│   ├── internal/      hook (realistic decision model), proto (byte-audit wire format),
│   │                  storm (client storm + latency sampler), sinksrv (upstream)
│   └── harness/       gate.sh (single-box), run-sut.sh + run-2box.sh (2-box), summarize.py
└── optimizer/    the autonomous hill-climb
    ├── score.sh        the FIXED referee — scores the current tree, with anti-cheat gates
    ├── loop.sh         one mutation per iteration: mutate → score → keep iff it won
    ├── gate-suite.sh   cross-workload no-regress promotion suite
    ├── config          the locked rules of the game (workload, cores, thresholds)
    ├── schema.sql      ClickHouse schema for the iteration log
    └── MONITOR.md      supervision playbook
```

## The gate (measurement)

The gate proves one architectural fact and one performance ratio, both designed so a
toy benchmark can't flatter itself (see [`gate/DESIGN.md`](gate/DESIGN.md) for the full
anti-fooling contract):

- **B1 — epoll elimination.** The io_uring SUT must show **zero** fd-registration
  symbols (`do_epoll_ctl`, `runtime.netpollopen`/`netpollclose`); the netpoller baseline
  has them per connection. (`runtime.netpoll`/`do_epoll_wait` always appear — the Go
  scheduler's idle poll — and are *not* a leak.) Any byte-audit failure voids a run.
- **B2 — CPU per connection.** Both SUT and baseline do a real **two-fd relay** (never
  accept-reply-close) with a **realistic** decision hook (auth CPU + a real blocking
  dial, never a no-op). Score = instructions/connection (frequency-independent).

Run it (needs root for `perf` + `taskset`; pins the SUT to one core):

```sh
sudo bash research/gate/harness/gate.sh                  # headline single-box run
sudo env REALISTIC=1 bash research/gate/harness/gate.sh  # realistic-dial parking test
```

Results land in `research/gate/results/<timestamp>/` with a `SUMMARY.md` verdict.
A measurement-grade 2-box run is in [`gate/harness/DEPLOY-LOADGEN.md`](gate/harness/DEPLOY-LOADGEN.md).

## The optimizer (search)

`optimizer/loop.sh` is a hill-climb: each iteration mutates the io_uring substrate
(`internal/uring`, the only auto-editable path), scores it with the **fixed referee**
`optimizer/score.sh`, and keeps the change only if it beats the champion by the promote
threshold *and* regresses no protected metric. The referee's anti-cheat gates (ring
unit tests, byte audit, two-fd upstream-served check, drop ceiling, duplex smoke) mean
the optimizer can't win by cheating — a flattering-but-wrong mutation scores 0.

```sh
bash research/optimizer/start.sh    # launch the loop detached (writes to research/optimizer/results/)
# stop: touch research/optimizer/results/STOP
```

This is what surfaced the engine's wins (multishot accept, the flood-deadlock fix, …).
The `score.sh` referee doubles as the project's benchmark harness — the fingerprint
feature's cost is measured by driving it across profiles (see
[`../fingerprint/benchmark.sh`](../fingerprint/benchmark.sh)).

> The rig is **production-grade probe code, not throwaway** — the `unsafe`/barrier hot
> path in the SUT carries the same care as the library. Don't regress it without a number.
