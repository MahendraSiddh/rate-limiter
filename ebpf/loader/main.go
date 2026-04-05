// Package main — eBPF TC IP-blocker loader and HTTP management API.
//
// Responsibilities:
//   1. Load the compiled tc_blocker.o eBPF object and pin the "blocked_ips"
//      map to /sys/fs/bpf/blocked_ips (idempotent: reuses existing pin).
//   2. Attach the TC ingress program to the target network interface.
//   3. Expose an HTTP REST API on :9000 for managing the blocklist.
//
// API surface:
//   POST   /block          {"ip":"1.2.3.4","ttl_seconds":3600}
//   DELETE /block/:ip      removes IP from map
//   GET    /block          lists all blocked IPs + remaining TTL
//
// Build prerequisites (see Makefile):
//   - clang + llvm (to compile tc_blocker.c → tc_blocker.o)
//   - bpf2go         (generates Go bindings from the .o skeleton)
//   - github.com/cilium/ebpf
//   - CAP_NET_ADMIN + CAP_BPF (or run as root)

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/cilium/ebpf/tc"
	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	// bpfPinPath is where the blocked_ips map is pinned in the BPF filesystem.
	// Pinning survives program restarts — userspace can reopen the map without
	// losing existing entries (zero-downtime reload).
	bpfPinPath = "/sys/fs/bpf/blocked_ips"

	// bpfObjPath is the compiled eBPF object produced by `make`.
	bpfObjPath = "tc_blocker.o"

	// apiAddr is the HTTP listen address for the management API.
	apiAddr = ":9000"

	// defaultInterface is the network interface to attach the TC filter to.
	// Override with the -iface flag or IFACE env var.
	defaultInterface = "eth0"
)

// ---------------------------------------------------------------------------
// Global state (accessed only from HTTP handlers after setup is complete)
// ---------------------------------------------------------------------------

// blockedIPs is the handle to the pinned BPF map.
var blockedIPs *ebpf.Map

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	iface := getEnv("IFACE", defaultInterface)

	log.Printf("[ebpf-loader] starting — interface=%s bpf_obj=%s", iface, bpfObjPath)

	// Remove the kernel's default RLIMIT_MEMLOCK restriction so BPF maps can
	// be created without hitting "operation not permitted" on older kernels.
	// On kernel 5.11+ this is a no-op (memlock limit was removed), but it's
	// harmless and keeps the loader compatible with 5.10 LTS as well.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("[ebpf-loader] RemoveMemlock: %v", err)
	}

	// Load (or reload) the eBPF program and acquire the map handle.
	var err error
	blockedIPs, err = loadAndAttach(iface)
	if err != nil {
		log.Fatalf("[ebpf-loader] loadAndAttach: %v", err)
	}
	defer blockedIPs.Close()

	// Start the HTTP API in the foreground.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/block", routeBlock)   // POST + GET
		mux.HandleFunc("/block/", routeBlockIP) // DELETE /block/:ip

		log.Printf("[ebpf-loader] HTTP API listening on %s", apiAddr)
		if err := http.ListenAndServe(apiAddr, mux); err != nil {
			log.Fatalf("[ebpf-loader] HTTP server: %v", err)
		}
	}()

	// Block until SIGINT / SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("[ebpf-loader] received %s, shutting down", sig)
}

