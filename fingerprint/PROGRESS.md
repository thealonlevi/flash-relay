# Fingerprint optimization — overnight autonomous run (2026-06-21 night)

## #1 cut per-SYN cost (direct packet access) — DONE, with a finding
Rewrote eBPF to direct packet access (no load/store_bytes helpers), pull_data
moved to SYN-only path. FINDING: did NOT reduce the overhead. The cost is the
per-packet tc-egress DISPATCH (running the hook on every egress packet), not the
program's logic — so micro-optimizing the program barely moves it. Measured
overhead ~3-5% on churn + throughput, near the noise floor of this degraded box
(66k CLOSE-WAIT). Real lever to cut it = don't run eBPF per-packet (flower
SYN-prefilter, or sock_ops for non-reorder fields). Documented as future work.

## #2 real per-OS profiles (from p0f.fp canonical sigs)
- Windows 7/8/10: TTL 128, opts mss,nop,ws,sok,ts (20B, in-place) + win 8192 wscale 2
- Android:        TTL 64,  opts mss,sok,ts,nop,ws (SAME as Linux, no reorder) + win mss*44 wscale 3
- macOS 10.x:     TTL 64,  opts mss,nop,ws,nop,nop,ts,sok,eol+1 (24B, GROW +4 via skb_change_tail)
CONSISTENCY: TTL + option order = free (eBPF). window + wscale = functional, must
be set kernel-side via setsockopt(SO_RCVBUF) on the relay's upstream socket (ties
to #3) or the connection desyncs. Full p0f OS-match needs both.

## #3 per-conn selection via SO_MARK
eBPF map keyed on skb->mark -> profile; relay hook sets SO_MARK (+ SO_RCVBUF for
window/wscale) on the upstream socket before connect.

## #4 p0f fidelity validation
Use p0f -i lo as oracle; target p0f.fp sigs so match-by-construction.

## STATUS
- [x] #1 (done + finding)
- [x] #2 Windows profile (eBPF, in-place 20B) ; Android = sockopt-only (no eBPF) ; [ ] macOS resize (skb_change_tail)
- [x] #3 SO_MARK per-conn selection: eBPF switches on skb->mark; rawsock.DialMark + hook.Mark + SUT -fpmark + flashrelay.DialFingerprint
- [~] #4 p0f: rewrite confirmed (Windows opts+ttl via tcpdump/p0f raw_sig). FULL OS-LABEL match needs window+wscale which need real MSS (loopback MSS 65495 can't produce win 8192) -> real-NIC item.

## Benchmark conclusion (3 variants: rewrite-all/direct-access/mark-based)
All ~same: churn +3.6-3.9% instr/conn, throughput ~0% (within noise). Cost = per-packet tc-egress DISPATCH (hook invoked per egress packet), NOT program logic/rewrite. Intrinsic to tc-egress. Only lever: flower SYN-prefilter (marginal) or sock_ops (can't reorder opts). Throughput unaffected because few big packets amortize dispatch.
