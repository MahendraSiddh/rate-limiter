// Package main is the entry point for the decision engine.
//
// It starts two servers concurrently:
//   - A Unix domain socket server at /tmp/decision.sock (used by Nginx Lua)
//   - An HTTP server on :8080 (health, metrics, eBPF management)
//
// Both servers perform graceful shutdown on SIGINT / SIGTERM.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ginprometheus "github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ratelimiter/decision-engine/cmd/ebpf"
	"github.com/ratelimiter/decision-engine/internal/config"
	"github.com/ratelimiter/decision-engine/internal/engine"
	"github.com/ratelimiter/decision-engine/internal/kafka"
	mlclient "github.com/ratelimiter/decision-engine/internal/ml"
	redisclient "github.com/ratelimiter/decision-engine/internal/redis"
	pb "github.com/ratelimiter/decision-engine/proto"
)

func main() {
	// ── Structured logging ───────────────────────────────────────────────────
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	// ── Configuration ────────────────────────────────────────────────────────
	cfg := config.Load()

	log.Info().
		Str("socket", cfg.UnixSocketPath).
		Str("http_port", cfg.HTTPPort).
		Strs("redis", cfg.RedisAddrs).
		Str("ml", cfg.MLSidecarAddr).
		Msg("decision-engine starting")

	// ── Redis ────────────────────────────────────────────────────────────────
	redisClient, err := redisclient.New(cfg.RedisAddrs, cfg.BucketSize, cfg.BucketTTL)
	if err != nil {
		log.Fatal().Err(err).Msg("redis: init failed")
	}
	defer redisClient.Close()

	// ── Kafka ────────────────────────────────────────────────────────────────
	kafkaProducer := kafka.New(cfg.KafkaBrokers)
	defer kafkaProducer.Close()

	// ── ML sidecar ───────────────────────────────────────────────────────────
	mlClient, err := mlclient.New(cfg.MLSidecarAddr)
	if err != nil {
		// Non-fatal: engine will use score=0.0 (fail-safe)
		log.Warn().Err(err).Msg("ml: grpc dial failed, running without ML scoring")
	}
	if mlClient != nil {
		defer mlClient.Close()
	}

	// ── eBPF blocklist ───────────────────────────────────────────────────────
	bl := ebpf.New()
	defer bl.Close()

	// ── Engine ───────────────────────────────────────────────────────────────
	eng := engine.New(cfg, redisClient, mlClient, kafkaProducer, bl)

	// ── Graceful shutdown context ─────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Unix socket server ────────────────────────────────────────────────────
	go runUnixSocket(ctx, cfg.UnixSocketPath, eng)

	// ── HTTP server ───────────────────────────────────────────────────────────
	go runHTTP(ctx, cfg.HTTPPort, bl)

	// Wait for shutdown signal
	<-ctx.Done()
	log.Info().Msg("shutdown: signal received, draining...")

	// Give goroutines a moment to finish in-flight requests.
	time.Sleep(2 * time.Second)
	log.Info().Msg("shutdown: complete")
}

// ── Unix socket server ────────────────────────────────────────────────────────

func runUnixSocket(ctx context.Context, socketPath string, eng *engine.Engine) {
	// Remove stale socket file from a previous run.
	_ = os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatal().Err(err).Str("path", socketPath).Msg("unix socket: listen failed")
	}
	defer ln.Close()

	log.Info().Str("path", socketPath).Msg("unix socket: listening")

	// Close listener when context is cancelled.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Error().Err(err).Msg("unix socket: accept error")
			continue
		}
		go handleConn(conn, eng)
	}
}

func handleConn(conn net.Conn, eng *engine.Engine) {
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(200 * time.Millisecond))

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Bytes()

		var fv pb.FeatureVector
		if err := json.Unmarshal(line, &fv); err != nil {
			log.Warn().Err(err).Msg("unix socket: bad JSON, sending allow")
			writeResponse(conn, engine.Decision{Action: engine.ActionAllow})
			continue
		}

		decision := eng.Evaluate(context.Background(), &fv)
		writeResponse(conn, decision)
	}
}

func writeResponse(conn net.Conn, d engine.Decision) {
	b, _ := json.Marshal(d)
	b = append(b, '\n')
	conn.Write(b)
}

// ── HTTP server ───────────────────────────────────────────────────────────────

func runHTTP(ctx context.Context, port string, bl *ebpf.Blocklist) {
	router := ginprometheus.New()
	router.SetTrustedProxies(nil)

	// Health check
	router.GET("/health", func(c *ginprometheus.Context) {
		c.JSON(http.StatusOK, ginprometheus.H{"status": "ok"})
	})

	// Prometheus metrics
	router.GET("/metrics", func(c *ginprometheus.Context) {
		promhttp.Handler().ServeHTTP(c.Writer, c.Request)
	})

	// eBPF block/unblock management
	bl.RegisterRoutes(router.Group("/"))

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      router,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Info().Str("port", port).Msg("http: listening")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal().Err(err).Msg("http: server error")
	}
}
