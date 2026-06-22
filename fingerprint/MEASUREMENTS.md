# Behavioral-tell measurement: relay's Linux stack vs real-device captures (2026-06-22)
# Method: sustained loopback flow, tcpdump -tt -v, parse TSval cadence / IP ID / window.

## TS clock cadence
  Linux relay: 1000 Hz (1ms tick), per-conn origin randomized (e.g. TSval base 773956176).
  Modern macOS/iOS/Android: ~1000 Hz. => MATCHES, no rewrite needed.

## IP ID (all DF)
  Linux relay: incrementing +1 (e.g. 983,984,985,...).
  Real macOS: 0 (5/5 samples).  Real iOS: 25100 (random).  Real Android: 54,29595,385 (random).  Real Windows: 25535/49517 (incrementing).
  => macOS rewrite to 0; iOS/Android randomize; Windows keep. Done in eBPF, every packet.

## TTL coherence
  Pre-fix: TTL rewritten on SYN only -> Windows leaked TTL 64 on data packets. Fixed: every-packet TTL.

## Window
  SO_RCVBUF pins the buffer (disables autotuning/DRS); window still fluctuates with occupancy (34805..7164).
  Real OSes grow the window in early RTTs. Weak, hard-to-probe tell -> deferred.

## Per-packet eBPF cost (every-packet TTL+IPID rewrite)
  ~+3.9% system-wide instructions (alternating ON/OFF, medians, 60k pkts/run) — same as SYN-only; dispatch-bound.
