# Modern OS TCP/IP SYN fingerprints — research + replicability (2026-06-21)

Goal: catalog the **modern** macOS / Windows / Android (+ iOS) TCP SYN fingerprints
and determine which fields the relay can replicate (eBPF option-layout/TTL +
`SO_RCVBUF` wscale/window). Sources at bottom. Where a field was confirmed against a
**real device capture** it's marked ✔real.

## What a SYN fingerprint is (p0f / JA4T fields)

`ver : initial-TTL : ipopt-len : MSS : window,wscale : option-layout : quirks : payload`
The **option layout (order + set)** is the strongest, most stable signal; TTL is
coarse (64/128/255); MSS is path-dependent (not an OS tell); window+wscale are
functional and OS-chosen. JA4T = `window_options_mss_wscale`.

## Modern signatures (consolidated, most-reliable values)

| OS (modern) | TTL | option layout | bytes | wscale | window |
|---|---|---|---|---|---|
| **macOS** 13/14/15 | 64 | `mss,nop,ws,nop,nop,ts,sok,eol` | 24 | **6** ✔real | 65535 ✔real |
| **iOS** 16/17 | 64 | `mss,nop,ws,nop,nop,ts,sok,eol` (== macOS; ends in EOL) | 24 | ~6–7 | 65535 |
| **Windows** 10/11 | 128 | `mss,nop,ws,nop,nop,sok` (**no timestamps**) | 12 | 8 | 64240 / 65535 |
| **Android** 10–14 | 64 | `mss,sok,ts,nop,ws` (**== Linux**) | 20 | 8–9 | 65535 |
| Linux (our relay) | 64 | `mss,sok,ts,nop,ws` | 20 | 7 | (varies) |

Confirmed distinguishers from the sources: **Windows omits TCP timestamps** (opt 8);
**Apple (macOS/iOS) ends the option list with EOL (opt 0)**; **Unix layout is
`mss,sok,ts,nop,ws` (2-4-8-1-3)**. macOS values above are from a **live Mac capture**
(`captures-macos-real.txt`): `ttl64, win65535, wscale6, mss,nop,ws,nop,nop,ts,sok,eol`.

## What we can replicate (eBPF + SO_RCVBUF), and what we can't

**Replicable — the whole SYN fingerprint:**
- **TTL** — eBPF (cosmetic). ✓
- **Option layout (order + set)** — eBPF, including length changes vs our 20B Linux
  layout: macOS *grow* +4 (24B), Windows *shrink* −8 (12B, drop TS), Android none. ✓
- **window scale** — `SO_RCVBUF` (kernel-derived); mapped on this kernel (rmem_max=16M):
  2 MiB→wscale 6, 8 MiB→wscale 8. ✓
- **window size** — also `SO_RCVBUF`-derived; reaches real-OS values on a real NIC
  (loopback's huge MSS distorts it). ✓ on real NIC.
- **MSS** — left as the relay's real path MSS (correct: it's path-dependent, not an OS
  tell; p0f treats it as a hint/wildcard). ✓ by design.
- **DF flag** — Linux/macOS/Windows all set DF; matches already. ✓

**Not replicable (and mostly not part of the SYN fingerprint):**
- IP ID generation pattern + TCP timestamp clock/value dynamics — the relay's Linux
  values. p0f's `id+`/`ts` quirks largely match anyway; exact tsval cadence doesn't.
- Post-handshake window autotuning, RST/ICMP behavior, IP options — different kernel
  behavior we don't emulate. Not keyed by SYN fingerprinters.

**Conclusion: every modern profile the user wants (macOS, Windows 10/11, Android,
+iOS) is SYN-fingerprint-replicable** with eBPF (layout+TTL) + SO_RCVBUF (wscale+window).

## Profile implementation plan / status

| profile | mark | eBPF op | wscale (SO_RCVBUF) | status |
|---|---|---|---|---|
| macOS | 2 | reorder + **grow** 20→24 | 6 (2 MiB) | ✅ done, ✔real-validated |
| Windows 10/11 | 1 | reorder + **shrink** 20→12 (drop TS) | 8 (8 MiB) | ⏳ replacing old in-place Win7/8 |
| Android | 3 | **none** (layout == Linux) | 8 (8 MiB) | ⏳ sockopt-only |
| iOS | 4 | == macOS eBPF (mark 2 layout) | ~7 | ⏳ macOS eBPF + iOS wscale |

Caveats: wscale/window values vary by OS *build* — the table uses common modern
values; real-device captures (like the macOS one) pin them exactly. Full p0f/JA4T
OS-label confirmation needs a **real NIC** (loopback MSS 65495 distorts window).


## Real-device captures (provenance)
- **macOS** — MacBook **M4 Pro** (2025, latest macOS): TTL64, win65535, **wscale 6**, `mss,nop,ws,nop,nop,ts,sok,eol`, plain SYN.
- **iOS** — iPhone **17 Pro Max** (2025, latest iOS): identical layout + **wscale 6** (research had guessed 7), PLUS **ECN** (SYN ECE+CWR) and **tos 0x50** (DSCP). So on current Apple hardware iOS == macOS at the option layer; iOS additionally requests ECN (a deploy sysctl: net.ipv4.tcp_ecn) and marks DSCP (IP_TOS sockopt). Both off-by-default on our relay.
- **Windows 10/11** (real, via the device's network): TTL128, win65535, wscale 8, `mss,nop,ws,nop,nop,sok` (no TS) — matches FPWindows exactly.

## Sources
- FoxIO JA4T — https://blog.foxio.io/ja4t-tcp-fingerprinting (Windows no-TS; iOS EOL; format)
- pydoll network-fingerprinting — https://pydoll.tech/docs/deep-dive/fingerprinting/network-fingerprinting/ (per-OS p0f sigs)
- incolumitas TCP/IP fingerprinting — https://incolumitas.com/2021/03/13/tcp-ip-fingerprinting-for-vpn-and-proxy-detection/ (real Android/Linux samples)
- java2depth TCP/IP fingerprinting (2025) — https://java2depth.blogspot.com/2025/10/tcpip-fingerprinting.html
- p0f v3 — https://lcamtuf.coredump.cx/p0f3/ ; nmap OS detect — https://nmap.org/book/osdetect-methods.html
- live macOS capture — fingerprint/captures-macos-real.txt
