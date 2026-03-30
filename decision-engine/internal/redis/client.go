// Package redis provides an atomic token-bucket client backed by Redis Cluster.
package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucketScript atomically decrements a token bucket.
//
// KEYS[1]  = bucket key  (e.g. "bucket:{ip}")
// ARGV[1]  = initial bucket size (filled on first touch)
// ARGV[2]  = TTL in seconds
//
// Returns the number of tokens remaining after decrement.
// Returns -1 if the bucket is already empty (no tokens consumed).
var tokenBucketScript = redis.NewScript(`
local key      = KEYS[1]
local capacity = tonumber(ARGV[1])
local ttl      = tonumber(ARGV[2])

-- Initialise bucket if it doesn't exist
if redis.call("EXISTS", key) == 0 then
    redis.call("SET", key, capacity - 1, "EX", ttl)
    return capacity - 1
end

local remaining = tonumber(redis.call("GET", key))
if remaining <= 0 then
    -- Bucket exhausted — refresh TTL to avoid stale keys, but don't allow
    redis.call("EXPIRE", key, ttl)
    return -1
end

redis.call("DECRBY", key, 1)
redis.call("EXPIRE", key, ttl)
return remaining - 1
`)

// Client wraps a Redis Cluster connection and exposes token-bucket operations.
type Client struct {
	rdb        *redis.ClusterClient
	bucketSize int64
	bucketTTL  time.Duration
}

// New creates a new Redis Cluster client.
func New(addrs []string, bucketSize int64, bucketTTL time.Duration) (*Client, error) {
	rdb := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:        addrs,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		PoolSize:     20,
		MinIdleConns: 5,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &Client{
		rdb:        rdb,
		bucketSize: bucketSize,
		bucketTTL:  bucketTTL,
	}, nil
}

// Decrement atomically decrements the token bucket for the given IP.
// Returns (remaining, nil) on success.
// Returns (-1, nil) when the bucket is exhausted (caller should block/throttle).
// Returns (0, err) on Redis error (caller should fail-open).
func (c *Client) Decrement(ctx context.Context, ip string) (int64, error) {
	key := fmt.Sprintf("bucket:{%s}", ip)

	result, err := tokenBucketScript.Run(
		ctx,
		c.rdb,
		[]string{key},
		c.bucketSize,
		int64(c.bucketTTL.Seconds()),
	).Int64()

	if err != nil {
		return 0, fmt.Errorf("token bucket script: %w", err)
	}

	return result, nil
}

// Close releases the underlying connection pool.
func (c *Client) Close() error {
	return c.rdb.Close()
}
