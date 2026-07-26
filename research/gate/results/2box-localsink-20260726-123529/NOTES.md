# Real-NIC relay measurement — 4.6x, by fixing the topology rather than the relay

**Date:** 2026-07-26 · SUT `74.119.149.19` (2x20-core Xeon, relay on node0 cores
0-15, sink on node1 cores 20-35) · load from **two** boxes: Ashburn
`207.174.105.x` (RTT 0.178 ms, 3 source IPs) and Dallas `205.186.123.8`
(RTT 32.7 ms, 1 source IP, load-only) · profile `junk=0`, 16 relay listen ports,
16 local sink ports

## Result

| configuration | conn/s | relay CPU | note |
|---|---:|---:|---|
| remote sink on box 2 | 20,124 | 1.9/8 | upstream leg carried the path's 1-2s tail |
| local sink, **unpinned** | 30,514 | 5.07/16 | sink floating over the relay's own cores |
| local sink pinned, Ashburn only | 80,672 | — | p50 14.7 ms, 0 errors |
| **local sink pinned, BOTH boxes** | **93,424** | **11.21/16** | 0 errors, byte audit clean |
| both, higher concurrency | 95,134 | 11.10/16 | Dallas at its edge (13,256 errors) |

**4.6x over the previous real-NIC best**, and none of it came from changing the
relay. It came from removing things that were not the relay.

Relay cost settled at **117-120 us-core/conn**, against ~140 us-core measured on
loopback for the same clean profile. The relay is behaving normally over the wire;
earlier real-NIC runs (166 us-core, 1.9/8 cores, flat profile) were measuring
queueing, not work.

## The three fixes, in order of value

1. **Move the sink onto the SUT box (4x).** Every clean-profile connection needs one
   upstream connection, so the relay inherited the client path's concurrency tail
   and multiplied it through a pipeline three round trips deep. Keeping the upstream
   local removes that without weakening the measurement: the client leg — the thing
   under test — is still a real NIC.
2. **Pin the sink (3x).** It was started without taskset and floated across all 80
   CPUs, burning 6.95 cores *on the relay's own cores*. Combined throughput was
   30,514; pinned to node1 the same run gave 93,424.
3. **Two independent load paths (+16%).** Ashburn alone 80,672 -> both 93,424. Less
   than the 95% additivity measured against a bare sink, because box 1 now hosts the
   sink too and is the shared resource.

## What is limiting it now

- **Box 1 total capacity.** 33.96 of 40 physical cores busy: relay 11.10, sink 6.35,
  and ~16.5 of unattributed kernel/softirq. The box is again playing two roles.
- **Client path concurrency cliffs.** Ashburn holds p99 ~1.1 s past ~1k concurrent
  but keeps scaling; Dallas collapses between inflight 8,000 (33,856 conn/s clean)
  and 16,000 (1,076 conn/s, 28,549 errors).
- **The relay itself is NOT the limit** — 69% of its 16 cores at the best point.

So box 1's accept ceiling over a real NIC is **above 95k conn/s** and still not
found. But this is the first real-NIC number where the relay is doing recognisable
work rather than waiting on the network.

## Still unavailable

The connect-flood (`junk=93`) profile remains throttled to ~800 conn/s inbound from
Ashburn and ~317 from Dallas — additive at best ~1.1k, useless. The collapse-cliff
sweep stays loopback-only until there is a private path.
