# TCP/IP fingerprint rewrite (eBPF tc-egress)

Make the relay's **outbound** connections carry a chosen OS's TCP/IP SYN
fingerprint (TTL + TCP option order/set) instead of Linux's, so an upstream/
destination fingerprinting the relay (p0f-style) sees macOS / Windows 10+ /
Android rather than the egress box's real Linux stack.

**Scope:** this controls only the **TCP/IP layer** (the SYN the relay sends when
it dials upstream). It does **not** touch TLS (JA3/JA4) — the client's ClientHello
is forwarded untouched, so the destination sees the *client's* TLS fingerprint.
For a believable spoof the TCP profile should match whatever OS the client's
TLS/HTTP fingerprint claims (select the profile in the relay's per-connection hook).

## How it works

A tc-egress eBPF program (`bpf/syn_rewrite.bpf.c`) runs on the egress interface.
It touches **only pure SYN packets** — every other packet pays just a cheap
"is-this-a-SYN?" branch — and on a SYN it rewrites the IP TTL and reorders the
TCP options to the target profile, fixing the IP and TCP checksums.

```sh
./build.sh                                   # clang -> bpf/syn_rewrite.bpf.o
tc qdisc add dev eth0 clsact
tc filter add dev eth0 egress bpf da obj bpf/syn_rewrite.bpf.o sec tc   # attach
tc qdisc del dev eth0 clsact                 # detach
```

Needs `CAP_NET_ADMIN` + the eBPF/tc toolchain (`clang`, `libbpf-dev`, `iproute2`).

## Status

**Prototype + benchmark (loopback).** Current profile: TTL→128 and reorder the
Linux SYN options `[MSS, SACK_OK, TS, NOP, WScale]` → `[MSS, NOP, WScale, SACK_OK,
TS]` (same 20-byte length → in-place rewrite, no packet resize). Validated:
handshake + data survive the rewrite; IP checksum correct; TTL + option order
rewritten as intended.

### Benchmark — eBPF off vs on (single box, loopback, 1-core relay)

| Aspect | OFF | ON | Impact |
|---|---|---|---|
| Throughput, bytes/instruction | 0.772 | 0.781 | ~0% (within noise) |
| Throughput, MB/s | 1080 | 1066 | −1.3% (within noise) |
| Connection churn, instr/conn | 143,746 | 148,964 | **+3.6%** |

**Takeaway:** the per-data-packet classifier is negligible (bulk throughput
unaffected); the cost is per-connection (~+3.6% instr/conn), almost entirely the
single SYN rewrite (~5k instructions, helper-heavy). For a data-plane-dominated
proxy the overall CPU impact is small — it only lands on the accept/dial path.

## TODO (real-profile fidelity + optimization)

- **Real OS profiles** with their exact option *sets* (macOS/Windows omit or add
  options vs Linux → different total length) — needs `bpf_skb_change_tail` to
  grow/shrink the SYN + adjust TCP data-offset / IP total-length / checksums.
- **Per-connection profile selection** via an eBPF map keyed on `SO_MARK`, set by
  the relay's hook (so each client gets a TCP profile matching its TLS/HTTP claim).
- **Cut the ~5k instr/SYN**: replace the `load_bytes`/`store_bytes` helpers with
  direct packet access (bounds-checked), and minimize checksum-helper calls.
- **Real-NIC validation**: confirm TCP checksum under real TX offload (loopback
  uses CHECKSUM offload / doesn't validate TCP csum), and diff the emitted SYN
  against real macOS/Windows/Android captures with p0f for fidelity.
- **Window scale / MSS consistency**: only forge values the kernel actually uses
  (via `setsockopt`) so the SYN advertisement matches the connection's behavior.
