# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A pure-Go (`CGO_ENABLED=0`, `linux && amd64`) io_uring TCP relay engine: accept client →
run a caller-supplied decision hook (may block; does auth + dials upstream) → adopt the
returned upstream fd → splice client↔upstream with correct half-close. The defining
property is **zero Go netpoller on any data-plane fd**.

Two trees live here and they are not peers:
- **the library** (`flashrelay/`, `internal/`) — the distilled, shippable artifact.
- **the research rig** (`research/`) — the measurement gate + autonomous optimizer that
  *found* the wins. Production-grade probe code, not throwaway.

## Commands

```sh
CGO_ENABLED=0 go build ./...                 # library + examples + research rig
CGO_ENABLED=0 go vet ./...
gofmt -l flashrelay/ internal/ examples/     # CI fails if this prints anything

go test ./flashrelay/ ./internal/...         # the full unit suite (~9s; loopback + io_uring)
go test -race ./flashrelay/ ./internal/...   # needs CGO_ENABLED=1
go test ./internal/uring/ -run TestSplice -v # a single test
go test ./flashrelay/ -run TestIdleTimeout -v

go run ./examples/echo-relay -port 8080 -upstream 127.0.0.1:9000
```

CI (`.github/workflows/ci.yml`) runs build + vet + gofmt, then the suite plus `-race`.
The test job needs `io_uring_setup(2)` to be permitted on the runner; it logs the
runner's kernel and `io_uring_disabled` first so an environment failure is not mistaken
for a code failure.

### Measurement rig (needs root: `perf` + `taskset`)

```sh
sudo bash research/gate/harness/gate.sh                  # B1 + B2 vs the netpoll baseline
sudo env REALISTIC=1 bash research/gate/harness/gate.sh  # realistic ms-scale dial (parking test)
sudo bash research/gate/harness/flood.sh                 # 93%-junk connect-flood profile
sudo bash research/gate/harness/hold.sh                  # held-conn RSS slope
DRYRUN=1 bash research/gate/harness/saturation-sweep.sh  # collapse-cliff sweep: placement only
```

**Core placement is load-bearing.** On a multi-socket box, logical CPUs are usually
numbered interleaved across NUMA nodes with SMT siblings half the range apart, so a naive
`seq 0 N-1` core list straddles sockets *and* doubles up SMT siblings — which folds NUMA
and SMT effects into whatever else is being measured. Pick cores via
`research/gate/harness/topology.sh`; `assert_disjoint` guards the SUT's cores against the
loadgen's (shared cores put loadgen CPU inside the SUT's perf counts and void the run).
See `research/gate/harness/SATURATION-RUN.md`.
Pass knobs as `sudo env VAR=… bash …` — plain `sudo VAR=… bash` drops them. Results land
in `research/gate/results/<timestamp>/SUMMARY.md`.

```sh
bash research/optimizer/start.sh                    # detached hill-climb loop
touch research/optimizer/results/STOP               # stop it
```

### Fingerprint feature (needs `clang`/`libbpf-dev`/`iproute2` + CAP_NET_ADMIN)

```sh
cd fingerprint && ./build.sh                 # -> bpf/syn_rewrite.bpf.o
sudo bash fingerprint/validate.sh lo         # asserts all 4 profiles (SYN + per-packet coherence)
sudo bash fingerprint/benchmark.sh lo        # cost via the optimizer referee
```

## Architecture

### The engine (`flashrelay/worker.go`) — one shared-nothing ring per core

