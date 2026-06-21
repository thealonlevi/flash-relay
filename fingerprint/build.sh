#!/usr/bin/env bash
# Build the tc-egress SYN-rewrite eBPF object. Needs clang + libbpf headers
# (apt: clang llvm libbpf-dev) and the multiarch asm/ include path.
set -euo pipefail
cd "$(dirname "$0")/bpf"
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
  -I/usr/include/x86_64-linux-gnu \
  -c syn_rewrite.bpf.c -o syn_rewrite.bpf.o
echo "built $(pwd)/syn_rewrite.bpf.o ($(stat -c%s syn_rewrite.bpf.o) bytes)"
echo "attach: tc qdisc add dev <if> clsact && tc filter add dev <if> egress bpf da obj syn_rewrite.bpf.o sec tc"
echo "detach: tc qdisc del dev <if> clsact"
