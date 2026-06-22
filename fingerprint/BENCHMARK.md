# Fingerprint feature benchmark

Measured the TCP-fingerprint feature's per-connection cost through the **project
referee** (`research/optimizer/score.sh`): the io_uring relay pinned to one core, an upstream
sink on another, a loadgen storm on four more, scoring **instructions per connection**
(frequency-independent — riptide's exact question) with anti-cheat gates that also
prove the relay still **correctly relays under load** (upstream actually dialed,
duplex intact, drop rate under ceiling).

On loopback the tc-egress eBPF runs **inline in the relay's process context**, so
`perf stat -p relay` captures the eBPF cost. Reproduce with `fingerprint/benchmark.sh`.

## Setup

- Workload: B2-style churn, reqlen 64 / replylen 256, 512 in-flight, 8 reps × 5 s each.
- Baseline = eBPF **detached**, no SO_MARK. Each profile = eBPF **attached**, relay
  dials upstream with that profile's `SO_MARK` (`-fpmark n`).
- Baseline re-run 4× to characterize noise: **143,299 instr/conn (median), within ±1.0%**.

## Results

| profile  | instr/conn | vs baseline | conn/s | gate | eBPF work per conn |
|----------|-----------:|------------:|-------:|------|--------------------|
| baseline |    143,299 |           — | ~13–15k | ok | none |
| **Windows** | 145,772 | **+1.7%** | ~13.4k | ok | SYN option shrink −8 + per-pkt TTL 128 |
| **Android** | 147,342 | **+2.8%** | ~13.6k | ok | per-pkt random IP ID (no option rewrite) |
| **macOS**   | 150,710 | **+5.2%** | ~13.2k | ok | SYN option grow +4 (`change_tail`) + per-pkt IP ID 0 |
| **iOS**     | 150,461 | **+5.0%** | ~13.1k | ok | SYN option grow +4 + per-pkt random IP ID |

`gate = ok` on every profile: connections were genuinely relayed end-to-end (the
two-fd gate proves the upstream sink served ~all of them), the duplex correctness
smoke passed, and the drop rate stayed under the ceiling — i.e. the fingerprint
rewrites don't break the relay under a 13k-conn/s storm.

## Reading it

- **Cost ceiling ≤ ~5% instr/conn** (macOS/iOS), Windows cheapest at **+1.7%**.
- **The ordering tracks the eBPF work**, exactly as expected: Windows (option shrink +
  TTL, no IP-ID rewrite) < Android (per-packet IP-ID only) < macOS/iOS (the `change_tail`
  option *grow* + per-packet IP-ID). The `change_tail` SYN resize is the single most
  expensive piece, amortized over a short churn connection.
- **conn/s is unaffected** (varies 13–15k with no correlation to profile — pure noise).
  The cost is purely **CPU-per-connection**, not a throughput hit: few packets per
  short conn, and the per-packet IP rewrite is dwarfed by the tc-egress dispatch that
  already fires. This matches the packet-level number (~+3.9% on a packet-heavy flow).
- These are **instr/conn** on loopback (CPU-isolated). On a real NIC the absolute
  numbers shift but the *relative* per-profile cost holds.

Bottom line: full per-OS fingerprinting (SYN layout + TTL + IP ID, coherent across the
whole flow) costs **under ~5% CPU per connection** and passes every correctness gate.
