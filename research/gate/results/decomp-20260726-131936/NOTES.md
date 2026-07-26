# Where the 8-core scaling loss actually lives

**Question.** flash-relay delivers 61,173 conn/s on 1 core but only ~47,000 per core
on 8 (≈76-80% efficiency). Is that the relay's design, or the machine?

**Method.** Two configurations with identical kernel-visible structure, cores, offered
load and memory traffic — differing *only* by the process boundary:

- **B** — ONE process, 8 ring workers, cores 0-7, 16 listen ports
- **A** — EIGHT processes, 1 ring worker each, one core each, same 16 ports

Both present 16 ports x 8 listeners = 128 listeners, an 8-member `SO_REUSEPORT`
group per port. What differs is only what a single process shares: address space,
connection map, hook pool, Go runtime/GC, and the per-connection global atomics.

## Result (two runs)

| config | conn/s | CPU | per-core | IPC |
|---|---:|---:|---:|---:|
| B — 1 proc, 8 workers | 377,272 / 366,836 | 8.03 / 7.91 | 47,159 / 45,854 | 0.820 / 0.828 |
| A — 8 procs, 1 worker | 390,768 / 387,726 | 8.06 / 7.96 | 48,846 / 48,466 | 0.834 / 0.840 |

**A beats B by 3.6% and 5.7%** — same direction both runs, with IPC consistently
~0.013 higher. CPU consumed is identical (8.03/7.91 vs 8.06/7.96), so this is
efficiency, not extra budget.

Against the 1-core baseline of 61,173 conn/s/core:

```
  1 process,  8 workers : 76% efficiency
  8 processes, 1 worker : 80% efficiency      <- perfectly independent
  1 core                : 100% (by definition)
```

## Conclusion

Of the ~24% per-core loss at 8 cores, **only ~4-6 points come from sharing a
process**. The remaining **~20 points survive total process independence** — eight
programs with separate address spaces, heaps, runtimes and counters still lose it.

That loss is therefore hardware and shared-kernel: memory/LLC contention (IPC 0.95
at 1 core vs 0.83 at 8), plus kernel structures every process shares regardless —
the accept path, slab, port allocation.

**90%+ at 8 cores is not reachable by changing the relay's code on this rig.** The
ceiling with perfect independence is ~80%. flash-relay's shared-nothing design is
already close to optimal for this workload; it is the machine that stops scaling,
not the program.

This is consistent with the earlier instruction count: instructions/conn is flat
from 1 to 8 cores (50,246 -> 51,101, +1.7%). The relay does the same *work* per
connection at 8 cores; those instructions simply retire more slowly.

## The 4-6% that IS recoverable

The most likely source is the per-connection global atomics: `gCompleted` and
`gAccepted` are incremented once per connection by every worker, so eight cores
ping-pong two shared cache lines ~400,000 times a second. Per-worker counters
summed at read time would remove that entirely.

Worth noting `gAccepted` was added earlier in this campaign (for the statsfile
`accepted=` field) and increments unconditionally, so part of this cost is
self-inflicted. It is cheap to fix and worth roughly what the whole
process-separation buys.

## Caveat

The load generator runs on the same box (node1) in both configurations, so its
memory traffic depresses *both* equally. The A-vs-B comparison is clean; the
absolute 80% may be pessimistic and could improve with load arriving over a NIC
instead.
