# Gate result — MEASUREMENT-GRADE (2-box, real NIC)


## Verdict

- **B1 (epoll elimination):** ✅ PASS — SUT fd-registration symbols **0** (0.000%); baseline **4** (0.47%).
- **B2 (conn/s-per-core):** ❌ FAIL — SUT **0.98×** baseline conn/s, **1.44×** fewer instructions/conn.
- **Byte audit:** ✅ clean.

**Gate: ❌ NO-GO**


## Metrics

| metric | baseline | SUT (io_uring) | ratio |
|---|---:|---:|---:|
| instructions / conn | 239,705 | 166,859 | 1.44× |
| conn/s / core | 498 | 490 | 0.98× |
| p50 latency | 1,087,763µs | 2,103,735µs | 0.52× |
| p99 latency | 3,125,961µs | 2,210,296µs | 1.41× |
| p99.9 latency | 4,189,399µs | 4,185,237µs | 1.00× |
| B1 fd-registration symbols | 4 | 0 | — |

_conn/s + instructions/conn measured on the SUT box (perf + statsfile); latency measured on the loadgen box (client-side connect-to-first-byte)._

