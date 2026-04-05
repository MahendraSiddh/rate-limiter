#!/usr/bin/env sh
# ─────────────────────────────────────────────────────────────────────────────
# scripts/init-cluster.sh
#
# One-shot Redis Cluster bootstrap script.
#
# Waits for all 6 nodes to accept TCP connections, then issues:
#   redis-cli --cluster create ... --cluster-replicas 1 --cluster-yes
#
# The cluster topology is assigned deterministically by redis-cli:
#   - First 3 addresses → Masters   (nodes 1–3)
#   - Last  3 addresses → Replicas  (nodes 4–6, one replica per master)
#
# Slot distribution (16384 total):
#   node-1 (primary): slots  0    – 5460
#   node-2 (primary): slots  5461 – 10922
#   node-3 (primary): slots  10923 – 16383
#
# Each group (ip → shard) key must use hash tags to ensure co-location:
#   "bucket:{192.168.1.1}" → CRC16("{192.168.1.1}") % 16384 → shard
# ─────────────────────────────────────────────────────────────────────────────

set -eu

NODES="
redis-node-1:6379
redis-node-2:6379
redis-node-3:6379
redis-node-4:6379
redis-node-5:6379
redis-node-6:6379
"

RETRY_MAX=30
RETRY_DELAY=2

# ── Wait for all nodes to be reachable ───────────────────────────────────────

echo "[init-cluster] Waiting for all 6 Redis nodes to be ready..."

for node in $NODES; do
  host="${node%%:*}"
  port="${node##*:}"
  attempts=0

  until redis-cli -h "$host" -p "$port" ping | grep -q PONG; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge "$RETRY_MAX" ]; then
      echo "[init-cluster] ERROR: $node did not become ready after $((RETRY_MAX * RETRY_DELAY))s"
      exit 1
    fi
    echo "[init-cluster] $node not ready yet (attempt $attempts/$RETRY_MAX), retrying in ${RETRY_DELAY}s..."
    sleep "$RETRY_DELAY"
  done

  echo "[init-cluster] ✓ $node is ready"
done

# ── Check if cluster is already initialised ───────────────────────────────────

CLUSTER_STATE=$(redis-cli -h redis-node-1 -p 6379 cluster info | grep cluster_state | tr -d '\r')

if [ "$CLUSTER_STATE" = "cluster_state:ok" ]; then
  echo "[init-cluster] Cluster already initialised — skipping."
  exit 0
fi

echo "[init-cluster] Bootstrapping Redis Cluster (3 masters + 3 replicas, 1 replica each)..."

# ── Create cluster ────────────────────────────────────────────────────────────
#
# --cluster-replicas 1  assigns 1 replica per master.
# redis-cli picks the replica assignment automatically:
#   masters → redis-node-1, redis-node-2, redis-node-3
#   replicas → redis-node-4, redis-node-5, redis-node-6

redis-cli --cluster create \
  redis-node-1:6379 \
  redis-node-2:6379 \
  redis-node-3:6379 \
  redis-node-4:6379 \
  redis-node-5:6379 \
  redis-node-6:6379 \
  --cluster-replicas 1 \
  --cluster-yes

# ── Validate ──────────────────────────────────────────────────────────────────

echo ""
echo "[init-cluster] Cluster created. Validating topology..."
redis-cli -h redis-node-1 -p 6379 cluster info

echo ""
echo "[init-cluster] Node roles:"
redis-cli -h redis-node-1 -p 6379 cluster nodes

echo ""
echo "[init-cluster] ✓ Redis Cluster is ready."
