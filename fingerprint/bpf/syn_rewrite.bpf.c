// syn_rewrite.bpf.c — tc-egress eBPF that rewrites the OUTBOUND TCP SYN so the
// relay's connections to upstreams carry a chosen OS TCP/IP fingerprint instead
// of Linux's. It touches ONLY pure SYN packets (the cheap classifier lets data
// packets through untouched), so the data-plane cost is ~a per-packet branch.
//
// PROFILE (benchmark phase): TTL -> 128 and reorder the standard Linux SYN
// options [MSS, SACK_OK, TS, NOP, WScale] (20 bytes) to a Windows-flavored order
// [MSS, NOP, WScale, SACK_OK, TS] (also 20 bytes). Same length => in-place rewrite,
// no packet resize (resize for length-changing real profiles = next step). Only a
// SYN whose options match the exact Linux layout is rewritten; anything else is
// passed through untouched (never corrupt an unknown packet).
//
// PERF: uses DIRECT packet access (bounds-checked, after bpf_skb_pull_data) for all
// reads/writes and recomputes the IP checksum inline — only bpf_csum_diff +
// bpf_l4_csum_replace remain as helpers (TCP checksum, offload-correct). This is
// the optimized path; the earlier version used skb_load_bytes/store_bytes (~10
// helper calls).
//
// Build: see ../build.sh   Attach: tc filter ... egress bpf da obj ... sec tc
#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/tcp.h>
#include <linux/in.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define TC_ACT_OK 0
#define NEW_TTL 128

SEC("tc")
int syn_rewrite(struct __sk_buff *skb)
{
	// --- CHEAP CLASSIFY (runs on every egress packet): direct reads bounded by
	// data_end, NO pull_data. Non-SYN packets early-return here paying only a few
	// bounded byte reads — so the bulk data plane is untouched. pull_data (which
	// can linearize the whole skb) is deferred to the SYN-only rewrite path below.
	{
		void *data = (void *)(long)skb->data;
		void *data_end = (void *)(long)skb->data_end;
		__u8 *p = data;
		if (p + 14 > (__u8 *)data_end)
			return TC_ACT_OK;
		int l2c;
		if ((p[0] >> 4) == 4)
			l2c = 0;
		else if (((__u16)p[12] << 8 | p[13]) == ETH_P_IP)
			l2c = 14;
		else
			return TC_ACT_OK;
		if (p + l2c + 34 > (__u8 *)data_end)
			return TC_ACT_OK;
		__u8 *ipc = p + l2c;
		if ((ipc[0] >> 4) != 4 || (ipc[0] & 0x0f) != 5 || ipc[9] != IPPROTO_TCP)
			return TC_ACT_OK;
		__u8 *tcpc = ipc + 20;
		if ((tcpc[12] >> 4) != 10 || !(tcpc[13] & 0x02) || (tcpc[13] & 0x10))
			return TC_ACT_OK; // not a pure SYN with 20 option bytes
	}

	// --- REWRITE PATH (SYN only): now make the header range writable+linear.
	if (bpf_skb_pull_data(skb, 74) < 0)
		return TC_ACT_OK;
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;
	__u8 *p = data;

	if (p + 14 > (__u8 *)data_end)
		return TC_ACT_OK;
	int l2;
	if ((p[0] >> 4) == 4)
		l2 = 0;
	else if (((__u16)p[12] << 8 | p[13]) == ETH_P_IP)
		l2 = 14;
	else
		return TC_ACT_OK;

	// Need L2 + IP(20) + TCP base(20) + options(20) = l2 + 60 bytes.
	if (p + l2 + 60 > (__u8 *)data_end)
		return TC_ACT_OK;
	__u8 *ip = p + l2;
	if ((ip[0] >> 4) != 4 || (ip[0] & 0x0f) != 5) // IPv4, no IP options
		return TC_ACT_OK;
	if (ip[9] != IPPROTO_TCP)
		return TC_ACT_OK;

	__u8 *tcp = ip + 20;
	if ((tcp[12] >> 4) != 10) // 40-byte TCP header => 20 option bytes
		return TC_ACT_OK;
	if (!(tcp[13] & 0x02) || (tcp[13] & 0x10)) // pure SYN (SYN set, ACK clear)
		return TC_ACT_OK;

	__u8 *o = tcp + 20; // option bytes
	// Only the exact Linux layout: MSS(2) SACKOK(4) TS(8) NOP(1) WS(3).
	if (o[0] != 2 || o[4] != 4 || o[6] != 8 || o[16] != 1 || o[17] != 3)
		return TC_ACT_OK;

	// Snapshot old options, build new order: MSS, NOP, WScale, SACK_OK, TS (20B).
	__u8 old[20], neu[20];
#pragma unroll
	for (int i = 0; i < 20; i++)
		old[i] = o[i];
	neu[0] = old[0]; neu[1] = old[1]; neu[2] = old[2]; neu[3] = old[3]; // MSS
	neu[4] = 0x01;                                                      // NOP
	neu[5] = old[17]; neu[6] = old[18]; neu[7] = old[19];               // WScale
	neu[8] = old[4]; neu[9] = old[5];                                   // SACK_OK
	neu[10] = old[6]; neu[11] = old[7]; neu[12] = old[8]; neu[13] = old[9]; // TS
	neu[14] = old[10]; neu[15] = old[11]; neu[16] = old[12]; neu[17] = old[13];
	neu[18] = old[14]; neu[19] = old[15];

	// Write new options directly into the packet.
#pragma unroll
	for (int i = 0; i < 20; i++)
		o[i] = neu[i];

	// TTL -> NEW_TTL (direct), then recompute the IP header checksum inline over
	// the 20-byte header (checksum field treated as 0). Linux RX validates this.
	ip[8] = NEW_TTL;
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

	// TCP checksum: apply the option-bytes diff (offload-correct via the helper).
	__s64 diff = bpf_csum_diff((__be32 *)old, 20, (__be32 *)neu, 20, 0);
	bpf_l4_csum_replace(skb, l2 + 20 + 16, 0, diff, 0);

	return TC_ACT_OK;
}

char _license[] SEC("license") = "GPL";
