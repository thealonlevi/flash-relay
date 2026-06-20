# gate — the B1+B2 kill-gate

Does the io_uring relay mechanism even work? This directory answers it before we
build the full library (RELAY_PLAN.md, gate-first). Design contract:
[`DESIGN.md`](DESIGN.md).

## Build

```
CGO_ENABLED=0 go build ./gate/...     # pure Go, no cgo
go test ./gate/internal/uring/        # ring smoke tests (recv/send + accept)
```

## Run the gate (root: needs perf + taskset)

```
sudo bash gate/harness/gate.sh                      # headline (CPU-isolation)
sudo env REALISTIC=1 bash gate/harness/gate.sh      # realistic-dial (parking test)
```
> Use `sudo env VAR=…` to pass knobs — plain `sudo VAR=… bash` drops them.
> For the **2-box** measurement-grade run, see [`harness/DEPLOY-LOADGEN.md`](harness/DEPLOY-LOADGEN.md).

Knobs (env): `CORE_SUT CORE_SINK CORE_LG RPORT SPORT INFLIGHT DUR WARMUP REPS
REQLEN REPLYLEN AUTHCPU`. Results land in `gate/results/<timestamp>/` with a
`SUMMARY.md` verdict; only curated files are committed (perf binaries are not).

## Pieces

| path | role |
|---|---|
| `internal/uring` | hand-rolled pure-Go io_uring ring (the substrate the optimizer will mutate) |
| `internal/rawsock` | TCP sockets via raw syscalls only — never `net` (no netpoller on data-plane fds) |
| `internal/hook` | decision model: auth CPU spin + ms-scale async dial park + real blocking connect |
| `internal/proto` | two-fd byte-audit wire protocol |
| `cmd/relay-uring` | **SUT** — the io_uring relay probe (commit #1 of the real library) |
| `cmd/relay-netpoll` | **baseline** — netpoller relay (`net` + `io.Copy`) |
| `cmd/sink` / `cmd/loadgen` | upstream + one-shot client storm (infrastructure; may use `net`) |
| `cmd/loadgend` | loadgen **control daemon** (box 2): HTTP `/run` + in-process sink, so box 1 drives the 2-box run remotely |
| `internal/storm` / `internal/sinksrv` | shared storm + sink logic |
| `harness/` | `gate.sh` (1-box), `run-sut.sh` + `run-2box.sh` (2-box), `summarize.py`, `combine-2box.py` |

## Reading B1

B1 is a **binary architectural fact**: the SUT must have **zero fd-registration
symbols** (`do_epoll_ctl`, `runtime.netpollopen`, `runtime.netpollclose`) — those
fire per-connection only if a data-plane fd is handed to the netpoller. Note
`runtime.netpoll` + `do_epoll_wait` always appear in any Go program (the
scheduler's idle poll) and are **not** a leak — the harness ignores them.

The dev-grade run on a single-box loopback KVM is conclusive for B1 (hardware-
independent) and indicative for B2 (the SUT/baseline ratio); absolute conn/s is
loadgen/loopback-limited. Measurement-grade absolutes need real hardware.
