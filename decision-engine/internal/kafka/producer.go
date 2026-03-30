// Package kafka provides an async Kafka producer for publishing rate-limit decisions.
package kafka

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rs/zerolog/log"
	kafkago "github.com/segmentio/kafka-go"
)

const (
	topicDecisions = "rate-limit-decisions"
	topicBlocked   = "blocked-ips"
)

// DecisionEvent is the payload published to the rate-limit-decisions topic.
type DecisionEvent struct {
	IP            string      `json:"ip"`
	Score         float32     `json:"score"`
	Action        string      `json:"action"`
	Timestamp     time.Time   `json:"timestamp"`
	FeatureVector interface{} `json:"feature_vector"`
}

// Producer publishes decision events to Kafka asynchronously.
type Producer struct {
	decisions *kafkago.Writer
	blocked   *kafkago.Writer
}

// New returns a Producer connected to the given brokers.
func New(brokers []string) *Producer {
	newWriter := func(topic string) *kafkago.Writer {
		return &kafkago.Writer{
			Addr:                   kafkago.TCP(brokers...),
			Topic:                  topic,
			Balancer:               &kafkago.LeastBytes{},
			BatchSize:              100,
			BatchTimeout:           10 * time.Millisecond,
			WriteTimeout:           5 * time.Second,
			RequiredAcks:           kafkago.RequireOne,
			AllowAutoTopicCreation: false,
		}
	}

	return &Producer{
		decisions: newWriter(topicDecisions),
		blocked:   newWriter(topicBlocked),
	}
}

// PublishDecision sends a DecisionEvent to the rate-limit-decisions topic.
// Errors are logged and swallowed — Kafka publishing is best-effort.
func (p *Producer) PublishDecision(ctx context.Context, evt DecisionEvent) {
	b, err := json.Marshal(evt)
	if err != nil {
		log.Error().Err(err).Msg("kafka: marshal decision event")
		return
	}

	if err := p.decisions.WriteMessages(ctx, kafkago.Message{
		Key:   []byte(evt.IP),
		Value: b,
	}); err != nil {
		log.Warn().Err(err).Str("topic", topicDecisions).Msg("kafka: publish decision")
	}
}

// PublishBlocked sends a blocked IP to the blocked-ips topic.
func (p *Producer) PublishBlocked(ctx context.Context, ip string) {
	msg := struct {
		IP        string    `json:"ip"`
		Timestamp time.Time `json:"timestamp"`
	}{IP: ip, Timestamp: time.Now().UTC()}

	b, err := json.Marshal(msg)
	if err != nil {
		log.Error().Err(err).Msg("kafka: marshal blocked-ip event")
		return
	}

	if err := p.blocked.WriteMessages(ctx, kafkago.Message{
		Key:   []byte(ip),
		Value: b,
	}); err != nil {
		log.Warn().Err(err).Str("topic", topicBlocked).Msg("kafka: publish blocked-ip")
	}
}

// Close flushes and closes both writers.
func (p *Producer) Close() error {
	var errs []error
	if err := p.decisions.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := p.blocked.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
