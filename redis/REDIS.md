# REDIS.md — Redis Cluster Design Reference

> Scope: `redis/` — covers cluster topology, key design, Lua scripts, sharding
> strategy, and monitoring.  Read alongside `CLAUDE.md` for full system context.

---

## 1. Cluster Topology

| Node | Role | Host Port | Cluster Bus Port | Slots |
|------|------|-----------|-----------------|-------|
| redis-node-1 | **Primary** | 7001 | 17001 | 0 – 5460 |
| redis-node-2 | **Primary** | 7002 | 17002 | 5461 – 10922 |
| redis-node-3 | **Primary** | 7003 | 17003 | 10923 – 16383 |
| redis-node-4 | Replica of node-1 | 7004 | 17004 | — |
| redis-node-5 | Replica of node-2 | 7005 | 17005 | — |
| redis-node-6 | Replica of node-3 | 7006 | 17006 | — |

Redis Cluster divides the keyspace into **16 384 hash slots**.  Each primary own
a contiguous range.  The `redis-cluster-init` container calls:

```sh
redis-cli --cluster create \
  redis-node-1:6379 redis-node-2:6379 redis-node-3:6379 \
  redis-node-4:6379 redis-node-5:6379 redis-node-6:6379 \
  --cluster-replicas 1 --cluster-yes
```

`redis-cli` assigns the first three addresses as masters and the last three as
replicas (one per master, round-robin).

---

## 2. Consistent Hashing — IP → Shard

Redis Cluster uses **CRC-16 mod 16384** as its hash function.  The Go client
(`go-redis v9 ClusterClient`) performs this computation client-side to route
every command to the correct node _without_ a proxy.

### Hash-tag convention

All application keys use a **hash tag** (`{...}`) so that keys belonging to
the same logical entity are guaranteed to land on the same slot:

```
bucket:{192.168.1.1}        → CRC16("192.168.1.1") % 16384
model:{client-abc}          → CRC16("client-abc")  % 16384
```

When a hash tag is present, Redis computes the slot using only the bytes
**inside the curly braces**, ignoring the prefix/suffix.  This allows
`EVALSHA` (Lua scripts) to operate on multiple keys of the same client in a
single round-trip (multi-key Lua is legal only when all keys are on the same
slot).

### Routing walk-through (Go client)

```
IP = "203.0.113.42"
key = "bucket:{203.0.113.42}"

hash_tag = "203.0.113.42"          # bytes inside {}
slot     = crc16(hash_tag) % 16384  # e.g. → slot 9182

slot 9182 falls in [5461, 10922]    # → Primary: redis-node-2:6379
```

The `ClusterClient` maintains an in-memory slot→node map refreshed via
`CLUSTER SLOTS` / `CLUSTER SHARDS` (Redis 7+ prefers `CLUSTER SHARDS`).
On a `MOVED` redirect (after failover or resharding), the client updates its
map automatically and retries the command transparently.

### Go client initialisation (from `internal/redis/client.go`)

```go
rdb := redis.NewClusterClient(&redis.ClusterOptions{
    // Seed addresses — client discovers all nodes from these
    Addrs:        []string{
        "redis-node-1:6379",
        "redis-node-2:6379",
        "redis-node-3:6379",
    },
    DialTimeout:  2 * time.Second,
    ReadTimeout:  500 * time.Millisecond,
    WriteTimeout: 500 * time.Millisecond,
    PoolSize:     20,
    MinIdleConns: 5,
})
```

The client seeds topology from **any subset** of nodes; full topology is
discovered automatically.  Production deployments should seed at least one
node per shard (i.e., all three primaries) to survive the failure of any
single seed address at startup.

### Failover behaviour

| Scenario | go-redis behaviour |
|----------|--------------------|
| Primary dies, replica not yet elected | Commands fail with `CLUSTERDOWN` until election completes (~3 s with default `cluster-node-timeout 5000`) |
| Replica elected as primary | Client receives `MOVED` → refreshes slot map → retries transparently |
| Network partition (minority side) | Primary refuses writes (`cluster-require-full-coverage no` allows reads on healthy shards) |

---

## 3. Key Schema

| Key Pattern | Type | TTL | Owner | Description |
|-------------|------|-----|-------|-------------|
| `bucket:{<ip>}` | Sorted Set | `WINDOW_SECONDS` (60 s) | Decision Engine | Sliding-window rate-limit counter |
| `model:{<client_id>}` | String (bytes) | `MODEL_TTL_SECONDS` (3600 s) | ML Sidecar | Per-client ONNX model snapshot |

All keys carry an explicit TTL so `volatile-lru` can evict them under memory
pressure without touching cluster metadata keys (which have no TTL and are
therefore eviction-immune).

---

## 4. Lua Scripts

### `lua-scripts/token_bucket.lua` — Sliding-window rate limiter

