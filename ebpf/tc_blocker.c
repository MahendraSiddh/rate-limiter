// SPDX-License-Identifier: GPL-2.0
//
// tc_blocker.c — eBPF TC ingress IP-blocking program
//
// Attaches to the Linux Traffic Control (tc) ingress hook and inspects
// every inbound packet. If the packet's source IPv4 address is found in
// the "blocked_ips" LRU hash map AND the block has not yet expired, the
// packet is silently dropped (TC_ACT_SHOT) before it ever reaches userspace.
// Otherwise the packet is passed through (TC_ACT_OK).
//
// Map path: /sys/fs/bpf/blocked_ips
// Attach:   tc filter add dev eth0 ingress bpf da obj tc_blocker.o sec tc
//
// Kernel requirement: Linux 5.15+ (bpf_ktime_get_ns, LRU_HASH, TC hook).

#include <linux/bpf.h>
#include <linux/pkt_cls.h>   // TC_ACT_SHOT, TC_ACT_OK
#include <linux/if_ether.h>  // struct ethhdr, ETH_P_IP
#include <linux/ip.h>        // struct iphdr
#include <bpf/bpf_helpers.h> // SEC(), bpf_map_lookup_elem(), bpf_ktime_get_ns()
#include <bpf/bpf_endian.h>  // bpf_ntohs()

// ---------------------------------------------------------------------------
// Map definition: blocked_ips
//
// BPF_MAP_TYPE_LRU_HASH is used instead of HASH so the kernel automatically
// evicts the least-recently-used entry when the map is full. This prevents
// the map from becoming a resource-exhaustion vector itself.
//
// key   : __u32  — source IPv4 address in *network* byte order (big-endian)
//                  so we can compare directly against iphdr->saddr without
//                  byte-swapping on every packet.
// value : __u64  — block expiry timestamp in Unix epoch *seconds*.
//                  A value of 0 means "block indefinitely".
// ---------------------------------------------------------------------------
struct {
    __uint(type,        BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 100000);
    __type(key,   __u32);  // IPv4 source address (network byte order)
    __type(value, __u64);  // expiry: Unix epoch seconds (0 = forever)
    // PIN_BY_NAME causes libbpf to pin the map at /sys/fs/bpf/<map-name>
    // automatically when the object is loaded via bpf_object__load().
    // The Go loader uses this to re-open the map across hot-reloads.
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} blocked_ips SEC(".maps");

// ---------------------------------------------------------------------------
// TC ingress entry point
//
// The kernel passes a pointer to the sk_buff metadata as struct __sk_buff.
// ctx->data and ctx->data_end delimit the packet bytes in memory.
// Every pointer dereference must be bounds-checked or the BPF verifier will
// reject the program at load time.
// ---------------------------------------------------------------------------
SEC("tc")
int tc_ingress_blocker(struct __sk_buff *ctx)
{
    // Cast the kernel-provided pointers to byte pointers.
    // The verifier tracks these as untrusted packet memory.
    void *data     = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;

    // -----------------------------------------------------------------------
    // Step 1: Parse the Ethernet header
    //
    // Bounds check: ensure there are at least sizeof(ethhdr) bytes available.
    // Without this the verifier rejects the program with "invalid mem access".
    // -----------------------------------------------------------------------
    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return TC_ACT_OK;  // Too short to be a valid frame — pass through.

    // Only process IPv4 packets. ARP, IPv6, VLAN-tagged frames, etc. all pass.
    // bpf_ntohs() converts the 16-bit EtherType from big-endian to host order.
    if (bpf_ntohs(eth->h_proto) != ETH_P_IP)
        return TC_ACT_OK;

    // -----------------------------------------------------------------------
    // Step 2: Parse the IPv4 header
    //
    // The IP header immediately follows the Ethernet header.
    // We only need saddr, so a single bounds check covering the fixed 20-byte
    // base header is sufficient.
    // -----------------------------------------------------------------------
    struct iphdr *iph = (struct iphdr *)(eth + 1);
    if ((void *)(iph + 1) > data_end)
        return TC_ACT_OK;  // Truncated IP header — pass through safely.

    // -----------------------------------------------------------------------
    // Step 3: Look up source IP in the blocked_ips map
    //
    // bpf_map_lookup_elem() returns a pointer directly into the map's value
    // slot (zero-copy). Returns NULL if the key is not present.
    // -----------------------------------------------------------------------
    __u32 src_ip = iph->saddr;  // Already in network byte order — matches key.

    __u64 *expiry = bpf_map_lookup_elem(&blocked_ips, &src_ip);
    if (!expiry)
        return TC_ACT_OK;  // IP not in blocklist — let it through.

    // -----------------------------------------------------------------------
    // Step 4: Check TTL expiry
    //
    // bpf_ktime_get_ns() returns nanoseconds since boot (CLOCK_MONOTONIC).
    // We convert to seconds and compare against the stored Unix epoch expiry.
    //
    // To bridge monotonic → wall clock we store epoch seconds in userspace and
    // fetch wall-clock seconds here using the BPF_FUNC_ktime_get_boot_ns path.
    // The simpler approach used here:
    //   - Userspace stores  (now_epoch_sec + ttl)  as *expiry.
    //   - Kernel compares   bpf_ktime_get_coarse_ns() / 1e9  against *expiry.
    //
    // bpf_ktime_get_coarse_ns() (kernel 5.11+) is cheaper than the fine-grained
    // variant and sufficient for second-resolution TTL checks.
    //
    // Special case: *expiry == 0 means "block forever" (no expiry).
    // -----------------------------------------------------------------------
    if (*expiry != 0) {
        // Divide nanoseconds → seconds. The verifier needs integer division;
        // use a right-shift by 30 ≈ /1e9 (accurate to within ~7%).
        // For production precision, divide by 1000000000ULL explicitly —
        // modern kernels inline this as a multiply-by-reciprocal.
        __u64 now_sec = bpf_ktime_get_coarse_ns() / 1000000000ULL;

        if (now_sec >= *expiry) {
            // Block has expired. Pass this packet and let userspace GC the
            // entry. We do NOT delete from the map here because BPF programs
            // run with preemption disabled and map deletion under high load
            // can cause lock contention; userspace handles cleanup via a
            // periodic sweep over the map with bpf_map_get_next_key().
            return TC_ACT_OK;
        }
    }

    // -----------------------------------------------------------------------
    // Step 5: Drop the packet
    //
    // TC_ACT_SHOT tells the kernel to free the sk_buff and increment the
    // interface's drop counter (visible in `ip -s link show eth0`).
    // No ICMP "port unreachable" is sent — this is a silent drop, matching
    // the behaviour expected for volumetric attack mitigation.
    // -----------------------------------------------------------------------
    return TC_ACT_SHOT;
}

// The BPF verifier requires a license string so GPL-only helpers (like
// bpf_ktime_get_coarse_ns) are accessible to the program.
char _license[] SEC("license") = "GPL";
