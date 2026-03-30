// Package engine implements the core rate-limit decision logic.
package engine

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ratelimiter/decision-engine/internal/config"
	"github.com/ratelimiter/decision-engine/internal/kafka"
	mlclient "github.com/ratelimiter/decision-engine/internal/ml"
	redisclient "github.com/ratelimiter/decision-engine/internal/redis"
	pb "github.com/ratelimiter/decision-engine/proto"
)

// Action is the decision returned to Nginx.
type Action string

const (
	ActionAllow    Action = "allow"
	ActionThrottle Action = "throttle"
	ActionBlock    Action = "block"
)

// Decision is the JSON response sent back to the Lua layer.
type Decision struct {
	Action     Action `json:"action"`
	RetryAfter int    `json:"retry_after"` // seconds; 0 for allow/block
}

// Blocklist is the interface to add/remove IPs from the eBPF kernel map.
type Blocklist interface {
	Add(ip string) error
	Remove(ip string) error
}

// Engine orchestrates Redis, ML, Kafka, and the eBPF blocklist.
type Engine struct {
	cfg       *config.Config
	redis     *redisclient.Client
	ml        *mlclient.Client
	kafka     *kafka.Producer
	blocklist Blocklist
}

// New constructs a ready Engine.
func New(
	cfg *config.Config,
	redis *redisclient.Client,
	ml *mlclient.Client,
	kp *kafka.Producer,
	bl Blocklist,
) *Engine {
	return &Engine{
		cfg:       cfg,
		redis:     redis,
		ml:        ml,
		kafka:     kp,
		blocklist: bl,
	}
}

// Evaluate processes a feature vector and returns an enforcement decision.
func (e *Engine) Evaluate(ctx context.Context, fv *pb.FeatureVector) Decision {
	ip := fv.GetClientIp()

	// 1. Atomically decrement token bucket in Redis.
	remaining, redisErr := e.redis.Decrement(ctx, ip)
	bucketExhausted := redisErr != nil || remaining < 0

	if redisErr != nil {
		log.Warn().Err(redisErr).Str("ip", ip).Msg("engine: redis error, treating bucket as ok")
		bucketExhausted = false // fail-open on Redis error
	}

	// 2. Get anomaly score from ML sidecar (fail-safe: 0.0 on error).
	score, modelVer, _ := e.ml.Score(ctx, fv)

	log.Debug().
		Str("ip", ip).
		Float32("score", score).
		Int64("remaining", remaining).
		Bool("exhausted", bucketExhausted).
		Str("model", modelVer).
		Msg("engine: evaluated")

	// 3. Apply decision logic.
	var decision Decision

	switch {
	case score > float32(e.cfg.ScoreBlockThreshold) || bucketExhausted:
		decision = Decision{Action: ActionBlock, RetryAfter: 0}

		// Push IP to eBPF kernel block map and Kafka.
		if err := e.blocklist.Add(ip); err != nil {
			log.Warn().Err(err).Str("ip", ip).Msg("engine: ebpf block failed")
		}

		go e.kafka.PublishBlocked(context.Background(), ip)

	case score > float32(e.cfg.ScoreThrottleThreshold):
		// retry_after scales with score: higher anomaly = longer back-off.
		retryAfter := int(score * 2 * 60) // e.g. score=0.70 → 84s
		if retryAfter < 1 {
			retryAfter = 1
		}
		decision = Decision{Action: ActionThrottle, RetryAfter: retryAfter}

	default:
		decision = Decision{Action: ActionAllow, RetryAfter: 0}
	}

	// 4. Publish decision event to Kafka (async, best-effort).
	go e.kafka.PublishDecision(context.Background(), kafka.DecisionEvent{
		IP:        ip,
		Score:     score,
		Action:    string(decision.Action),
		Timestamp: time.Now().UTC(),
		FeatureVector: map[string]interface{}{
			"ja3_hash":        fv.GetJa3Hash(),
			"user_agent_hash": fv.GetUserAgentHash(),
			"req_rate_1s":     fv.GetReqRate_1S(),
			"req_rate_10s":    fv.GetReqRate_10S(),
			"req_rate_60s":    fv.GetReqRate_60S(),
			"http_method":     fv.GetHttpMethod(),
			"is_http2":        fv.GetIsHttp2(),
		},
	})

	return decision
}
