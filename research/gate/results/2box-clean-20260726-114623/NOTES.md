# 2-box clean-profile attempt — the path caps it, not the relay

**Date:** 2026-07-26 · box 1 (SUT) `74.119.149.19` Ashburn, box 2 (loadgen+sink)
`207.174.105.x` Ashburn, RTT 0.178 ms · profile `junk=0` (every connection does a
full upstream dial + request + reply)

## Why this was attempted

The connect-flood (`junk=93`) profile cannot cross this network: it is throttled to
~800-1,000 conn/s inbound to box 1 while the same pattern runs 34.6k in the reverse
direction. The clean profile is *not* throttled that way (114k against a bare sink),
so it was the one real-NIC arm available — intended to find box 1's accept ceiling
over a wire instead of by loopback extrapolation.

## Result: not measurable on this path

Through the relay: **12,724 - 20,124 conn/s**, and the relay was never the limit —
1.2-2.6 of 8 cores, a flat profile (top symbol 4.08%, no hotspot), `shed=0`,
`errs=5`, no backpressure. It sat idle waiting.

The cause is the path, demonstrated with the relay removed entirely. A plain
loadgen on box 1 dialing box 2's sink:

| inflight | conn/s | p50 | p99 | p99.9 |
|---:|---:|---:|---:|---:|
| 200 | 48,108 | 1.60 ms | 40.7 ms | 417 ms |
| 1000 | 62,082 | 9.35 ms | 210 ms | 1,009 ms |
| 2000 | 48,709 | 12.4 ms | 758 ms | 2,074 ms |
| 4000 | 52,616 | 21.4 ms | 1,058 ms | 2,059 ms |

Above ~1,000 concurrent the path grows a 1-2 second tail — the shape of TCP SYN
retransmit RTO — and throughput stops scaling at ~50-60k. The same tail is present
in every relay configuration tested (p99 1.2-2.0 s at p50 28-34 ms), and is absent
from the client leg against a bare sink at the same concurrency (p99 41.9 ms).

Since every clean-profile connection requires one upstream connection, the relay
path inherits that ceiling and then compounds it: the tail latency multiplies
through a pipeline that is three round trips deep.

**So this network degrades under connection CONCURRENCY, not merely under the
connect-flood pattern.** The flood case is the extreme (~800 conn/s); the clean case
is the mild one (a retransmit tail past ~1k concurrent). Same direction, same shape.

## What box 1's accept ceiling actually is

Still only known from loopback, where nothing is in the path:

- `junk=93` churn: ~30k conn/s per core (measured to 407k on 16 cores, itself
  loadgen-bound)
- `junk=0` with a real upstream dial per connection: ~7k conn/s per core (~57k on
  8 cores)

Over this NIC we reached ~20k total, so the wire measurement is ~20x below what the
box demonstrably does locally. Nothing here characterises box 1.

## Two false leads recorded so we do not repeat them

1. **"Smaller hook pool is better."** A three-point ladder (512/2048/8192 -> 20.1k/8.3k/2.9k)
   looked monotonic. A finer ladder is bimodal: pool 64/128/256/512 gave
   19.3k/8.1k/6.8k/19.6k. The system flips between a ~20k and a ~7k regime;
   `hookworkers` is not the explanatory variable. Do not tune it from this data.
2. **"TIME_WAIT reuse is off, so the peer must not send timestamps."** Built on
   `TCPTimeWaitReuse`, which is not a kernel counter — see the netprobe fix in
   `5a9c0ce`. The real counter (`TWRecycled`) shows reuse is active. The
   64511/60 = 1,075 conn/s arithmetic that matched the earlier 1,086 collapse
   assumed reuse was off, so that agreement may be coincidence; the sink port span
   did remove the collapse, but the mechanism is not established.

## To actually get a real-NIC number

A path without the provider's equipment in it: a private VLAN between the boxes, or
box 1's second NIC (`enp96s0f1np1`, currently DOWN) cabled directly to box 2. Both
were flagged earlier; this run is the evidence that it is required rather than
merely preferable.