```
EVALSHA <sha> 1 bucket:{<ip>} <max_tokens> <window_seconds> <unix_ts>
```

| Step | Redis command | Purpose |
|------|--------------|---------|
| 1 | `ZREMRANGEBYSCORE key -inf (now - window)` | Prune expired entries |
| 2 | `ZCARD key` | Count in-window requests |
| 3a (allow) | `ZADD key <now> <unique_member>` | Record this request |
| 3a (allow) | `EXPIRE key <window_seconds>` | Reset idle TTL |
| 3b (deny) | `EXPIRE key <window_seconds>` | Prevent stale key leak |

Returns `1` (allow) or `0` (deny).

> **Why sorted set over simple counter?**  A simple `INCR`+`EXPIRE` counter
> resets the window on every expiry rather than sliding.  The sorted-set
> approach provides a true sliding window at the cost of O(log N) per insert
> vs O(1) for a counter — acceptable given bucket sizes ≤ 300 tokens/min.

### `lua-scripts/get_set_model.lua` — ML model check-and-return

```
EVALSHA <sha> 1 model:{<client_id>}
```

Returns:
- `nil`/`false` — key absent; caller loads baseline model from PostgreSQL.
- `{bytes, ttl}` — model blob + seconds until expiry.

Atomicity prevents a race where two ML Sidecar replicas simultaneously load
the baseline and overwrite each other's freshly trained model.

---

## 5. Memory & Eviction

```
maxmemory        512mb
maxmemory-policy volatile-lru
```

With `volatile-lru`, Redis evicts the **least-recently-used key that has a
TTL** when `512mb` is reached.  This means:

- Rate-bucket sorted sets (TTL = `WINDOW_SECONDS`) → evictable.
- ML model blobs (TTL = `MODEL_TTL_SECONDS`) → evictable.
- Cluster internal keys (`nodes.conf` metadata) → **never evicted** (no TTL).

If a bucket is evicted mid-window, the next request will see an empty set and
be allowed through (fail-open in favour of availability).  The window resets
cleanly — this is an acceptable trade-off for a cache-only node.

---

## 6. Persistence

```
appendonly no
save ""
```

All rate-limit state is **ephemeral by design**.  Cluster replication (one
replica per primary) provides HA.  On a full cluster restart counters reset,
causing a brief burst window — the eBPF/XDP layer and Nginx local counters
absorb this gap.

---

## 7. Monitoring (redis-exporter)

The `redis-exporter` sidecar (port **9121**) exposes Prometheus metrics.
Add this scrape config to `observability/prometheus/prometheus.yml`:

```yaml
- job_name: redis
  static_configs:
    - targets: ['redis-exporter:9121']
```

### Key metrics to track

| Metric | Alert threshold | What it means |
|--------|----------------|---------------|
| `redis_keyspace_hits_total / (hits + misses)` | < 80% | Cache hit rate per bucket |
| `redis_evicted_keys_total` (rate) | > 0 / 10 s | Memory pressure; consider raising `maxmemory` |
| `redis_commands_duration_seconds{quantile="0.99"}` | > 2 ms | p99 command latency |
| `redis_cluster_slots_assigned` | < 16384 | Cluster not fully covered |
| `redis_connected_clients` | spikes | Connection pool exhaustion |
| `redis_mem_fragmentation_ratio` | > 1.5 | Excessive memory fragmentation; consider `MEMORY PURGE` |

### Suggested Grafana panels

1. **Hit Rate** — `rate(redis_keyspace_hits_total[1m]) / (rate(redis_keyspace_hits_total[1m]) + rate(redis_keyspace_misses_total[1m]))`
2. **Eviction Rate** — `rate(redis_evicted_keys_total[1m])`
3. **p99 Latency** — `histogram_quantile(0.99, rate(redis_commands_duration_seconds_bucket[1m]))`
4. **Cluster Health** — `redis_cluster_slots_assigned == 16384`

---

## 8. Operations Runbook

### Bootstrap cluster from scratch

```bash
# Start all 6 nodes + init container
docker compose -f redis/docker-compose.cluster.yml up -d

# Tail init logs
docker logs -f redis-cluster-init

# Verify
redis-cli -h localhost -p 7001 cluster info | grep cluster_state
# Expected: cluster_state:ok
```

### Manual resharding

```bash
redis-cli --cluster rebalance redis-node-1:6379 --cluster-use-empty-masters
```

### Check slot assignment

```bash
redis-cli -h localhost -p 7001 cluster shards
```

### Flush all rate-limit buckets (emergency)

```bash
# Flush all keys matching bucket:* across the cluster
for port in 7001 7002 7003; do
  redis-cli -h localhost -p $port --scan --pattern 'bucket:*' | \
    xargs -r redis-cli -h localhost -p $port DEL
done
```
