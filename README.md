# flash-relay

[![CI](https://github.com/thealonlevi/flash-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/thealonlevi/flash-relay/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)
[![Platform: linux/amd64](https://img.shields.io/badge/platform-linux%2Famd64-555.svg)](#install)
[![CGO: disabled](https://img.shields.io/badge/cgo-disabled-success.svg)](#install)

**A pure-Go (`CGO_ENABLED=0`) io_uring TCP relay engine for Linux** — with **zero Go
netpoller on any data-plane fd**. Accept a client → run a real decision hook
(auth / blacklist / IP-alloc / dial, may block) → adopt an externally-dialed upstream
fd → splice client ↔ upstream bidirectionally with correct half-close → close.

The relay sibling of [`flashaccept`](https://github.com/thealonlevi/flashaccept) (a C
accept engine that proved ~3× fewer CPU instructions/conn). Built for
[riptide](https://github.com/thealonlevi).

```go
import "github.com/thealonlevi/flash-relay/flashrelay"

srv, _ := flashrelay.New(flashrelay.Config{Addr: "0.0.0.0", Port: 443, Workers: 40}, hook)
go srv.Run()   // blocks; one shared-nothing io_uring ring per core
defer srv.Stop()
```

---

## Why

A proxy/relay spends its life shuffling bytes between two sockets. The Go `net` package
registers every connection fd with the runtime netpoller (epoll) — per-connection
`epoll_ctl` syscalls and poller bookkeeping that a high-churn egress pays on every
connection. flash-relay does the data plane on **raw io_uring** and **never** lets a
data-plane fd (listener, client, or upstream) touch the netpoller. The blocking
decision hook and the blocking upstream dial run on goroutines that enter *blocking
syscalls* (parking the OS thread via the Go scheduler), not the poller.

The result, measured against an idiomatic netpoller relay on a single-box loopback rig:

| Benchmark | Result |
|---|---|
| **B1 — epoll elimination** | **Zero** `epoll_ctl` / `runtime.netpollopen` fd-registration symbols (the netpoller baseline has them per connection). Architectural, conclusive. |
| **B2 — CPU per connection** | **1.55× fewer instructions/conn** and **1.58× more conn/s-per-core** than the netpoller baseline; byte-audit clean across all reps. |

> Numbers are **dev-grade** (single-box loopback, CPU-isolated). Measurement-grade
> absolutes need real hardware — see [`research/`](research/).

## Features

- **Shared-nothing per-core engine** — each worker owns its `SO_REUSEPORT` listener,
  io_uring ring, hook-goroutine pool, and connection map; scales across cores with no
  shared scheduler.
- **Off-ring decision hook** — your `Hook` may block (auth, blacklist lookup, IP
  allocation, the upstream dial); a slow hook parks one connection, never the ring.
- **Correct relay semantics** — partial-send handling, proper half-close, graceful
  drain on `Stop`, idle timeout, per-direction byte counters, IPv4 + IPv6 + bind-IP.
- **TCP/IP fingerprinting (optional)** — make outbound connections present a
  **macOS / Windows / Android / iOS** TCP/IP fingerprint instead of the egress box's
  Linux stack, via an eBPF tc-egress rewrite + per-socket sockopts. See below.

## Install

```sh
go get github.com/thealonlevi/flash-relay/flashrelay
```

Requires **Linux** (io_uring) and **Go 1.25+**. Pure Go, no cgo. The build is
constrained to `linux && amd64`.

## Usage

The `Hook` is where you make the per-connection decision and dial the upstream. Return
a `Decision` that either adopts a connected upstream fd (relay) or rejects:

```go
hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
    // req = the initial client bytes (up to Config.InitialReqLen).
    // Do auth / blacklist / IP-alloc here — it may block; it runs off-ring.
    fd, err := flashrelay.Dial("10.0.0.9", 443) // blocking raw-syscall dial; never the netpoller
    if err != nil {
        return flashrelay.Decision{Reject: true, Reply: []byte("upstream unavailable\n")}
    }
    return flashrelay.Decision{UpstreamFD: fd}
}

srv, err := flashrelay.New(flashrelay.Config{
    Addr: "0.0.0.0", Port: 443, Workers: 40, IdleTimeout: 5 * time.Minute,
}, hook)
if err != nil { log.Fatal(err) }
go srv.Run()
// ... later: srv.Stop() drains in-flight connections, Run() returns.
```

A complete, runnable embedding is in [`examples/echo-relay/`](examples/echo-relay):

```sh
nc -lk 9000 &                                                   # an upstream
go run ./examples/echo-relay -port 8080 -upstream 127.0.0.1:9000
```

### Public API

Five entry points, one handler type, one config (zero-value fields take documented
defaults). Full reference: [`docs/API.md`](docs/API.md) or `go doc ./flashrelay`.

| Symbol | Purpose |
|---|---|
| `New(cfg, hook) (*Server, error)` | Validate config, build the engine. |
| `(*Server).Run() error` | Start workers; blocks until `Stop`, drains, returns. |
| `(*Server).Stop()` | Async-signal-safe graceful shutdown. |
| `(*Server).Stat() Stats` | Counter snapshot (accepted / completed / bytes / live …). |
| `Dial(host, port) (int, error)` | Blocking raw-syscall upstream dial (off-poller) for a `Hook`. |
| `DialFingerprint(host, port, profile)` | Dial with a chosen OS TCP/IP fingerprint (see below). |
| `Hook`, `Decision`, `Config`, `Stats` | The handler type, its return, config, counters. |

## TCP/IP fingerprinting

The egress (dial-to-upstream) connections can present a chosen OS's TCP/IP SYN
fingerprint instead of Linux's. A `tc`-egress eBPF program rewrites the outbound
packets per-connection (selected by `SO_MARK`), and the kernel supplies the functional
fields via `SO_RCVBUF` / `IP_TOS` / `tcp_ecn` — so the disguise is *coherent*, not a
half-spoof.

```go
fd, _ := flashrelay.DialFingerprint("10.0.0.9", 443, flashrelay.FPMacOS)
return flashrelay.Decision{UpstreamFD: fd}
```

Four profiles, each **matched byte-for-byte against a real device** (MacBook M4 Pro,
iPhone 17 Pro Max, Windows 10/11, a cellular Android), coherent across the whole flow
(TTL + IP ID on every packet, not just the SYN):

| Profile | TTL | TCP options | wscale | IP ID | extras |
|---|---|---|---|---|---|
| `FPWindows` | 128 | `mss,nop,ws,nop,nop,sok` (no TS) | 8 | incrementing | — |
| `FPMacOS` | 64 | `mss,nop,ws,nop,nop,ts,sok,eol` | 6 | 0 | — |
| `FPAndroid` | 64 | `mss,sok,ts,nop,ws` (== Linux) | 9 | random | — |
| `FPiOS` | 64 | == macOS layout | 6 | random | ECN, DSCP 0x50 |

Cost: **≤ 5% instructions/connection** (Windows +1.7%, Android +2.8%, macOS/iOS +5.2%),
no throughput impact, all relay correctness gates green. Full design, the eBPF, deploy
requirements, validation, and benchmark: [`fingerprint/`](fingerprint/).

## Build & test

```sh
CGO_ENABLED=0 go build ./...                 # library + examples + research rig
go test ./flashrelay/ ./internal/...         # relay path, ring (recv/send/accept/splice/timeout), rawsock v4+v6
go run ./examples/echo-relay -port 8080 -upstream 127.0.0.1:9000
```

## Layout

```
flashrelay/     the public library      — API + the per-core ring engine (worker.go)
internal/
  uring/        hand-rolled pure-Go io_uring ring (unsafe/mmap/barrier hot path)
  rawsock/      TCP sockets via raw syscall.Socket only — never net (the non-negotiable)
fingerprint/    the TCP/IP fingerprint feature: eBPF tc-egress rewrite + docs + benchmark
examples/       echo-relay — a minimal embedding
docs/           API.md, the riptide handoff
research/        how it was found: the measurement rig (gate/) + autonomous optimizer
```

The library is the **distilled artifact**; [`research/`](research/) is **how it was
found** — a closed-loop optimizer that mutated the io_uring engine and kept only
changes that lowered measured instructions/connection (the same idea as flashaccept's
research rig). The gate harness scores every accept-path change; don't regress the hot
path without a number.

## Status

Gate ✅ **GO** (dev-grade): B1 epoll-elimination is conclusive on this rig; B2 is the
dev-grade ratio. The importable library is built and tested; the fingerprint feature is
built, validated, and benchmarked. Measurement-grade absolutes (and the B3–B9 suite)
need real hardware. See [`research/`](research/) (the rig that found it),
[`research/gate/DESIGN.md`](research/gate/DESIGN.md) (measurement contract), and
[`docs/RIPTIDE-HANDOFF.md`](docs/RIPTIDE-HANDOFF.md) (honest handoff with caveats).

## License

[MIT](LICENSE) © Alon Levi.
