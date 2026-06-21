// syn_rewrite.bpf.c — tc-egress eBPF that rewrites the relay's OUTBOUND TCP SYN to
// a chosen OS TCP/IP fingerprint. The relay sets SO_MARK on the upstream socket;
// the SYN carries it as skb->mark and we switch on it. Unmarked traffic returns on
// the first instruction, so only the relay's marked outbound conns are touched.
//
// Profiles (option *layout* + TTL; the functional window/wscale come from the relay
// via setsockopt(SO_RCVBUF), see fingerprint/README.md):
//   mark 1 FP_WINDOWS : TTL 128, opts mss,nop,ws,sok,ts        (20B, in-place)
//   mark 2 FP_MACOS   : TTL  64, opts mss,nop,ws,nop,nop,ts,sok,eol (24B, grow +4)
// (Android == Linux option layout -> no eBPF; sockopt only.)
//
// Only a SYN whose options are the exact Linux layout [mss,sok,ts,nop,ws] (20B) is
// rewritten; anything else passes through untouched.
//
// CHECKSUMS: IP header recomputed inline (Linux RX validates it). TCP checksum via
// bpf_csum_diff + bpf_l4_csum_replace (offload-correct). NOTE: on loopback the TCP
// checksum is offload-handled and not validated end-to-end — TCP-csum correctness
// must be confirmed on a real NIC (see README).
#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/tcp.h>
#include <linux/in.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define TC_ACT_OK 0
#define FP_WINDOWS 1
#define FP_MACOS   2

// Recompute the 20-byte IPv4 header checksum in place from direct-access bytes.
static __always_inline void ip_csum(__u8 *ip)
{
	__u32 s = 0;
#pragma unroll
	for (int i = 0; i < 20; i += 2) {
		if (i == 10)
			continue; // skip the checksum field
		s += ((__u32)ip[i] << 8) | ip[i + 1];
	}
	s = (s & 0xffff) + (s >> 16);
	s = (s & 0xffff) + (s >> 16);
	__u16 c = ~s;
	ip[10] = c >> 8;
	ip[11] = c & 0xff;
}

