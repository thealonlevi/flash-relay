# flash-relay

**A pure-Go (`CGO_ENABLED=0`) io_uring TCP relay engine for Linux.** The relay sibling of
[`flashaccept`](https://github.com/thealonlevi/flashaccept): accept a client → run a real decision
hook → splice client↔upstream bidirectionally → close, with **no Go netpoller on any data-plane
fd** (listener, client, or upstream). Built for [riptide](https://github.com/thealonlevi).

> **Status: gate ✅ GO (dev-grade).** The B1+B2 kill-gate is built and passing on a single-box
> loopback dev rig: **B1** — the io_uring relay has **zero** epoll fd-registration symbols (the
> baseline has them); **B2** — **1.58× conn/s-per-core** and **1.55× fewer instructions/conn** than
> the netpoller baseline; byte-audit clean across all reps. B1 (epoll elimination) is conclusive
> here; B2 is the dev-grade ratio. Measurement-grade absolutes (and B3–B9) need real hardware.
> See [`RELAY_PLAN.md`](RELAY_PLAN.md), [`gate/DESIGN.md`](gate/DESIGN.md), and the latest
> [`gate/results/`](gate/results/) `SUMMARY.md`.

## The one number that decides adoption

On a relay workload, the io_uring path must clear **all three**, or riptide should not adopt:

1. **≈0 CPU** in `epoll_ctl` / `osq_lock` / `runtime_pollOpen` / `runtime_pollClose`.
2. Materially higher **conn/s-per-core** and lower **p99 connect latency** than a netpoller relay.
3. Flat **RSS-per-connection at 50k+** concurrent, no leak.

## Layout

- `RELAY_PLAN.md` — plan of record (contract, sequence, benchmark suite B1–B9, anti-fooling rules).
- `gate/` — the B1+B2 kill-gate: the io_uring relay probe, the netpoller baseline, the realistic
  decision stub, and the measurement harness. `gate/DESIGN.md` is the measurement design.

## License

TBD.
