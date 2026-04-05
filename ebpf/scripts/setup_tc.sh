#!/usr/bin/env bash
# =============================================================================
# setup_tc.sh — TC qdisc/filter setup for eBPF IP blocker
#
# Usage:
#   sudo ./setup_tc.sh [interface] [bpf_object]
#
#   interface  : network interface to attach to (default: eth0)
#   bpf_object : compiled eBPF .o file             (default: ../tc_blocker.o)
#
# This script handles three scenarios:
#   1. Fresh install  — no qdisc/filter exists yet
#   2. Hot-reload     — replace the BPF program atomically (zero packet drop)
#   3. Teardown       — remove qdisc + filter (called with --teardown flag)
#
# Requirements: iproute2 ≥ 5.13 (tc with BPF support), Linux kernel 5.15+
# =============================================================================

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
IFACE="${1:-eth0}"
BPF_OBJ="${2:-../tc_blocker.o}"
TC_SECTION="tc"            # SEC("tc") name in the C source
TC_PRIORITY=1              # filter priority (lower = evaluated first)
BPF_PIN_DIR="/sys/fs/bpf"  # BPF filesystem mount point

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { echo -e "\033[1;34m[*]\033[0m $*"; }
ok()    { echo -e "\033[1;32m[✓]\033[0m $*"; }
warn()  { echo -e "\033[1;33m[!]\033[0m $*"; }
error() { echo -e "\033[1;31m[✗]\033[0m $*" >&2; exit 1; }

require_root() {
    [[ $EUID -eq 0 ]] || error "This script must be run as root (sudo)."
}

require_tool() {
    command -v "$1" &>/dev/null || error "Required tool not found: $1"
}

# ---------------------------------------------------------------------------
# Teardown mode: remove qdisc (and with it all attached filters)
# ---------------------------------------------------------------------------
if [[ "${1:-}" == "--teardown" ]]; then
    IFACE="${2:-eth0}"
    info "Tearing down TC hooks on $IFACE..."
    if tc qdisc show dev "$IFACE" | grep -q "clsact"; then
        tc qdisc del dev "$IFACE" clsact
        ok "clsact qdisc removed from $IFACE"
    else
        warn "No clsact qdisc found on $IFACE — nothing to remove."
    fi
    exit 0
fi

# ---------------------------------------------------------------------------
# Pre-flight checks
# ---------------------------------------------------------------------------
require_root
require_tool tc
require_tool ip

info "eBPF TC setup: interface=$IFACE bpf_obj=$BPF_OBJ"

# Verify the eBPF object exists.
[[ -f "$BPF_OBJ" ]] || error "eBPF object not found: $BPF_OBJ. Run 'make' first."

# Verify the interface exists.
ip link show dev "$IFACE" &>/dev/null || error "Interface $IFACE not found."

# Verify the BPF filesystem is mounted.
mountpoint -q "$BPF_PIN_DIR" || error "/sys/fs/bpf is not mounted. Run: mount -t bpf bpf /sys/fs/bpf"

# ---------------------------------------------------------------------------
# Step 1: Ensure clsact qdisc is attached
#
# clsact is a minimal qdisc that provides ingress + egress TC hooks without
# any packet scheduling logic. It is the only qdisc that supports the
# TC_CLS_ACT_EGRESS and TC_CLS_ACT_INGRESS points simultaneously.
#
# We use `tc qdisc replace` (not `add`) so the command is idempotent —
# it succeeds whether the qdisc already exists or not.
# ---------------------------------------------------------------------------
info "Ensuring clsact qdisc on $IFACE..."
if tc qdisc show dev "$IFACE" | grep -q "clsact"; then
    warn "clsact already attached — skipping qdisc creation."
else
    tc qdisc add dev "$IFACE" clsact
    ok "clsact qdisc added to $IFACE"
fi

# ---------------------------------------------------------------------------
# Step 2: Attach / atomically replace the BPF filter
#
# `tc filter replace` performs an atomic swap of the BPF program:
#   - If no filter exists at the given priority, it creates one.
#   - If a filter already exists at priority $TC_PRIORITY, the kernel replaces
#     it atomically — not a single packet is processed by the old program
#     after the replace completes, and not a single packet is lost.
#
# Key flags:
#   ingress           : attach to the ingress TC hook (before packet reaches netstack)
#   bpf               : classifier type
#   da                : Direct Action — return codes (TC_ACT_SHOT/OK) are used directly
#   obj $BPF_OBJ      : path to the compiled eBPF ELF object
#   sec $TC_SECTION   : SEC("tc") function inside the ELF to use
#   direct-action     : synonym for 'da'
#   prio $TC_PRIORITY : filter priority (lower = higher priority)
#   protocol all      : match all L3 protocols (ETH_P_ALL)
#   花 flowid 1:1    : required by tc but ignored when direct-action is set
# ---------------------------------------------------------------------------
info "Attaching/replacing BPF filter on $IFACE ingress (priority $TC_PRIORITY)..."

tc filter replace \
    dev "$IFACE" \
    ingress \
    prio "$TC_PRIORITY" \
    handle 1 \
    bpf \
    da \
    obj "$BPF_OBJ" \
    sec "$TC_SECTION"

ok "BPF filter attached on $IFACE ingress"

# ---------------------------------------------------------------------------
# Step 3: Verify the filter is loaded
# ---------------------------------------------------------------------------
info "Verifying filter attachment..."
if tc filter show dev "$IFACE" ingress | grep -q "bpf"; then
    ok "Filter confirmed active on $IFACE ingress"
else
    error "Filter attachment verification failed. Check dmesg for BPF verifier errors."
fi

# ---------------------------------------------------------------------------
# Step 4: Mount BPF filesystem if not already mounted (belt-and-suspenders)
# ---------------------------------------------------------------------------
if ! mountpoint -q "$BPF_PIN_DIR" 2>/dev/null; then
    info "Mounting BPF filesystem at $BPF_PIN_DIR..."
    mount -t bpf bpf "$BPF_PIN_DIR"
    ok "BPF filesystem mounted"
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "  ┌─────────────────────────────────────────────────────┐"
echo "  │  eBPF TC IP blocker is active                       │"
echo "  │                                                     │"
echo "  │  Interface : $IFACE                                 │"
echo "  │  BPF object: $BPF_OBJ                               │"
echo "  │  Map pin   : $BPF_PIN_DIR/blocked_ips               │"
echo "  │                                                     │"
echo "  │  Manage via the loader HTTP API:                    │"
echo "  │    POST   http://localhost:9000/block               │"
echo "  │    DELETE http://localhost:9000/block/<ip>          │"
echo "  │    GET    http://localhost:9000/block               │"
echo "  └─────────────────────────────────────────────────────┘"
echo ""

# Show current filter state for confirmation.
echo "Current TC filters on $IFACE ingress:"
tc filter show dev "$IFACE" ingress
