// Package ebpf provides a Go interface to the eBPF blocked-IPs LRU hash map.
//
// When the pinned BPF map (/sys/fs/bpf/ratelimiter/blocked_ips) is present
// (i.e. the XDP program is loaded), IPs are written/removed atomically.
// If the map is absent the Blocklist falls back to a no-op so that the
// decision engine can run without a Linux kernel supporting eBPF.
package ebpf

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/http"

	"github.com/cilium/ebpf"
	"github.com/rs/zerolog/log"

	"github.com/gin-gonic/gin"
)

const pinnedMapPath = "/sys/fs/bpf/ratelimiter/blocked_ips"

// Blocklist wraps the eBPF LRU hash map for blocked IPs.
type Blocklist struct {
	m *ebpf.Map // nil ⟹ no-op mode
}

// New opens the pinned eBPF map.  Returns a no-op Blocklist if unavailable.
func New() *Blocklist {
	m, err := ebpf.LoadPinnedMap(pinnedMapPath, nil)
	if err != nil {
		log.Warn().Err(err).Msg("ebpf: blocked_ips map unavailable, running in no-op mode")
		return &Blocklist{}
	}
	log.Info().Str("path", pinnedMapPath).Msg("ebpf: blocked_ips map opened")
	return &Blocklist{m: m}
}

// Add inserts the IPv4 address into the block map (value = 1).
func (b *Blocklist) Add(ip string) error {
	if b.m == nil {
		return nil // no-op
	}

	key, err := ipToKey(ip)
	if err != nil {
		return fmt.Errorf("ebpf add: %w", err)
	}

	var val uint32 = 1
	if err := b.m.Put(key, val); err != nil {
		return fmt.Errorf("ebpf map put %s: %w", ip, err)
	}

	log.Info().Str("ip", ip).Msg("ebpf: IP blocked")
	return nil
}

// Remove deletes the IPv4 address from the block map.
func (b *Blocklist) Remove(ip string) error {
	if b.m == nil {
		return nil // no-op
	}

	key, err := ipToKey(ip)
	if err != nil {
		return fmt.Errorf("ebpf remove: %w", err)
	}

	if err := b.m.Delete(key); err != nil {
		return fmt.Errorf("ebpf map delete %s: %w", ip, err)
	}

	log.Info().Str("ip", ip).Msg("ebpf: IP unblocked")
	return nil
}

// Close releases the map file descriptor.
func (b *Blocklist) Close() error {
	if b.m != nil {
		return b.m.Close()
	}
	return nil
}

// ── HTTP handlers ────────────────────────────────────────────────────────────

// RegisterRoutes attaches POST /block and DELETE /block/:ip to a gin router.
func (b *Blocklist) RegisterRoutes(r gin.IRouter) {
	r.POST("/block", b.handleBlock)
	r.DELETE("/block/:ip", b.handleUnblock)
}

// POST /block  body: {"ip": "1.2.3.4"}
func (b *Blocklist) handleBlock(c *gin.Context) {
	var req struct {
		IP string `json:"ip" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := b.Add(req.IP); err != nil {
		log.Error().Err(err).Str("ip", req.IP).Msg("http: block failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"blocked": req.IP})
}

// DELETE /block/:ip
func (b *Blocklist) handleUnblock(c *gin.Context) {
	ip := c.Param("ip")

	if err := b.Remove(ip); err != nil {
		log.Error().Err(err).Str("ip", ip).Msg("http: unblock failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"unblocked": ip})
}

// ── helpers ──────────────────────────────────────────────────────────────────

// ipToKey converts an IPv4 string to a 4-byte big-endian uint32 map key.
func ipToKey(s string) ([]byte, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP: %s", s)
	}

	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("only IPv4 supported, got: %s", s)
	}

	key := make([]byte, 4)
	binary.BigEndian.PutUint32(key, binary.BigEndian.Uint32(ip4))
	return key, nil
}
