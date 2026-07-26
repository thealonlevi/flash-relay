> **SUPERSEDED by `../sat-fast-110048/SUMMARY.md`. One central claim below is wrong.**
> This run concluded flash-relay's lock contention was "flat across a 16x core range" and
> that the collapse was eliminated rather than relocated. That was an artifact of an
> under-powered load generator: the client dialed via `net.Dial` and so carried the
> netpoller ceiling it exists to measure, capping near 245k conn/s. flash-relay was never
> pushed hard enough to reach its knee, so the curve looked flat because nothing strained
> it. With a netpoller-free junk path (+64% load, same cores), its lock contention does
> rise — 7.5% at N=8 to 14.4% at N=20. The corrected claim is *delayed and bounded*, not
> eliminated. Everything below about the BASELINE's collapse still holds and was
> reproduced more strongly in the newer run (netpoll goes backwards past N=16).

# Saturation sweep — the collapse-cliff question, answered (loopback, dev-grade)

**Date:** 2026-07-26 · **Box:** 2×20-core Xeon Gold 6230 (40 physical cores, SMT on,
2 NUMA nodes), kernel 6.8.0-111 · **Profile:** connect-flood, `junk=93%`,
`inflight=500`, 16-port span · **Placement:** relay on node0 physical cores, sink +
loadgen on node1 (disjoint, one logical CPU per physical core)

This is the run `SATURATION-RUN.md` was prepped for and had never executed.

## The question

The netpoller baseline is known to *tip* rather than plateau under a connect-flood.
flash-relay removes the Go-runtime doorways (shared scheduler, netpoller, GC). Does the
cliff **disappear**, or merely **relocate** to the kernel doorway underneath it — the
`SO_REUSEPORT` accept path?

## Result

| N | build | conn/s | CPU used | conn/s per core used | kernel lock | sched | netpoller |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | netpoll | 33,566 | 1.09 | 30,795 | 4.3% | 1.8% | 1.0% |
| 1 | **uring** | **37,288** | 1.06 | **35,177** | 7.4% | 0.3% | **0.0%** |
| 2 | netpoll | 63,863 | 2.17 | 29,430 | 4.7% | 1.7% | 1.0% |
| 2 | **uring** | **75,252** | 2.11 | **35,664** | 7.8% | 0.4% | **0.0%** |
| 4 | netpoll | 107,831 | 4.23 | 25,492 | 4.9% | 1.9% | 1.1% |
| 4 | **uring** | **138,972** | 4.23 | **32,854** | 8.0% | 0.4% | **0.0%** |
| 8 | netpoll | 203,191 | 8.57 | 23,710 | 8.2% | 1.9% | 1.4% |
| 8 | **uring** | **227,406** | 7.67 | **29,649** | 8.1% | 0.5% | **0.0%** |
| 16 | netpoll | 234,088 | 16.93 | 13,827 | **35.1%** | 1.4% | 1.3% |
| 16 | **uring** | **245,521** | **10.35** | **23,722** | **7.9%** | 0.9% | **0.0%** |

`conn/s` is total connections handled (junk + completed), which is the flood's actual
rate. Both builds face an identical workload and an identical port span.

## Reading

**The collapse is real, and it is the baseline's.** netpoll from N=8 to N=16 doubled its
cores and bought **+15% throughput** (203k → 234k) while per-core efficiency fell **42%**
(23,710 → 13,827). At N=16 it burned 16.93 cores. That is the tip, not a plateau.

**It did not relocate — it is absent in flash-relay.** The decisive column is kernel lock
contention across the sweep:

```
uring    7.4% -> 7.8% -> 8.0% -> 8.1% -> 7.9%     FLAT across a 16x core range
netpoll  4.3% -> 4.7% -> 4.9% -> 8.2% -> 35.1%    climbs, then explodes
```

flash-relay *starts* with higher absolute lock contention at N=1 and ends with less than
a quarter of the baseline's at N=16, because its figure does not grow with core count.

**What is contending, by name.** netpoll at N=16:

```
32.56%  osq_lock                 <- optimistic spin-queue (mutex) contention
 4.79%  mutex_spin_on_owner
```

37% of all CPU spinning on a kernel mutex. flash-relay's top symbol at the same point is
`_raw_spin_unlock_irqrestore` at 5.57%, with no `osq_lock` anywhere in its top 8 — a flat,
distributed profile with no single serialization point. That is the shared-nothing
per-core ring design doing exactly what it was built to do.

**B1 holds under flood:** netpoller/epoll is 0.0% for the SUT at every N.

## The honest limit of this run

**flash-relay's knee was never found.** At N=8 and N=16 it used only 7.67 and 10.35 of its
allotted cores — it was never CPU-saturated, because the load generator ran out of box
first. Confirmed directly: raising the loadgen from 16 to 19 cores at N=16 moved
throughput 222,699 → 242,031 (+8.7%) and relay CPU 10.40 → 11.13. Node1 has no cores left
to give.

So `uring`'s true ceiling is **above** the ~245k conn/s measured here, and the per-core
ratios at N≥8 (1.25×, 1.72×) **understate** it. The netpoll knee is real and measured; the
uring curve is a lower bound that has not bent yet.

Two candidate ceilings were tested and **excluded**:

- *The 1-core sink.* Giving it 4 cores changed throughput 219,392 → 225,002 (+2.6%). Not
  the limit.
- *Destination port space.* Already widened to a 16-port span (see below).

## Caveats

1. **Loopback, so this is the SHAPE of scaling, not absolute throughput.** No NIC, no RSS,
   no softirq placement, no real-NIC saturation. Absolute conn/s is not measurement-grade.
2. **Dev-grade.** Single box, one run per point, no repetition across reboots.
3. **A real-NIC version of this run is currently impossible on this network.** A
   connect-flood aimed at box 1 is throttled to ~1k conn/s by an in-path device: the
   identical pattern runs 34.6k conn/s in the reverse direction, and the clean
   (non-flood) profile crosses the same path at 114k conn/s. Both endpoints have
   `-P INPUT ACCEPT` and zero conntrack entries, so neither is the cause. This also
   explains the earlier `2box-20260620-200121` invalidation (461 conn/s).

## Harness defects this run exposed

All three would have produced confident, wrong curves, and all are fixed:

1. **Single relay port capped the load generator, not the relay.** With one
   `(srcIP,dstIP,dstPort)` tuple the client's ephemeral-port allocation was the
   bottleneck: 8 loadgen cores managed 36k conn/s while the 8-core relay sat at **22%
   CPU**. A 16-port span gave **134,616 conn/s and 76% relay CPU — 3.75×**. Before this
   fix both builds pinned at ~27k and every curve was flat.
2. **`INFLIGHT=8000` was far past the knee.** At N=8, throughput is flat from 200 to 1000
   in flight and then degrades — 8000 cost 21% throughput and inflated p50 from 2.5ms to
   814ms. That measures the client's own queueing. Default is now 500.
3. **The sweep never found its own results.** `saturation-sweep.sh` runs from `harness/`
   while `multicore.sh` cds to `gate/`, so a relative `OUT` resolved to two different
   directories and every point reported "no summary" despite valid runs. Latent because
   the sweep had only ever been dry-run, and a dry run returns before that code.

## What would sharpen this

- **More load than one box can make.** flash-relay needs a second load box (or a NIC path)
  before its knee can be found at all. That is now the binding constraint on this result.
- **The NUMA arm.** `RELAY_NUMA=both` as its own curve, to separate cross-socket cost from
  the accept path.
- **A private path to a load box** would make the real-NIC version of this run possible.
