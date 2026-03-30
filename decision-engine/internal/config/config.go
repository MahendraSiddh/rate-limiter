// Package config loads all service configuration from environment variables.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the full runtime configuration for the decision engine.
type Config struct {
	// Servers
	UnixSocketPath string
	HTTPPort       string

	// Redis Cluster
	RedisAddrs []string

	// Token bucket
	BucketSize int64         // max tokens per window
	BucketTTL  time.Duration // key expiry / window size

	// Kafka
	KafkaBrokers []string

	// ML sidecar gRPC address (host:port)
	MLSidecarAddr string

	// Decision thresholds
	ScoreBlockThreshold    float64
	ScoreThrottleThreshold float64
}

// Load reads configuration from the environment, applying defaults where needed.
func Load() *Config {
	return &Config{
		UnixSocketPath: getEnv("UNIX_SOCKET_PATH", "/tmp/decision.sock"),
		HTTPPort:       getEnv("HTTP_PORT", "8080"),

		RedisAddrs: splitCSV(getEnv("REDIS_ADDRS", "localhost:6379")),

		BucketSize: getEnvInt64("BUCKET_SIZE", 100),
		BucketTTL:  time.Duration(getEnvInt64("BUCKET_TTL_SECONDS", 60)) * time.Second,

		KafkaBrokers: splitCSV(getEnv("KAFKA_BROKERS", "localhost:9092")),

		MLSidecarAddr: getEnv("ML_SIDECAR_ADDR", "localhost:50051"),

		ScoreBlockThreshold:    getEnvFloat("SCORE_BLOCK_THRESHOLD", 0.85),
		ScoreThrottleThreshold: getEnvFloat("SCORE_THROTTLE_THRESHOLD", 0.60),
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvInt64(key string, defaultVal int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultVal
}

func getEnvFloat(key string, defaultVal float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return defaultVal
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