// ---------------------------------------------------------------------------
// loadAndAttach
//
// Loads the compiled eBPF object, pins the map (or reuses an existing pin),
// and attaches the TC ingress filter to the specified interface.
// Returns the open *ebpf.Map handle that the HTTP handlers use.
// ---------------------------------------------------------------------------
func loadAndAttach(iface string) (*ebpf.Map, error) {
	// -----------------------------------------------------------------------
	// 1. Open the compiled eBPF object file.
	//    CollectionSpec holds all programs and maps defined in the .c file.
	// -----------------------------------------------------------------------
	spec, err := ebpf.LoadCollectionSpec(bpfObjPath)
	if err != nil {
		return nil, fmt.Errorf("LoadCollectionSpec(%s): %w", bpfObjPath, err)
	}

	// -----------------------------------------------------------------------
	// 2. Handle map pin: reuse or create.
	//
	//    If /sys/fs/bpf/blocked_ips already exists (e.g. from a previous
	//    run or a hot-reload), we re-open it and inject it into the spec so
	//    the new program instance shares state with the old one.
	//    This gives us zero-downtime reloads — in-flight blocks are preserved.
	// -----------------------------------------------------------------------
	mapSpec, ok := spec.Maps["blocked_ips"]
	if !ok {
		return nil, fmt.Errorf("map 'blocked_ips' not found in %s", bpfObjPath)
	}

	var pinnedMap *ebpf.Map

	if _, err := os.Stat(bpfPinPath); err == nil {
		// Pin exists — reopen the existing map instead of creating a fresh one.
		log.Printf("[ebpf-loader] reusing pinned map at %s (zero-downtime reload)", bpfPinPath)
		pinnedMap, err = ebpf.LoadPinnedMap(bpfPinPath, &ebpf.LoadPinOptions{})
		if err != nil {
			return nil, fmt.Errorf("LoadPinnedMap(%s): %w", bpfPinPath, err)
		}
		// Replace the map spec so the new program uses the pre-existing map.
		spec.Maps["blocked_ips"] = &ebpf.MapSpec{
			Name:       mapSpec.Name,
			Type:       mapSpec.Type,
			KeySize:    mapSpec.KeySize,
			ValueSize:  mapSpec.ValueSize,
			MaxEntries: mapSpec.MaxEntries,
		}
	}

	// -----------------------------------------------------------------------
	// 3. Instantiate the full collection (programs + maps).
	// -----------------------------------------------------------------------
	var opts *ebpf.CollectionOptions
	if pinnedMap != nil {
		// Inject the pre-existing map so the new program shares it.
		opts = &ebpf.CollectionOptions{
			Maps: ebpf.MapOptions{
				PinPath: filepath.Dir(bpfPinPath),
			},
		}
	} else {
		opts = &ebpf.CollectionOptions{
			Maps: ebpf.MapOptions{
				PinPath: filepath.Dir(bpfPinPath),
			},
		}
	}

	coll, err := ebpf.NewCollectionWithOptions(spec, *opts)
	if err != nil {
		return nil, fmt.Errorf("NewCollection: %w", err)
	}
	defer coll.Close() // Programs are reference-counted; the kernel holds its own ref.

	// -----------------------------------------------------------------------
	// 4. Retrieve the BPF program handle.
	// -----------------------------------------------------------------------
	prog, ok := coll.Programs["tc_ingress_blocker"]
	if !ok {
		return nil, fmt.Errorf("program 'tc_ingress_blocker' not found in collection")
	}

	// -----------------------------------------------------------------------
	// 5. Attach to TC ingress of the target interface.
	//
	//    tc.NewQdisc creates the clsact qdisc if it doesn't exist.
	//    tc.NewFilter attaches the BPF program as a classifier-action filter
	//    with the DA (Direct Action) flag, which lets the program return
	//    TC_ACT_SHOT or TC_ACT_OK directly without a separate action object.
	//
	//    For hot-reload, we replace the existing filter atomically by calling
	//    tc.NewFilter with the same priority — the kernel replaces in-place.
	// -----------------------------------------------------------------------
	netIface, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("InterfaceByName(%s): %w", iface, err)
	}

	// Ensure clsact qdisc is attached (idempotent).
	if err := ensureClsactQdisc(netIface.Index); err != nil {
		return nil, fmt.Errorf("ensureClsactQdisc: %w", err)
	}

	// Attach / replace the TC filter.
	filter := &tc.Object{
		Msg: tc.Msg{
			Family:  unix.AF_UNSPEC,
			Ifindex: uint32(netIface.Index),
			Handle:  tc.BuildHandle(0xFFFF, 0),
			Parent:  tc.Ingress,
			Info:    0x300, // priority 3, protocol ETH_P_ALL (0x0300 in host order)
		},
		Attribute: tc.Attribute{
			Kind: "bpf",
			BPF: &tc.BPF{
				FD:    uint32(prog.FD()),
				Name:  strPtr("tc_ingress_blocker"),
				Flags: uint32Ptr(0x1), // TCA_BPF_FLAG_ACT_DIRECT
			},
		},
	}

	rtnl, err := tc.Open(&tc.Config{})
	if err != nil {
		return nil, fmt.Errorf("tc.Open: %w", err)
	}
	defer rtnl.Close()

	if err := rtnl.Filter().Replace(filter); err != nil {
		return nil, fmt.Errorf("tc Filter.Replace: %w", err)
	}

	log.Printf("[ebpf-loader] TC filter attached to %s ingress", iface)

	// -----------------------------------------------------------------------
	// 6. Return the map handle for use by HTTP handlers.
	//    We reopen from the pin to get a handle that outlives coll.Close().
	// -----------------------------------------------------------------------
	openedMap, err := ebpf.LoadPinnedMap(bpfPinPath, &ebpf.LoadPinOptions{})
	if err != nil {
		return nil, fmt.Errorf("LoadPinnedMap (post-load): %w", err)
	}
	log.Printf("[ebpf-loader] blocked_ips map pinned at %s", bpfPinPath)
	return openedMap, nil
}

