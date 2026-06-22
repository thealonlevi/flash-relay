# TCP/IP fingerprint rewrite (eBPF tc-egress)

Make the relay's **outbound** connections carry a chosen OS's TCP/IP SYN
fingerprint (TTL + TCP option order/set) instead of Linux's, so an upstream/
destination fingerprinting the relay (p0f-style) sees macOS / Windows 10+ /
Android rather than the egress box's real Linux stack.

**Scope:** controls only the **TCP/IP layer** (the SYN the relay sends when it
dials upstream). It does **not** touch TLS (JA3/JA4) — the client's ClientHello is
forwarded untouched, so the destination sees the *client's* TLS fingerprint. For a
believable spoof, pick the TCP profile to match the OS the client's TLS/HTTP
fingerprint claims (select it per-connection in the relay's hook).

## How it works

A tc-egress eBPF program (`bpf/syn_rewrite.bpf.c`) runs on the egress interface.
The relay sets **`SO_MARK`** on each upstream socket (the chosen profile id); the
SYN carries that as `skb->mark`, and the eBPF switches on it. **Unmarked traffic
returns on the first instruction** — only the relay's deliberately-marked outbound
connections are touched. On a marked SYN it rewrites the IP TTL and the TCP option
layout to the profile and fixes the checksums.

```
relay (hook) --SO_MARK=1--> upstream socket --> SYN(skb->mark=1) --> eBPF: Windows layout
```

Wiring: `rawsock.DialMark(ip,port,mark)`, `hook.Config.Mark` / SUT `-fpmark`,
`flashrelay.DialFingerprint(host,port,profile)` for the library hook.

```sh
./build.sh                                            # clang -> bpf/syn_rewrite.bpf.o
tc qdisc add dev eth0 clsact
tc filter add dev eth0 egress bpf da obj bpf/syn_rewrite.bpf.o sec tc   # attach
tc qdisc del dev eth0 clsact                          # detach
```

Needs `CAP_NET_ADMIN` + the eBPF/tc toolchain (`clang`, `libbpf-dev`, `iproute2`).

## Profiles — MODERN OSes (see RESEARCH.md for sources)

Selected via `flashrelay.DialFingerprint(host, port, profile)` (sets SO_MARK for the
eBPF layout/TTL + SO_RCVBUF for the wscale). All validated on loopback (✅).

| Profile | id/mark | TTL | TCP options | wscale | IP ID | eBPF work |
|---|---|---|---|---|---|---|
| Linux (relay's real stack) | 0 | 64 | `mss,sok,ts,nop,ws` (20B) | 7 | incrementing | — |
| **Windows 10/11** | 1 | 128 | `mss,nop,ws,nop,nop,sok` (12B, **no TS**) | 8 | incrementing | shrink −8 + per-pkt TTL ✅ |
| **macOS** 13–15 | 2 | 64 | `mss,nop,ws,nop,nop,ts,sok,eol` (24B) | 6 | **0** | grow +4 + per-pkt IPID=0 ✅ |
| **Android** 10–14 | 3 | 64 | `mss,sok,ts,nop,ws` (**== Linux**) | 9 | **random** | per-pkt IPID rand ✅ |
| **iOS** (iPhone 17 Pro Max) | 4 | 64 | == macOS layout | 6 | **random** | grow +4 + per-pkt IPID rand ✅ |

All four are matched against **real-device captures** (`captures-*-real.txt`). **Key
split:** TTL, option *order/set*, and IP ID are forged by the eBPF (cosmetic, no
desync); **window + wscale are functional** and come from `SO_RCVBUF` (the kernel
derives them) — needs `net.core.rmem_max` raised (≥16 MiB): 2M→wscale 6, 4M→7, 8M→8,
16M→9. The MSS is left as the relay's real path MSS (path-dependent, not an OS tell).
Full p0f/JA4T OS-*label* confirmation needs a real NIC (loopback MSS 65495 distorts
the window).

### Per-packet coherence (TTL + IP ID on EVERY packet, not just the SYN)

The eBPF rewrites TTL and IP ID on **every** egress packet of a marked conn, not only
the SYN. This closes two real multi-packet tells:
- **TTL on data** — the Windows profile sends TTL 128 on the SYN *and all data packets*
  (before this, only the SYN was 128 and data leaked Linux's TTL 64 — a glaring tell).
- **IP ID per OS** — from real captures (all DF): **macOS = 0**, **iOS/Android =
  random**, **Windows = incrementing** (matches Linux, left as-is). The relay's Linux
  default is incrementing, so macOS/iOS/Android needed rewriting. macOS (mark 2) and iOS
  (mark 4) share the option layout but differ here, which is why iOS has its own mark.

This is safe (egress-only header fields, no flow-control state) and stays
**dispatch-bound**: the per-packet IP rewrite adds negligible cost over the tc-egress
hook invocation that already fires per packet (see cost below). TS clock and IP-ID are
the only per-packet header tells; **TS needs no rewrite** — measured at 1000 Hz, identical
to modern macOS/iOS/Android, with Linux already randomizing the per-conn origin.

## Deploy requirements

1. `clang`/`libbpf-dev`/`iproute2` to build; `CAP_NET_ADMIN` to attach + set SO_MARK.
2. Attach the eBPF on the egress interface (`tc ... egress bpf da obj ... sec tc`).
3. `sysctl -w net.core.rmem_max=16777216` (so SO_RCVBUF can reach the target wscales).
4. `sysctl -w net.ipv4.tcp_ecn=1` so the Apple profiles request **real ECN** (the iOS
   `[SEW]` SYN bits). This is genuine ECN negotiation, not forged flags — but it is a
   **global** toggle: every outbound SYN becomes ECN-capable. That's within all modern
   OSes' real behavior (Win11/Android/macOS all support ECN; it's path-variable), so it
   doesn't make the other profiles implausible. iOS's DSCP (`tos 0x50`) is per-socket
   (`IP_TOS`), set automatically by `DialFingerprint`.
5. The relay's hook returns the profile id; `DialFingerprint` does the rest.

## Status & benchmark

**All 4 modern profiles working + validated (loopback):** each `DialFingerprint`
profile emits the exact TTL + option layout + wscale + IP ID of its OS; all four match
real-device captures (modulo path-MSS); unmarked traffic passes through as untouched
Linux; handshake + data survive (incl. the grow/shrink paths). `validate.sh` asserts
both the SYN fields **and** per-packet coherence (TTL + IP ID hold on data packets).

**Cost (eBPF off vs on, system-wide instructions, alternating runs, medians):**

| Aspect | Impact |
|---|---|
| Throughput (bytes/instr, MB/s) | **~0%** (within noise) |
| Packet-heavy / churn (instr) | **~+3.9%** |

**The cost is the per-packet tc-egress *dispatch*** (the hook is invoked on every
egress packet), **not** the program logic. The every-packet TTL+IP-ID rewrite (added
for full-flow coherence) measures the **same ~+3.9%** as the old SYN-only version —
the IP-header rewrite (a few byte writes + a 20-byte csum loop) is negligible against
the per-packet hook invocation that already fires. So full-flow coherence came
essentially for free. It's intrinsic to tc-egress; the only lever to cut the dispatch
itself is a `flower` SYN-prefilter (but we now *want* per-packet, so that's moot).

## Behavioral tells — what's closed, what remains

Measured the relay's Linux stack against the real-device captures for the deep
behavioral fields (not just the SYN):

| Tell | Finding | Action |
|---|---|---|
| **TS clock cadence** | Linux **1000 Hz**, origin already randomized — identical to modern macOS/iOS/Android | **none needed** (modern convergence) |
| **IP ID** | Linux incrementing; real macOS=0, iOS/Android=random | **rewritten** per-OS, every packet ✅ |
| **TTL on data** | was SYN-only (Windows leaked TTL 64 on data) | **rewritten** every packet ✅ |
| **Window autotuning curve** | `SO_RCVBUF` pins the buffer (no DRS growth); real OSes grow early | **deferred** — weak, hard-to-probe tell; window already fluctuates with occupancy |
| **Retransmit / RTO timing** | kernel-timer behavior, not a header field | **out of scope** — needs a userspace TCP stack |
| **Keepalive** | app-opt-in, not an OS default; faithful behavior is *off* (what we do) | **no emulation** (off is correct for short proxy conns) |

**Remaining work needs the real egress box:** the exact **window value** (loopback MSS
65495 distorts it), **TCP-checksum under real TX offload** (loopback offload masks it —
IP csum is validated), and the final **p0f/JA4T OS-label** confirmation. Run
`validate.sh <nic>` + `p0f -i <nic>` there.

### Implementation notes

- **Per-packet TTL + IP ID:** done via direct packet access on the IP header (no
  `pull_data` — the header is linear), then `ip_csum()` recompute. TCP checksum is
  untouched (TTL/IPID aren't in the TCP pseudo-header), so the per-packet path is just
  IP-header work. macOS=0, iOS/Android=`bpf_get_prandom_u32()`, Windows=keep.
- **Length-changing options (macOS grow +4, Windows shrink −8):** `bpf_skb_change_tail`,
  re-fetch pointers, write the new option layout, fix TCP data-offset, IP total-length,
  recompute IP checksum. **TCP checksum** is incremental (offload-correct): option
  bytes (`csum_diff`), the data-offset byte, and the pseudo-header length
  (`bpf_l4_csum_replace(..., BPF_F_PSEUDO_HDR)`). Verifier rule: do all direct packet
  writes *before* any csum helper (they invalidate packet pointers). The pre-pull
  TTL/IPID writes survive `pull_data`/`change_tail` (they don't touch the head bytes).
