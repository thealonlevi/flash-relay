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

## Profiles (canonical p0f.fp signatures)

| Profile | TTL | TCP options | window,wscale | eBPF work |
|---|---|---|---|---|
| Linux (relay's real stack) | 64 | `mss,sok,ts,nop,ws` (20B) | *,7 | — |
| **Windows 7/8/10** (mark 1) | 128 | `mss,nop,ws,sok,ts` (20B) | 8192,2 | TTL + in-place reorder ✅ |
| **macOS 10.x** (mark 2) | 64 | `mss,nop,ws,nop,nop,ts,sok,eol+1` (24B) | 65535,* | reorder + **grow +4** (TODO) |
| **Android** (mark 3) | 64 | `mss,sok,ts,nop,ws` (**same as Linux**) | *,3 | **none** — sockopt only |

**Key:** TTL + option *order* are cosmetic → forge freely in eBPF. **window +
wscale are functional** — forging them in the SYN alone desyncs flow control, so
they must come from the kernel via `setsockopt(SO_RCVBUF)` on the upstream socket
(the kernel derives wscale + initial window from the receive buffer). Android needs
*no* eBPF (its option layout == Linux); it's purely a window/wscale (sockopt) job.

## Status & benchmark

**Working + validated (loopback):** Windows profile — `SO_MARK=1` connection emits
TTL 128 + `mss,nop,ws,sok,ts`, IP checksum correct, handshake + data survive;
unmarked connections pass through as untouched Linux. Per-connection selection via
`SO_MARK` works end-to-end through the relay.

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
