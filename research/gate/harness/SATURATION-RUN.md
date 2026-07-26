# The saturation run — prepped, not yet executed

*The one experiment that decides whether flash-relay's headline is "less CPU per
connection" (incremental) or "no collapse cliff" (the SLA-saving one).*

## The question

A production 80-core node took a connect-flood and did not plateau — it **tipped**:
~34k conn/s at ~90% CPU, then past that, CPU pinned at 94.5% while useful throughput
fell to ~8k conn/s. That super-linear collapse is what costs the SLA, and eliminating it
is worth far more than the 1.4–2× per-connection CPU win.

flash-relay removes the Go-runtime doorways that produce that collapse (shared
scheduler, netpoller, GC). The open question is whether the cliff **disappears** or
merely **relocates** to the kernel doorway underneath it — the `SO_REUSEPORT` accept
path. The earlier 6-core run already showed flash-relay's kernel lock contention *rising*
(7.6% vs the netpoller's 4.3%), which is exactly the shape of a relocating bottleneck.

**A collapse is a shape, not a point** — it is only visible as conn/s vs cores across the
knee. So the experiment is a sweep, not a single run.

## What was built

| file | role |
|---|---|
| `topology.sh` | NUMA/SMT-aware core selection (sourceable helpers) |
| `saturation-sweep.sh` | sweeps core counts, runs both builds per point, tabulates the curve |
| `multicore.sh` | unchanged single-point runner; now accepts a `RELCORES` override |

### Why topology.sh exists (read this before trusting any number)

On a dual-socket Xeon the kernel numbers logical CPUs **interleaved across sockets**, and
SMT siblings sit half the range apart. On the 2×28-core 8176M this was prepped on:

```
node0 = even CPUs 0,2,…,110        node1 = odd CPUs 1,3,…,111
SMT siblings: cpu N pairs with cpu N+56   → (0,56) (1,57) (2,58) …
```

So `multicore.sh`'s historical `RELCORES=$(seq 0 N-1)` puts the relay on cores
`0,1,2,3…` — **straddling both sockets and doubling up SMT siblings**. That folds NUMA
traffic and SMT contention into the lock-contention signal and makes the result
uninterpretable: you cannot tell whether a flattening curve means the reuseport lock
(#3) or cross-socket memory (#4). The sweep therefore places relay rings one per
**physical** core on **one** socket by default, and load generation on the other.

`RELAY_NUMA=both` deliberately spans sockets — that is the #4 NUMA measurement, run as
its **own** curve to compare against the single-socket one, never mixed into it.

## Running it

Always dry-run first — it prints every sweep point's placement and executes nothing:

```sh
DRYRUN=1 SWEEP="1 2 4 8 16 28" bash research/gate/harness/saturation-sweep.sh
```

Then the real runs (root: needs `perf` + `taskset`):

```sh
# 1. The headline curve — one socket, isolates the reuseport accept path (#3).
sudo env SWEEP="1 2 4 8 16 28" JUNK=93 INFLIGHT=8000 \
     bash research/gate/harness/saturation-sweep.sh

# 2. The NUMA curve (#4) — same sweep, rings spanning both sockets. Compare to #1.
sudo env RELAY_NUMA=both SWEEP="1 2 4 8 16 28" JUNK=93 INFLIGHT=8000 \
     bash research/gate/harness/saturation-sweep.sh
```

`JUNK` is the junk/reject mix — zero-byte connect-flood connections that connect and
close without ever reaching upstream. Set it to match the production incident (the
dc-stable-1 profile is connection-rate-heavy at ~90%+ reject; the original ISP incident
was 93%). This is the knob that makes the test reflect the real failure mode rather than
a clean stream.

Results land in `results/saturation-<timestamp>/SWEEP.txt`.

## Reading the result

Per sweep point, per build: `conn/s`, CPU cores actually used, conn/s per core used,
kernel lock contention %, Go scheduler %, netpoller %.

| pattern | conclusion |
|---|---|
| uring conn/s keeps climbing with N, lock% flat | **collapse eliminated** — this is the headline |
| uring conn/s flattens **and** lock% climbs | **collapse relocated** to the reuseport accept path (#3); the real ceiling, now named and targetable |
| uring's knee at higher N than netpoll's | quantifies the win even if a ceiling remains |
| `both` curve materially worse than `single` | NUMA (#4) is a live constraint; ring/NIC-queue pinning needed |

The netpoller baseline must show its **own** knee in the same sweep. If it does not, the
load source is not pushing hard enough and neither curve means anything.

## Limits of this rig — do not overstate the result

1. **Loopback.** This measures the *shape* of scaling — reuseport contention, NUMA
   behavior, where each build's knee sits. It does **not** measure real-NIC saturation.
   Absolute conn/s remains loadgen-bound and is not measurement-grade.
2. **The loadgen must out-supply the relay.** The sweep gives loadgen up to 2× the
   relay's cores and **warns** when it cannot. On a 2×28-core box that means points past
   ~14 relay cores are already under-supplied, and the number you read at N=28 may be the
   *loadgen's* ceiling rather than the relay's. Past that, a second load box is required —
   see `DEPLOY-LOADGEN.md`. Treat any warned point as indicative only.
3. **Single-socket sweep caps at the node's physical core count** (28 here). Reaching the
   80-core target needs the target box.
4. **Not safe beside production traffic.** The sweep saturates every core it is given and
   floods loopback. The box this was prepped on is also a live node; nothing here has
   been executed against it.

## Status

Built and dry-run verified (placement, disjointness, warnings). **Not executed** — no
saturation numbers exist from this rig yet.
