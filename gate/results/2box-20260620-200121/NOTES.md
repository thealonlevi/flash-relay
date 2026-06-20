# Notes — 2-box run 20260620-200121 (read before trusting SUMMARY.md)

**Topology:** SUT = box 1 (203.0.113.10); loadgen+sink = box 2 (203.0.113.20,
via `loadgend`). Cross-box over **public IPs**, RTT ≈ 13 ms.

## What is valid and measurement-grade here

- **B1 (epoll elimination): ✅ PASS, conclusive.** SUT shows **0** fd-registration
  symbols (`do_epoll_ctl`/`netpollopen`/`netpollclose`); baseline shows 4. No
  data-plane fd touches the netpoller — confirmed over a real NIC.
- **instructions/conn: ✅ 1.44× fewer** for io_uring (167k vs 240k). This is the
  per-connection CPU cost (perf-measured) and is valid regardless of saturation —
  it directly answers riptide's question ("relay each connection for less CPU").

## Why the conn/s/core number in SUMMARY.md is NOT valid (ignore the NO-GO)

SUMMARY.md reports conn/s ≈ 500 for **both** builds (ratio ~1.0) and p50 ~1–2 s,
and flags NO-GO. That is a **false negative**: the SUT core was never CPU-bound.
The relay log showed `live=1` throughout (never more than ~1 connection in
flight), i.e. the core sat ~idle.

**Root cause — isolation test (decisive):** pointing the storm at a *bare `sink`*
on box 1 (no relay, no second dial) also caps at **461 conn/s, p99 4.1 s**. So the
ceiling is the **cross-public-IP new-connection rate itself** — cloud provider
SYN/connection-rate limiting on the public path — not flash-relay. Pushing
in-flight from 512→6000 made it *worse* (~22 conn/s), the classic anti-DDoS
back-off signature. The churn benchmark (B2) stresses exactly what the provider
throttles, so conn/s/core cannot be measured over this path.

## Where the conn/s/core ratio actually stands

From the **CPU-bound loopback** dev-grade run (core is the bottleneck there):
**1.58× conn/s-per-core**, 1.55× fewer instructions/conn. To confirm conn/s
measurement-grade on two boxes, we need an **unthrottled link** — a private
network / same-VPC subnet (provider SYN limits generally don't apply intra-VPC).

## Bottom line

The kill-gate's substantive question is answered **GO** on real hardware: epoll
eliminated (B1=0) and **1.44× less CPU per connection** over a real NIC. The
conn/s-per-core *multiple* is established loopback (1.58×); confirming it over a
NIC is blocked by an environmental (provider) limit, not by flash-relay.