`Server.Run()` creates `Workers` `SO_REUSEPORT` listeners **sequentially** (concurrent
binds race the kernel's reuseport-group setup), then hands one to each `runWorker`.
Each worker is `LockOSThread`'d, optionally pinned, and owns its ring, connection map,
eventfd, and hook-goroutine pool. Nothing is shared between workers except the atomic
counters.

Connections are **map entries, not goroutines**. All state machine transitions happen in
one `switch op` over harvested CQEs. `user_data` packs `connID<<8 | opType` (`ud`/`unpack`).

**The off-ring hook bridge** is the one place with cross-goroutine coordination: the ring
worker pushes a `job` onto a channel, a pool goroutine runs the (blocking) user `Hook`
and pushes the `hookResult` back, waking the ring via an eventfd. Wakeups are
**coalesced** — only write the eventfd when no wake is outstanding. The ordering is
load-bearing: the consumer clears `notified` **before** draining, so a push after the
clear re-arms and no completion can be lost. (An optimizer-found win, ported by hand.)

### `internal/uring` — the hand-rolled ring

Pure-Go io_uring binding (no third-party dep) so the optimizer can mutate the hot path
and so no library can secretly register a data-plane fd with the netpoller. A `Ring` is
**single-goroutine-owned**; SQ tail is published with a release store, CQ tail read with
an acquire load. `SQE` field offsets mirror the kernel struct — **do not reorder them**.
Syscall numbers and mmap offsets are hardcoded for linux/amd64.

Buffers passed to any `Prep*` are read/written by the kernel asynchronously: **keep them
alive (in per-connection state) until the matching CQE is harvested.**

### `internal/rawsock` — sockets via raw syscalls only

Never the `net` package. `Dial` is a *blocking* `connect(2)`; the blocking call parks the
calling OS thread via the Go scheduler rather than registering with epoll.

## Non-negotiables

1. **No `net`/`os.File`/`net.Conn` on any data-plane fd** — listener, client, or upstream.
   A single wrap reintroduces the netpoller and invalidates B1, which is the whole thesis.
   Tests, `examples/`, and rig infrastructure (loadgen, sink) may use `net` freely.
2. **Single-shot accept + `AcceptBatch` backpressure, and the always-armed 100 ms timeout
   op.** History: multishot accept over-accepts with no flow control → CQ overflow →
   `io_uring_enter` wedges → unkillable process. That build was reverted. The armed
   timeout guarantees the worker can never block forever waiting for a CQE that won't come.
   Any change here must survive `flood.sh`.
3. **`flashrelay/` is never auto-edited.** The optimizer's `ALLOWED_PATHS` is
   `internal/uring research/gate/cmd/relay-uring`. `internal/uring` is *shared* by the
   library and the SUT, so a ring win lands in both; engine-logic wins found on the SUT
   are **hand-ported** into `flashrelay/worker.go`.
4. **Don't regress the hot path without a number.** `research/optimizer/score.sh` is the
   fixed referee (instr/conn, with anti-cheat gates: ring unit tests, byte audit, two-fd
   upstream-served check, drop ceiling, duplex smoke). `research/optimizer/config` is the
   locked rules of the game — read it, don't edit it as part of a change.
5. **The public API surface is a contract.** `New`/`Run`/`Stop`/`Stat`/`Dial`/
   `DialFingerprint` + `Hook`/`Decision`/`Config`/`Stats`; zero-value `Config` fields take
   documented defaults. Changing the handler semantics (send-then-relay, reject
   send-then-close) is a breaking change — update `docs/API.md` and the README table with it.
   `Decision` fields have strict precedence `Reject > More > UpstreamFD`; an ignored
   `UpstreamFD` must be **closed**, never dropped, or it leaks.
6. **A hook result can outlive its connection.** The hook runs off-ring and may land after
   the conn was shed, idle-closed, or failed — the ordinary case when auth is slow under a
   flood. Every branch consuming a hook result must check `cc == nil || cc.closing` and
   close any adopted fd itself. This is the shape of a real fixed bug; don't reintroduce it.
7. **`Stats` is a partition.** Every accepted conn lands in exactly one terminal bucket
   (`Completed`/`Rejected`/`Shed`/`IdleClosed`/`Errors`), enforced by `conn.counted` +
   `conn.relayStarted`. A new teardown path must charge itself to exactly one bucket —
   `TestStatsTerminalBuckets` asserts the identity.

## Measurement discipline

The rig is built to stop a benchmark flattering itself; `research/gate/DESIGN.md` is the
contract, and it's worth reading before touching anything measured.

- **B1 is binary**: zero *fd-registration* symbols (`do_epoll_ctl`, `runtime.netpollopen`,
  `runtime.netpollclose`). `runtime.netpoll`/`do_epoll_wait` appear in every Go program
  (the scheduler's idle poll) and are **not** a leak — the harness ignores them.
- **Never accept-reply-close.** Every measured connection is a real two-fd relay with a
  byte audit; a relay that short-circuits makes the run *invalid*, not merely slow.
- **The decision hook is never a no-op** — calibrated auth busy-spin (not a sleep; a sleep
  yields and understates cost) plus a real blocking dial.
- **Don't conflate churn cost with data-plane cost.** Measure throughput with persistent
  streams and connection cost with a churn workload. Mixing them was a real past error
  (it produced a bogus splice win, since corrected — see `docs/RIPTIDE-HANDOFF.md` §6).
- Current numbers are **dev-grade** (single-box loopback KVM). Say so when quoting them.

## TCP/IP fingerprinting

`DialFingerprint(host, port, profile)` makes an *outbound* connection present a macOS /
Windows / Android / iOS TCP/IP fingerprint. The split matters:

- **Forged by the tc-egress eBPF** (`fingerprint/bpf/syn_rewrite.bpf.c`, selected per
  connection by `SO_MARK`): TTL, TCP option order/set, IP ID — rewritten on **every**
  packet, not just the SYN (SYN-only leaked Linux TTL on data, a glaring tell).
- **Functional, supplied by the kernel**: window scale via `SO_RCVBUF` (needs
  `net.core.rmem_max` ≥ 16 MiB), DSCP/ECN via `IP_TOS`, real ECN via `net.ipv4.tcp_ecn=1`.
  These can't be forged in-packet without desyncing the connection.

All four profiles are matched byte-for-byte against real-device captures
(`fingerprint/captures-*-real.txt`) — if you change a profile, re-derive it from a capture
and re-run `validate.sh`, don't hand-tune the table. It shapes **only** the TCP/IP layer;
TLS (JA3/JA4) is the client's, forwarded untouched.

## Conventions

- Commit messages: short `area: imperative summary`, then a body explaining what changed,
  **why**, the problem it solves, and impact/follow-ups — assume zero prior context.
- Doc updates ship with the change: `docs/API.md` for API, `fingerprint/README.md` for
  profile/cost changes, `research/gate/results/<run>/SUMMARY.md` for measured claims.
- Public IPs in docs/tests use RFC 5737 documentation ranges (`203.0.113.x`), never real ones.

Known stale reference: `research/gate/README.md` points at `./research/gate/internal/uring/`;
the ring tests actually live at `./internal/uring/`.
