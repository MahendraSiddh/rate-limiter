#!/usr/bin/env bash
# eBPF loader script — compiles and attaches the XDP program
# Requires: clang, llvm, libbpf-dev, bpftool

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BPF_SRC="$SCRIPT_DIR/src/rate_limiter.bpf.c"
BPF_OBJ="$SCRIPT_DIR/src/rate_limiter.bpf.o"
IFACE="${1:-eth0}"

echo "[*] Compiling eBPF program..."
clang -O2 -g -target bpf \
    -D__TARGET_ARCH_x86 \
    -c "$BPF_SRC" \
    -o "$BPF_OBJ"

echo "[*] Attaching XDP program to interface $IFACE..."
ip link set dev "$IFACE" xdpgeneric obj "$BPF_OBJ" sec xdp

echo "[✓] eBPF rate limiter attached to $IFACE"
