# Adaptive API Rate Limiter with ML-Driven Anomaly Detection

A production-grade, multi-layer rate limiting system that combines deterministic
algorithms with machine learning to adaptively throttle API traffic.

## Features

- **3-layer defense**: eBPF kernel filtering → OpenResty L7 proxy → Go decision engine
- **ML anomaly scoring**: Real-time anomaly detection via Python gRPC sidecar (Isolation Forest / ONNX)
- **Distributed state**: Redis Cluster for sliding-window counters across nodes
- **Event-driven**: Kafka for async event streaming and analytics pipeline
- **Time-series analytics**: TimescaleDB hypertables with continuous aggregates
- **Full observability**: Prometheus metrics + Grafana dashboards for every layer

## Architecture

```
Client → eBPF/XDP → OpenResty/Lua → Decision Engine (Go)
                                         ├── Redis Cluster (counters)
                                         ├── ML Sidecar (gRPC, anomaly score)
                                         ├── Kafka (events)
                                         └── TimescaleDB (analytics)
```

## Quick Start

```bash
# Launch the full stack
bash scripts/deploy.sh

# Run load tests
k6 run tests/load/rate_limit_test.js

# Dashboards
open http://localhost:3000   # Grafana (admin/admin)
open http://localhost:9090   # Prometheus
```

## Project Structure

```
rate-limiter/
├── nginx/                  # OpenResty + Lua configs
├── decision-engine/        # Go service (core rate-limit logic)
├── ml-sidecar/             # Python anomaly scoring service (gRPC)
├── ebpf/                   # C eBPF programs + loader scripts
├── redis/                  # Redis cluster configuration
├── kafka/                  # Kafka topic setup scripts
├── db/                     # PostgreSQL + TimescaleDB migrations
├── observability/          # Prometheus + Grafana dashboards
├── scripts/                # Setup + deployment scripts
├── tests/                  # Load tests (k6) + unit tests
├── docker-compose.yml      # Full stack orchestration
├── CLAUDE.md               # AI assistant architecture context
└── README.md
```

## Services & Ports

| Service           | Port  | Description                    |
|-------------------|-------|--------------------------------|
| Nginx / OpenResty | 80    | L7 proxy + Lua rate limiting   |
| Decision Engine   | 8080  | Central rate-limit logic (Go)  |
| ML Sidecar        | 50051 | Anomaly scoring (Python gRPC)  |
| Redis Cluster     | 6379  | Distributed counters (6 nodes) |
| Kafka             | 9092  | Event streaming                |
| PostgreSQL        | 5432  | TimescaleDB analytics          |
| Prometheus        | 9090  | Metrics collection             |
| Grafana           | 3000  | Dashboards & alerting          |

## Tech Stack

| Layer          | Technology                    |
|----------------|-------------------------------|
| Kernel         | eBPF / XDP (C)                |
| Proxy          | OpenResty (Nginx + Lua)       |
| Core Logic     | Go 1.22                       |
| ML Scoring     | Python 3.11, ONNX Runtime     |
| State Store    | Redis 7.2 Cluster             |
| Event Bus      | Apache Kafka                  |
| Database       | PostgreSQL 16 + TimescaleDB   |
| Monitoring     | Prometheus + Grafana          |
| Load Testing   | k6                            |

## License

MIT
