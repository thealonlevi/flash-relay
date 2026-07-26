# Saturation sweep v3 — with a load generator that can actually feed the SUT

**Date:** 2026-07-26 · **Box:** 2×20-core Xeon Gold 6230 (40 physical cores, SMT on),
kernel 6.8.0-111 · **Profile:** connect-flood `junk=93%`, `inflight=1200`, 16-port span ·
**Placement:** relay on node0 physical cores, sink + loadgen on node1 (disjoint)

Supersedes `../sat-104016/SUMMARY.md`, whose central claim was an artifact of an
under-powered load generator. See "What this run corrects".

## The curve

| N | build | conn/s | CPU used | conn/s per core | lock | sched | netpoller |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | netpoll | 32,652 | 1.09 | 29,956 | 4.3% | 2.4% | 1.5% |
| 1 | **uring** | **56,866** | 1.10 | **51,697** | 7.0% | 0.3% | **0.0%** |
| 2 | netpoll | 61,421 | 2.18 | 28,175 | 4.1% | 2.2% | 1.2% |
| 2 | **uring** | **109,361** | 2.18 | **50,166** | 7.1% | 0.3% | **0.0%** |
| 4 | netpoll | 116,457 | 4.25 | 27,402 | 4.5% | 2.0% | 1.1% |
| 4 | **uring** | **206,517** | 4.39 | **47,043** | 7.3% | 0.4% | **0.0%** |
| 8 | netpoll | 207,105 | 8.56 | 24,195 | 6.3% | 2.0% | 1.4% |
| 8 | **uring** | **379,542** | 8.83 | **42,983** | 7.5% | 0.4% | **0.0%** |
| 16 | netpoll | 274,219 | 16.96 | 16,169 | 28.8% | 1.5% | 1.2% |
| 16 | **uring** | **406,171** | 14.94 | **27,187** | 13.9% | 0.4% | **0.0%** |
| 20 | netpoll | 271,033 | 21.03 | 12,888 | **42.1%** | 1.2% | 1.0% |
| 20 | **uring** | **407,016** | 16.34 | **24,909** | 14.4% | 0.6% | **0.0%** |

Per-core ratio (uring ÷ netpoll): **1.73× · 1.78× · 1.72× · 1.78× · 1.68× · 1.93×**

## The baseline collapses; flash-relay plateaus

**netpoll goes backwards.** From N=16 to N=20 it consumed **24% more CPU** (16.96 → 21.03
cores) and delivered **less throughput** (274,219 → 271,033). Negative return on four
additional cores, with 42.1% of all CPU in lock contention. That is the cliff — not a
plateau, an actual regression under added parallelism.

**flash-relay flattens but never regresses**: 379,542 → 406,171 → 407,016 across
N=8/16/20, with lock contention stabilizing near 14%.

## The two contention profiles are qualitatively different

This is the part the aggregate "lock %" column obscures. At N=20:

```
netpoll   39.81%  osq_lock                          <- ONE mutex, optimistic-spin queue
           3.89%  mutex_spin_on_owner                  ~44% on a single serialization point

uring      6.89%  native_queued_spin_lock_slowpath  <- raw qspinlock, short critical sections
           4.64%  _raw_spin_unlock_irqrestore          spread over several call sites,
           3.97%  _raw_spin_lock                       no mutex anywhere in the profile
```

netpoll funnels through one sleeping lock, so added cores queue behind each other and
throughput inverts. flash-relay's contention is distributed spinlock traffic with no
single funnel — it costs throughput growth, but it cannot invert it.

**B1 holds under flood at every point:** netpoller/epoll is 0.0% for the SUT, 1.0-1.5%
for the baseline.

## What this run corrects

`sat-104016/SUMMARY.md` concluded flash-relay's lock contention was "**flat at ~8% across
the whole 1→16 core range**" and that the collapse was eliminated rather than relocated.
**That was an artifact of insufficient load.** The old load generator dialed via
`net.Dial`, so it carried the netpoller ceiling it exists to measure (13.3% `osq_lock` in
its own profile) and capped near 245k conn/s. flash-relay was never pushed hard enough to
reach its knee, so the curve looked flat because nothing was straining it.

With a netpoller-free junk path (+64% load from identical cores), flash-relay's lock
contention **does** rise: 7.5% at N=8 → 13.9% at N=16 → 14.4% at N=20, alongside a 37%
per-core efficiency drop from N=8 to N=16. The `SO_REUSEPORT` accept path does begin to
bite at 16+ cores.

The accurate claim is therefore **delayed and bounded, not eliminated**: at N=20
flash-relay carries a third of the baseline's lock contention (14.4% vs 42.1%), delivers
1.50× the throughput on 22% less CPU, and — decisively — does not invert.

## Still not flash-relay's ceiling

`uring` returned **406,171** at N=16 and **407,016** at N=20 — a 0.2% difference across
four extra cores, and it used only 14.94/16 and 16.34/20 of its allotted CPU (93%, 82%).
A standalone A/B measured this load generator's own maximum at ~398k conn/s. **The
plateau is the load generator's ceiling, not the relay's**, and the sweep printed the
loadgen warning at N=20.

So the N≤8 points are clean (both builds saturated, ratio 1.72-1.78×) and the N=16/20
points are **lower bounds** for flash-relay. Its true knee is still unmeasured — the
rising lock% is real (larger reuseport groups), but how far throughput would have kept
climbing is not known from this rig.

## Caveats

1. **Loopback.** This is the SHAPE of scaling, not absolute throughput. No NIC, no RSS,
   no softirq placement. Absolute conn/s is not measurement-grade.
2. **Dev-grade.** One run per point, single box, no repetition across reboots.
3. **Not comparable point-for-point with sat-104016**, which used `inflight=500` and the
   old loadgen. Two variables changed. The netpoll-vs-uring comparison *within* this run
   is clean — identical client, concurrency, and port span for both builds.
4. **The real-NIC version remains blocked** by in-path connect-flood throttling (~1k
   conn/s inbound; the same pattern runs 34.6k in reverse and the clean profile crosses
   the same path at 114k).

## Next

- **More load.** flash-relay's knee needs a second load box, or a cheaper real-connection
  path (the 7% non-junk still uses `net`).
- **The NUMA arm** (`RELAY_NUMA=both`) as its own curve, now that the load side can
  actually saturate the relay.
- **Attribute the qspinlock.** 6.9% in `native_queued_spin_lock_slowpath` is the next
  target; a `perf -g` caller breakdown would say whether it is the reuseport group, the
  accept queue, or slab.