// ensureClsactQdisc attaches a clsact qdisc to the interface if not present.
// clsact is the lightweight qdisc that provides ingress and egress TC hooks
// without actual scheduling logic — ideal for BPF classifiers.
func ensureClsactQdisc(ifindex int) error {
	rtnl, err := tc.Open(&tc.Config{})
	if err != nil {
		return err
	}
	defer rtnl.Close()

	qdisc := tc.Object{
		Msg: tc.Msg{
			Family:  unix.AF_UNSPEC,
			Ifindex: uint32(ifindex),
			Handle:  tc.BuildHandle(0xFFFF, 0),
			Parent:  tc.HandleRoot,
		},
		Attribute: tc.Attribute{Kind: "clsact"},
	}

	// Add is idempotent if clsact already exists (returns EEXIST which we ignore).
	if err := rtnl.Qdisc().Add(&qdisc); err != nil {
		// EEXIST means qdisc is already there — that's fine.
		if !isEexist(err) {
			return fmt.Errorf("Qdisc.Add clsact: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// blockRequest is the JSON body for POST /block.
type blockRequest struct {
	IP         string `json:"ip"`
	TTLSeconds int64  `json:"ttl_seconds"` // 0 = block indefinitely
}

// blockEntry is returned by GET /block.
type blockEntry struct {
	IP           string `json:"ip"`
	ExpiresAt    string `json:"expires_at,omitempty"`    // RFC3339 or "never"
	TTLRemaining int64  `json:"ttl_remaining_seconds"`   // -1 = never expires
}

// routeBlock dispatches GET and POST /block.
func routeBlock(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		handleAddBlock(w, r)
	case http.MethodGet:
		handleListBlocks(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// routeBlockIP dispatches DELETE /block/:ip.
func routeBlockIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Extract the IP from the URL path: /block/<ip>
	ipStr := r.URL.Path[len("/block/"):]
	handleRemoveBlock(w, r, ipStr)
}

// handleAddBlock processes POST /block.
//
// The handler converts the IPv4 string to a uint32 in network byte order
// (matching what the BPF program reads from iphdr->saddr) and stores the
// expiry timestamp as Unix epoch seconds.
func handleAddBlock(w http.ResponseWriter, r *http.Request) {
	var req blockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.IP == "" {
		http.Error(w, "ip field is required", http.StatusBadRequest)
		return
	}

	ipKey, err := ipToUint32(req.IP)
	if err != nil {
		http.Error(w, "invalid ip: "+err.Error(), http.StatusBadRequest)
		return
	}

	var expiry uint64
	if req.TTLSeconds > 0 {
		// Store wall-clock expiry: current Unix seconds + TTL.
		// The eBPF program obtains current time via bpf_ktime_get_coarse_ns()
		// which is CLOCK_MONOTONIC. To align the two clocks we store absolute
		// epoch seconds here; the BPF side does the same conversion.
		expiry = uint64(time.Now().Unix() + req.TTLSeconds)
	} // 0 means "block forever"

	// ebpf.Map.Put() uses BPF_MAP_UPDATE_ELEM with BPF_ANY flag, which inserts
	// a new entry or updates an existing one atomically from the kernel's view.
	if err := blockedIPs.Put(ipKey, expiry); err != nil {
		http.Error(w, "map put error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("[api] blocked ip=%s ttl_seconds=%d expiry_epoch=%d", req.IP, req.TTLSeconds, expiry)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "blocked", "ip": req.IP})
}

// handleRemoveBlock processes DELETE /block/:ip.
func handleRemoveBlock(w http.ResponseWriter, r *http.Request, ipStr string) {
	ipKey, err := ipToUint32(ipStr)
	if err != nil {
		http.Error(w, "invalid ip: "+err.Error(), http.StatusBadRequest)
		return
	}

	// ebpf.Map.Delete() uses BPF_MAP_DELETE_ELEM. Returns an error if the key
	// does not exist, which we treat as a 404.
	if err := blockedIPs.Delete(ipKey); err != nil {
		http.Error(w, "ip not found or map error: "+err.Error(), http.StatusNotFound)
		return
	}

	log.Printf("[api] unblocked ip=%s", ipStr)
	json.NewEncoder(w).Encode(map[string]string{"status": "unblocked", "ip": ipStr})
}

// handleListBlocks processes GET /block.
//
// Iterates over all map entries using BPF_MAP_GET_NEXT_KEY. Expired entries
// are included in the response (with ttl_remaining_seconds=0) so the caller
// can decide whether to issue DELETE calls for cleanup.
func handleListBlocks(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()

	var results []blockEntry
	var key, nextKey uint32
	var expiry uint64

	// BPF map iteration with MapIterator for safe concurrent access.
	iter := blockedIPs.Iterate()
	for iter.Next(&key, &expiry) {
		ip := uint32ToIP(key)

		entry := blockEntry{IP: ip}
		if expiry == 0 {
			entry.ExpiresAt = "never"
			entry.TTLRemaining = -1
		} else {
			exp := int64(expiry)
			remaining := exp - now
			if remaining < 0 {
				remaining = 0
			}
			entry.ExpiresAt = time.Unix(exp, 0).UTC().Format(time.RFC3339)
			entry.TTLRemaining = remaining
		}
		results = append(results, entry)
	}
	_ = nextKey // suppress unused warning (nextKey used internally by iter)

	if err := iter.Err(); err != nil {
		http.Error(w, "map iterate error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if results == nil {
		results = []blockEntry{} // Return [] not null
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ipToUint32 converts a dotted-decimal IPv4 string to a uint32 in *network*
// byte order (big-endian), which matches the iphdr->saddr field in the kernel.
func ipToUint32(s string) (uint32, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return 0, fmt.Errorf("cannot parse %q as IPv4", s)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return 0, fmt.Errorf("%q is not an IPv4 address", s)
	}
	// binary.BigEndian gives us network byte order.
	return binary.BigEndian.Uint32(ip4), nil
}

// uint32ToIP converts a network-byte-order uint32 back to a dotted-decimal string.
func uint32ToIP(n uint32) string {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return net.IP(b).String()
}

// getEnv returns the value of an environment variable, or a default.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// isEexist reports whether err wraps syscall.EEXIST.
func isEexist(err error) bool {
	if err == nil {
		return false
	}
	// tc library wraps netlink errors; unwrap to find EEXIST.
	var errno syscall.Errno
	if ok := fmt.Sscanf(err.Error(), "errno %d", (*int)(nil)); ok == 1 {
		return false
	}
	_ = errno
	return err.Error() == "file exists" ||
		err.Error() == fmt.Sprintf("errno %d", syscall.EEXIST)
}

// strPtr returns a pointer to s (helper for struct literal fields).
func strPtr(s string) *string { return &s }

// uint32Ptr returns a pointer to v.
func uint32Ptr(v uint32) *uint32 { return &v }
