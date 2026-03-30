#!/usr/bin/env bash
# deploy.sh — Build and launch the full stack
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$ROOT_DIR"

echo "=== Building all services ==="
docker compose build

echo "=== Starting infrastructure ==="
docker compose up -d zookeeper kafka postgres redis-node-{1..6} redis-cluster-init prometheus grafana

echo "Waiting for infrastructure to stabilize (15s)..."
sleep 15

echo "=== Creating Kafka topics ==="
docker compose exec kafka bash /kafka/create-topics.sh || \
    echo "Topics may already exist, continuing..."

echo "=== Starting application services ==="
docker compose up -d decision-engine ml-sidecar nginx

echo "=== Stack is up ==="
docker compose ps
echo ""
echo "  Nginx:            http://localhost:80"
echo "  Decision Engine:  http://localhost:8080"
echo "  Grafana:          http://localhost:3000  (admin/admin)"
echo "  Prometheus:       http://localhost:9090"
