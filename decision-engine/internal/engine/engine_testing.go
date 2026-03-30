// engine_testing.go — testability helpers.
// Separated from engine.go so production code has no test-only imports.
package engine

import (
	"context"

	pb "github.com/ratelimiter/decision-engine/proto"
	"github.com/ratelimiter/decision-engine/internal/config"
)

// ── Interfaces for dependency injection ───────────────────────────────────────

// RedisDecrementor is the Redis subset the engine needs.
type RedisDecrementor interface {
	Decrement(ctx context.Context, ip string) (int64, error)
}

// MLScorer is the ML-sidecar subset the engine needs.
type MLScorer interface {
	Score(ctx context.Context, fv *pb.FeatureVector) (float32, string, error)
}

// KafkaPublisher is the Kafka subset the engine needs.
type KafkaPublisher interface {
	// intentionally empty — published in goroutines; test engine swallows them
}

// ── Test engine ───────────────────────────────────────────────────────────────

// testEngine is a stripped-down Engine usable without real dependencies.
type testEngine struct {
	cfg       *config.Config
	remaining int64
	score     float32
}

// ReturnsRemaining is a constructor helper for NewForTesting.
type ReturnsRemaining int64

// ReturnsScore is a constructor helper for NewForTesting.
type ReturnsScore float32

// NewForTesting builds a lightweight Engine backed by stub values.
func NewForTesting(cfg *config.Config, remaining ReturnsRemaining, score ReturnsScore) *testEngine {
	return &testEngine{
		cfg:       cfg,
		remaining: int64(remaining),
		score:     float32(score),
	}
}

// Evaluate mirrors Engine.Evaluate using stub values.
func (e *testEngine) Evaluate(_ context.Context, fv *pb.FeatureVector) Decision {
	bucketExhausted := e.remaining < 0
	score := e.score

	switch {
	case score > float32(e.cfg.ScoreBlockThreshold) || bucketExhausted:
		return Decision{Action: ActionBlock}

	case score > float32(e.cfg.ScoreThrottleThreshold):
		retryAfter := int(score * 2 * 60)
		if retryAfter < 1 {
			retryAfter = 1
		}
		return Decision{Action: ActionThrottle, RetryAfter: retryAfter}

	default:
		return Decision{Action: ActionAllow}
	}
}
