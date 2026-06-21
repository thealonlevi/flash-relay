// syn_rewrite.bpf.c — tc-egress eBPF that rewrites the OUTBOUND TCP SYN so the
// relay's connections to upstreams carry a chosen OS TCP/IP fingerprint instead
// of Linux's. It touches ONLY pure SYN packets (the cheap classifier lets data
// packets through untouched), so the data-plane cost is ~a per-packet branch.
//
// PROTOTYPE PROFILE (benchmark phase): TTL -> 128 and reorder the standard Linux
// SYN options [MSS, SACK_OK, TS, NOP, WScale] (20 bytes) to a Windows-flavored
// order [MSS, NOP, WScale, SACK_OK, TS] (also 20 bytes). Same length => in-place
// rewrite, no packet resize (resize for length-changing real profiles = optimize
// phase). It only rewrites a SYN whose options match the exact Linux layout; any
// other layout is passed through unchanged (never corrupt an unknown packet).
//
// Build: clang -O2 -g -target bpf -c syn_rewrite.bpf.c -o syn_rewrite.bpf.o
// Attach: tc qdisc add dev <if> clsact
//         tc filter add dev <if> egress bpf da obj syn_rewrite.bpf.o sec tc
#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/tcp.h>
#include <linux/in.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define TC_ACT_OK 0
#define NEW_TTL 128

// L2 length: loopback/tunnels may present raw IP (0), Ethernet NICs present 14.
static __always_inline int l2_len(struct __sk_buff *skb)
{
	__u8 b0 = 0;
	if (bpf_skb_load_bytes(skb, 0, &b0, 1) < 0)
		return -1;
	if ((b0 >> 4) == 4)
		return 0; // raw IPv4 at offset 0
	__be16 eth_proto = 0;
	if (bpf_skb_load_bytes(skb, 12, &eth_proto, 2) < 0)
		return -1;
	if (eth_proto == bpf_htons(ETH_P_IP))
		return ETH_HLEN; // 14-byte Ethernet
	return -1;
}

SEC("tc")
int syn_rewrite(struct __sk_buff *skb)
{
	int l2 = l2_len(skb);
	if (l2 < 0)
		return TC_ACT_OK;

	// --- IPv4 header (assume no IP options: ihl=5/20 bytes) ---
	__u8 vihl = 0;
	if (bpf_skb_load_bytes(skb, l2, &vihl, 1) < 0)
		return TC_ACT_OK;
	if ((vihl >> 4) != 4 || (vihl & 0x0f) != 5)
		return TC_ACT_OK; // not IPv4 / has IP options -> skip
	__u8 proto = 0;
	if (bpf_skb_load_bytes(skb, l2 + 9, &proto, 1) < 0 || proto != IPPROTO_TCP)
		return TC_ACT_OK;

	int tcp = l2 + 20;

	// --- TCP: pure SYN, and exactly 20 bytes of options (data offset 10) ---
	__u8 dataoff = 0, flags = 0;
	if (bpf_skb_load_bytes(skb, tcp + 12, &dataoff, 1) < 0)
		return TC_ACT_OK;
	if ((dataoff >> 4) != 10)
		return TC_ACT_OK; // not a 40-byte TCP header (20 opt bytes)
	if (bpf_skb_load_bytes(skb, tcp + 13, &flags, 1) < 0)
		return TC_ACT_OK;
	if (!(flags & 0x02) || (flags & 0x10))
		return TC_ACT_OK; // require SYN set, ACK clear (pure SYN)

	int opt = tcp + 20;
	__u8 o[20];
	if (bpf_skb_load_bytes(skb, opt, o, 20) < 0)
		return TC_ACT_OK;

	// Only rewrite the exact Linux layout: MSS(2) SACKOK(4) TS(8) NOP(1) WS(3).
	if (o[0] != 2 || o[4] != 4 || o[6] != 8 || o[16] != 1 || o[17] != 3)
		return TC_ACT_OK;

	// New order: MSS, NOP, WScale, SACK_OK, TS  (same 20 bytes).
	__u8 n[20];
	n[0] = o[0]; n[1] = o[1]; n[2] = o[2]; n[3] = o[3];   // MSS (4)
	n[4] = 0x01;                                          // NOP (1)
	n[5] = o[17]; n[6] = o[18]; n[7] = o[19];             // WScale (3)
	n[8] = o[4]; n[9] = o[5];                             // SACK_OK (2)
	n[10] = o[6]; n[11] = o[7]; n[12] = o[8]; n[13] = o[9];     // TS (10)
	n[14] = o[10]; n[15] = o[11]; n[16] = o[12]; n[17] = o[13];
	n[18] = o[14]; n[19] = o[15];

	// TCP checksum: apply the diff of old vs new option bytes.
	__s64 diff = bpf_csum_diff((__be32 *)o, 20, (__be32 *)n, 20, 0);
	if (bpf_skb_store_bytes(skb, opt, n, 20, 0) < 0)
		return TC_ACT_OK;
	bpf_l4_csum_replace(skb, tcp + 16, 0, diff, 0);

	// TTL -> NEW_TTL, then recompute the IP header checksum from scratch over the
	// 20-byte header (unambiguous — no incremental/endianness pitfalls). Linux RX
	// validates the IP checksum, so this MUST be correct or the SYN is dropped.
	__u8 nt = NEW_TTL;
	bpf_skb_store_bytes(skb, l2 + 8, &nt, 1, 0);
	__u8 iph[20];
	if (bpf_skb_load_bytes(skb, l2, iph, 20) == 0) {
		iph[10] = 0;
		iph[11] = 0; // zero the checksum field for the computation
		__u32 s = 0;
#pragma unroll
		for (int i = 0; i < 20; i += 2)
			s += ((__u32)iph[i] << 8) | iph[i + 1];
		s = (s & 0xffff) + (s >> 16);
		s = (s & 0xffff) + (s >> 16);
		__u16 c = ~s;
		__u8 cb[2] = { (__u8)(c >> 8), (__u8)(c & 0xff) };
		bpf_skb_store_bytes(skb, l2 + 10, cb, 2, 0);
	}

	return TC_ACT_OK;
}

char _license[] SEC("license") = "GPL";
