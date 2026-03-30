// Package ml provides a gRPC client for the Python ML anomaly-scoring sidecar.
package ml

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/ratelimiter/decision-engine/proto"
)

const callDeadline = 100 * time.Millisecond

// Client wraps the MLScorer gRPC stub.
type Client struct {
	conn   *grpc.ClientConn
	scorer pb.MLScorerClient
}

// New dials the ML sidecar and returns a ready Client.
func New(addr string) (*Client, error) {
	//nolint:staticcheck // grpc.Dial is deprecated in v1.63 but required for v1.61
	conn, err := grpc.Dial(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}

	return &Client{
		conn:   conn,
		scorer: pb.NewMLScorerClient(conn),
	}, nil
}

// Score calls the ML sidecar and returns (anomaly_score, model_version, error).
// On any error the score defaults to 0.0 (fail-safe: treat unknown as normal).
func (c *Client) Score(ctx context.Context, fv *pb.FeatureVector) (float32, string, error) {
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()

	resp, err := c.scorer.Score(ctx, fv)
	if err != nil {
		log.Warn().Err(err).Str("ip", fv.GetClientIp()).Msg("ml: score call failed, defaulting to 0.0")
		return 0.0, "", err
	}

	return resp.GetAnomalyScore(), resp.GetModelVersion(), nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
