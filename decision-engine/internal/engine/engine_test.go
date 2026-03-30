package engine_test

import (
	"context"
	"testing"

	"github.com/ratelimiter/decision-engine/internal/config"
	"github.com/ratelimiter/decision-engine/internal/engine"
	pb "github.com/ratelimiter/decision-engine/proto"
)

// ── Fakes ─────────────────────────────────────────────────────────────────────

type fakeRedis struct{ remaining int64 }

func (f *fakeRedis) Decrement(_ context.Context, _ string) (int64, error) {
	return f.remaining, nil
}

type fakeML struct{ score float32 }

func (f *fakeML) Score(_ context.Context, _ *pb.FeatureVector) (float32, string, error) {
	return f.score, "test-v1", nil
}

type fakeKafka struct{ decisions []string }

func (f *fakeKafka) PublishDecision(_ context.Context, ip string, _ float32, action string) {
	f.decisions = append(f.decisions, action)
}
func (f *fakeKafka) PublishBlocked(_ context.Context, _ string) {}

type fakeBlocklist struct{ blocked []string }

func (f *fakeBlocklist) Add(ip string) error    { f.blocked = append(f.blocked, ip); return nil }
func (f *fakeBlocklist) Remove(_ string) error  { return nil }

// ── Helpers ───────────────────────────────────────────────────────────────────

func defaultCfg() *config.Config {
	return &config.Config{
		ScoreBlockThreshold:    0.85,
		ScoreThrottleThreshold: 0.60,
		BucketSize:             100,
	}
}

func vector(ip string) *pb.FeatureVector {
	return &pb.FeatureVector{
		ClientIp:      ip,
		HttpMethod:    "GET",
		UnixTimestamp: 1710000000,
	}
}

// engineWithFakes builds an Engine wired to simple fakes that satisfy the
// interfaces expected by engine.New via dependency injection.
// Because fakeRedis/fakeML don't implement the concrete types, we build
// the engine directly exercising its Evaluate logic by calling it with
// a shallow wrapper.
func newTestEngine(remaining int64, score float32) (*engine.Engine, *fakeBlocklist) {
	bl := &fakeBlocklist{}
	cfg := defaultCfg()

	// We test the decision logic by using a testable variant.
	// The engine package exposes EvaluateWith for testing.
	_ = remaining
	_ = score
	_ = cfg
	_ = bl
	return nil, bl
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestDecision_Allow(t *testing.T) {
	eng := engine.NewForTesting(
		defaultCfg(),
		engine.ReturnsRemaining(50),
		engine.ReturnsScore(0.10),
	)

	d := eng.Evaluate(context.Background(), vector("1.2.3.4"))
	if d.Action != engine.ActionAllow {
		t.Fatalf("expected allow, got %s", d.Action)
	}
}

func TestDecision_Throttle(t *testing.T) {
	eng := engine.NewForTesting(
		defaultCfg(),
		engine.ReturnsRemaining(50),
		engine.ReturnsScore(0.70),
	)

	d := eng.Evaluate(context.Background(), vector("1.2.3.4"))
	if d.Action != engine.ActionThrottle {
		t.Fatalf("expected throttle, got %s", d.Action)
	}
	if d.RetryAfter <= 0 {
		t.Fatalf("expected positive retry_after, got %d", d.RetryAfter)
	}
}

func TestDecision_Block_HighScore(t *testing.T) {
	eng := engine.NewForTesting(
		defaultCfg(),
		engine.ReturnsRemaining(50),
		engine.ReturnsScore(0.90),
	)

	d := eng.Evaluate(context.Background(), vector("1.2.3.4"))
	if d.Action != engine.ActionBlock {
		t.Fatalf("expected block, got %s", d.Action)
	}
}

func TestDecision_Block_Exhausted(t *testing.T) {
	eng := engine.NewForTesting(
		defaultCfg(),
		engine.ReturnsRemaining(-1), // bucket exhausted
		engine.ReturnsScore(0.10),
	)

	d := eng.Evaluate(context.Background(), vector("10.0.0.1"))
	if d.Action != engine.ActionBlock {
		t.Fatalf("expected block when bucket exhausted, got %s", d.Action)
	}
}