SEC("tc")
int syn_rewrite(struct __sk_buff *skb)
{
	__u32 fp = skb->mark;
	if (fp != FP_WINDOWS && fp != FP_MACOS)
		return TC_ACT_OK;

	// --- cheap classify (no pull_data): is this a pure SYN with the Linux layout?
	{
		void *d = (void *)(long)skb->data, *e = (void *)(long)skb->data_end;
		__u8 *p = d;
		if (p + 14 > (__u8 *)e)
			return TC_ACT_OK;
		int l2c = ((p[0] >> 4) == 4) ? 0 : ((((__u16)p[12] << 8 | p[13]) == ETH_P_IP) ? 14 : -1);
		if (l2c < 0 || p + l2c + 60 > (__u8 *)e)
			return TC_ACT_OK;
		__u8 *ipc = p + l2c, *tc = ipc + 20;
		if ((ipc[0] >> 4) != 4 || (ipc[0] & 0x0f) != 5 || ipc[9] != IPPROTO_TCP)
			return TC_ACT_OK;
		if ((tc[12] >> 4) != 10 || !(tc[13] & 0x02) || (tc[13] & 0x10))
			return TC_ACT_OK;
		__u8 *oc = tc + 20;
		if (oc[0] != 2 || oc[4] != 4 || oc[6] != 8 || oc[16] != 1 || oc[17] != 3)
			return TC_ACT_OK;
	}

	// --- rewrite path: make headers writable, re-fetch, read old options.
	if (bpf_skb_pull_data(skb, 74) < 0)
		return TC_ACT_OK;
	void *data = (void *)(long)skb->data, *data_end = (void *)(long)skb->data_end;
	__u8 *p = data;
	if (p + 14 > (__u8 *)data_end)
		return TC_ACT_OK;
	int l2 = ((p[0] >> 4) == 4) ? 0 : 14;
	if (p + l2 + 60 > (__u8 *)data_end)
		return TC_ACT_OK;
	__u8 *ip = p + l2, *tcp = ip + 20, *o = tcp + 20;
	if (o[0] != 2 || o[4] != 4 || o[6] != 8 || o[16] != 1 || o[17] != 3)
		return TC_ACT_OK;
	__u8 old[20];
#pragma unroll
	for (int i = 0; i < 20; i++)
		old[i] = o[i];
	// Linux fields: mss=old[0:4] sackok=old[4:6] ts=old[6:16] nop=old[16] ws=old[17:20]

	if (fp == FP_WINDOWS) {
		// Modern Windows 10/11: TTL 128 + opts mss,nop,ws,nop,nop,sok (12B, NO
		// timestamps) -> shrink the option field 20B -> 12B.
		__u8 w[12];
		w[0] = old[0]; w[1] = old[1]; w[2] = old[2]; w[3] = old[3]; // mss
		w[4] = 0x01;                                                // nop
		w[5] = old[17]; w[6] = old[18]; w[7] = old[19];            // ws
		w[8] = 0x01; w[9] = 0x01;                                  // nop nop
		w[10] = old[4]; w[11] = old[5];                            // sackok
		__u32 newlen = skb->len - 8;
		if (bpf_skb_change_tail(skb, newlen, 0) < 0)
			return TC_ACT_OK;
		data = (void *)(long)skb->data;
		data_end = (void *)(long)skb->data_end;
		p = data;
		if (p + l2 + 52 > (__u8 *)data_end) // ip20 + tcp20 + opts12
			return TC_ACT_OK;
		ip = p + l2; tcp = ip + 20; o = tcp + 20;
#pragma unroll
		for (int i = 0; i < 12; i++)
			o[i] = w[i];
		__u8 doff_old = tcp[12], doff_new = (8 << 4) | (tcp[12] & 0x0f);
		tcp[12] = doff_new;
		ip[8] = 128; // TTL 128
		__u16 totlen = (((__u16)ip[2] << 8) | ip[3]) - 8;
		ip[2] = totlen >> 8; ip[3] = totlen & 0xff;
		ip_csum(ip);
		__u8 do_old[4] = { doff_old, tcp[13], tcp[14], tcp[15] };
		__u8 do_new[4] = { doff_new, tcp[13], tcp[14], tcp[15] };
		__s64 d_doff = bpf_csum_diff((__be32 *)do_old, 4, (__be32 *)do_new, 4, 0);
		__s64 d_opts = bpf_csum_diff((__be32 *)old, 20, (__be32 *)w, 12, 0);
		int csoff = l2 + 20 + 16;
		bpf_l4_csum_replace(skb, csoff, 0, d_doff, 0);
		bpf_l4_csum_replace(skb, csoff, 0, d_opts, 0);
		bpf_l4_csum_replace(skb, csoff, bpf_htons(40), bpf_htons(32), 2 | BPF_F_PSEUDO_HDR);
		return TC_ACT_OK;
	}

	// FP_MACOS: TTL stays 64; opts mss,nop,ws,nop,nop,ts,sok,eol (24B) -> grow +4.
	__u8 m[24];
	m[0] = old[0]; m[1] = old[1]; m[2] = old[2]; m[3] = old[3];   // mss
	m[4] = 0x01;                                                  // nop
	m[5] = old[17]; m[6] = old[18]; m[7] = old[19];              // ws
	m[8] = 0x01; m[9] = 0x01;                                    // nop nop
	m[10] = old[6]; m[11] = old[7]; m[12] = old[8]; m[13] = old[9]; // ts
	m[14] = old[10]; m[15] = old[11]; m[16] = old[12]; m[17] = old[13];
	m[18] = old[14]; m[19] = old[15];
	m[20] = old[4]; m[21] = old[5];                              // sackok
	m[22] = 0x00; m[23] = 0x00;                                  // eol + pad

	__u32 newlen = skb->len + 4;
	if (bpf_skb_change_tail(skb, newlen, 0) < 0)
		return TC_ACT_OK;
	// pointers invalidated by change_tail — re-fetch + re-derive (l2 unchanged).
	data = (void *)(long)skb->data;
	data_end = (void *)(long)skb->data_end;
	p = data;
	if (p + l2 + 64 > (__u8 *)data_end)
		return TC_ACT_OK;
	ip = p + l2; tcp = ip + 20; o = tcp + 20;
#pragma unroll
	for (int i = 0; i < 24; i++)
		o[i] = m[i];

	// TCP data offset 10 -> 11 (40B -> 44B header).
	__u8 doff_old = tcp[12], doff_new = (11 << 4) | (tcp[12] & 0x0f);
	tcp[12] = doff_new;

	// IP total length 60 -> 64, then recompute IP checksum.
	__u16 totlen = (((__u16)ip[2] << 8) | ip[3]) + 4;
	ip[2] = totlen >> 8; ip[3] = totlen & 0xff;
	ip_csum(ip);

	// TCP checksum: (a) data-offset byte, (b) options 20B->24B, (c) pseudo-header
	// length 40->44. csum field (tcp[16:18]) excluded from the diffs.
	__u8 do_old[4] = { doff_old, tcp[13], tcp[14], tcp[15] };
	__u8 do_new[4] = { doff_new, tcp[13], tcp[14], tcp[15] };
	__s64 d_doff = bpf_csum_diff((__be32 *)do_old, 4, (__be32 *)do_new, 4, 0);
	__s64 d_opts = bpf_csum_diff((__be32 *)old, 20, (__be32 *)m, 24, 0);
	int csoff = l2 + 20 + 16;
	bpf_l4_csum_replace(skb, csoff, 0, d_doff, 0);
	bpf_l4_csum_replace(skb, csoff, 0, d_opts, 0);
	bpf_l4_csum_replace(skb, csoff, bpf_htons(40), bpf_htons(44), 2 | BPF_F_PSEUDO_HDR);
	return TC_ACT_OK;
}

char _license[] SEC("license") = "GPL";
