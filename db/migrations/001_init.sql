-- 001_init.sql
-- Initial schema for the Adaptive Rate Limiter
-- Uses TimescaleDB hypertables for time-series data

-- Enable TimescaleDB extension
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- ─────────────────────────────────────────────
--  Rate limit rules (static configuration)
-- ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS rate_limit_rules (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    description     TEXT,
    window_seconds  INTEGER NOT NULL DEFAULT 60,
    max_requests    INTEGER NOT NULL DEFAULT 100,
    burst_size      INTEGER NOT NULL DEFAULT 20,
    scope           TEXT NOT NULL DEFAULT 'ip',  -- ip | api_key | user | global
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─────────────────────────────────────────────
--  Rate limit events (time-series, hypertable)
-- ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS rate_limit_events (
    time            TIMESTAMPTZ NOT NULL,
    client_id       TEXT NOT NULL,
    rule_id         BIGINT REFERENCES rate_limit_rules(id),
    request_count   INTEGER NOT NULL DEFAULT 1,
    decision        TEXT NOT NULL,          -- allowed | throttled | blocked
    anomaly_score   DOUBLE PRECISION,
    latency_ms      DOUBLE PRECISION,
    metadata        JSONB
);

SELECT create_hypertable('rate_limit_events', 'time', if_not_exists => TRUE);

-- Indexes for common query patterns
CREATE INDEX idx_events_client   ON rate_limit_events (client_id, time DESC);
CREATE INDEX idx_events_decision ON rate_limit_events (decision, time DESC);

-- ─────────────────────────────────────────────
--  Anomaly baselines (per-client statistics)
-- ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS anomaly_baselines (
    client_id           TEXT NOT NULL,
    window_start        TIMESTAMPTZ NOT NULL,
    avg_rps             DOUBLE PRECISION,
    p99_rps             DOUBLE PRECISION,
    std_dev             DOUBLE PRECISION,
    sample_count        INTEGER,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (client_id, window_start)
);

SELECT create_hypertable('anomaly_baselines', 'window_start', if_not_exists => TRUE);

-- ─────────────────────────────────────────────
--  Continuous aggregate for hourly stats
-- ─────────────────────────────────────────────
CREATE MATERIALIZED VIEW IF NOT EXISTS hourly_request_stats
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 hour', time) AS bucket,
    client_id,
    COUNT(*)                    AS total_requests,
    SUM(CASE WHEN decision = 'blocked' THEN 1 ELSE 0 END) AS blocked_count,
    AVG(anomaly_score)          AS avg_anomaly_score,
    AVG(latency_ms)             AS avg_latency_ms
FROM rate_limit_events
GROUP BY bucket, client_id
WITH NO DATA;
