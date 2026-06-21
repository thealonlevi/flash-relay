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

| Profile | id | TTL | TCP options | wscale | eBPF work |
|---|---|---|---|---|---|
| Linux (relay's real stack) | 0 | 64 | `mss,sok,ts,nop,ws` (20B) | 7 | — |
| **Windows 10/11** | 1 | 128 | `mss,nop,ws,nop,nop,sok` (12B, **no TS**) | 8 | reorder + **shrink −8** ✅ |
| **macOS** 13–15 | 2 | 64 | `mss,nop,ws,nop,nop,ts,sok,eol` (24B) | 6 | reorder + **grow +4** ✅ |
| **Android** 10–14 | 3 | 64 | `mss,sok,ts,nop,ws` (**== Linux**) | 8 | **none** (sockopt only) ✅ |
| **iOS** 16/17 | 4 | 64 | == macOS layout (eBPF mark 2) | 7 | (reuses macOS) ✅ |

macOS is matched against a **live Mac capture** (`captures-macos-real.txt`). **Key:**
TTL + option *order/set* are forged by the eBPF (cosmetic); **window + wscale are
functional** and come from `SO_RCVBUF` (the kernel derives them) — needs
`net.core.rmem_max` raised (≥16 MiB): 2M→wscale 6, 4M→7, 8M→8. Android needs no eBPF
(layout == Linux). The MSS is left as the relay's real path MSS (path-dependent, not
an OS tell). Full p0f/JA4T OS-*label* confirmation needs a real NIC (loopback MSS
65495 distorts the window).

## Deploy requirements

1. `clang`/`libbpf-dev`/`iproute2` to build; `CAP_NET_ADMIN` to attach + set SO_MARK.
2. Attach the eBPF on the egress interface (`tc ... egress bpf da obj ... sec tc`).
3. `sysctl -w net.core.rmem_max=16777216` (so SO_RCVBUF can reach the target wscales).
4. The relay's hook returns the profile id; `DialFingerprint` does the rest.

## Status & benchmark

**All 4 modern profiles working + validated (loopback):** each `DialFingerprint`
profile emits the exact TTL + option layout + wscale of its OS; macOS matches the
live Mac capture byte-for-byte (modulo path-MSS); unmarked traffic passes through as
untouched Linux; handshake + data survive (incl. the grow/shrink paths).

**Cost (eBPF off vs on, 1-core loopback; three program variants measured —
rewrite-all, direct-packet-access, mark-based — all agree):**

| Aspect | Impact |
|---|---|
| Throughput (bytes/instr, MB/s) | **~0%** (within noise) |
| Connection churn (instr/conn) | **+3.6–3.9%** |

**The cost is the per-packet tc-egress *dispatch*** (the hook is invoked on every
egress packet), **not** the program logic or the rewrite — confirmed because
direct-packet-access and mark-gating (both of which slash the program's work) did
*not* move the number. Throughput is unaffected because few large packets amortize
the dispatch; churn shows it because it's more packets per unit work. It's
intrinsic to tc-egress. Only levers: a `flower` SYN-prefilter (marginal) or
`sock_ops` (connect-time only — but can't reorder options).

## Remaining work (precise approaches)

1. **macOS resize (mark 2).** Snapshot the option fields, `bpf_skb_change_tail(skb,
   len+4)`, re-fetch pointers, write the 24-byte macOS layout, set TCP data-offset
   10→11, IP total-length 60→64, recompute IP checksum. **TCP checksum** has three
   components: option bytes (`csum_diff` old20 vs new24), the data-offset byte, and
   the **pseudo-header length** 40→44 (`bpf_l4_csum_replace(..., BPF_F_PSEUDO_HDR)`).
   *Deferred:* the TCP checksum can't be validated on loopback (offload doesn't
   verify it) — needs a real-NIC capture, so it's a deploy-time task, not an
   overnight one.
2. **window/wscale fidelity (all profiles, for a full p0f OS-*label* match).** Set
   `SO_RCVBUF` on the upstream socket so the kernel emits the target wscale (and
   initial window). **Loopback can't reach it**: loopback MSS is 65495, so the
   advertised window can't be the ~8192 real OSes use — a full p0f OS-label match
   needs a real NIC (MSS ~1460). On loopback we can validate the *option layout +
   TTL* (the structural signal), which we do.
3. **Real-NIC validation.** Confirm TCP checksums under real TX offload and diff the
   emitted SYN against real macOS/Windows/Android captures (and p0f's OS label) on
   the egress box.
4. **Cut the per-packet dispatch** (if churn CPU matters): `flower` filter matching
   SYN-only so the eBPF action runs only on SYNs.
