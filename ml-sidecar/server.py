"""
Async gRPC server for the ML anomaly-scoring sidecar.

Ports
-----
  50051 — gRPC (MLScorer service)
  8000  — HTTP  (/health endpoint for Docker healthcheck)

Environment variables
---------------------
  REDIS_URL      Redis connection string (default: redis://redis:6379/0)
  GRPC_PORT      gRPC listen port       (default: 50051)
  HTTP_PORT      HTTP listen port       (default: 8000)
  LOG_LEVEL      Python log level       (default: INFO)
"""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import time
from concurrent.futures import ThreadPoolExecutor

import grpc
import redis
from aiohttp import web

# Generated stubs  (created by `make proto`)
import proto.scorer_pb2 as scorer_pb2
import proto.scorer_pb2_grpc as scorer_pb2_grpc

from scorer import Scorer

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

logging.basicConfig(
    level=os.getenv("LOG_LEVEL", "INFO").upper(),
    format="%(asctime)s %(levelname)-8s %(name)s — %(message)s",
)
logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

REDIS_URL: str = os.getenv("REDIS_URL", "redis://redis:6379/0")
GRPC_PORT: int = int(os.getenv("GRPC_PORT", "50051"))
HTTP_PORT: int = int(os.getenv("HTTP_PORT", "8000"))

# ---------------------------------------------------------------------------
# gRPC service implementation
# ---------------------------------------------------------------------------

_STARTUP_TIME = time.time()


def _fv_to_dict(fv: scorer_pb2.FeatureVector) -> dict:
    """Convert a protobuf FeatureVector message into a plain Python dict."""
    return {
        "client_ip": fv.client_ip,
        "ja3_hash": fv.ja3_hash,
        "user_agent_hash": fv.user_agent_hash,
        "req_rate_1s": fv.req_rate_1s,
        "req_rate_10s": fv.req_rate_10s,
        "req_rate_60s": fv.req_rate_60s,
        "http_method": fv.http_method,
        "uri_hash": fv.uri_hash,
        "content_length": fv.content_length,
        "header_count": fv.header_count,
        "is_http2": fv.is_http2,
        "unix_timestamp": fv.unix_timestamp,
    }


class MLScorerServicer(scorer_pb2_grpc.MLScorerServicer):
    """Implements the MLScorer gRPC service."""

    def __init__(self, scorer: Scorer) -> None:
        self._scorer = scorer

    async def Score(
        self,
        request: scorer_pb2.FeatureVector,
        context: grpc.aio.ServicerContext,
    ) -> scorer_pb2.ScoreResponse:
        fv_dict = _fv_to_dict(request)
        # Run CPU-bound scoring in a thread pool to avoid blocking the event loop
        loop = asyncio.get_running_loop()
        result = await loop.run_in_executor(None, self._scorer.score, fv_dict)
        return scorer_pb2.ScoreResponse(
            anomaly_score=result.anomaly_score,
            model_version=result.model_version,
        )


# ---------------------------------------------------------------------------
# HTTP /health endpoint  (aiohttp)
# ---------------------------------------------------------------------------


async def health_handler(request: web.Request) -> web.Response:
    uptime = time.time() - _STARTUP_TIME
    return web.json_response({"status": "ok", "uptime_seconds": round(uptime, 2)})


async def start_http_server(port: int) -> web.AppRunner:
    app = web.Application()
    app.router.add_get("/health", health_handler)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "0.0.0.0", port)
    await site.start()
    logger.info("HTTP /health listening on port %d", port)
    return runner


# ---------------------------------------------------------------------------
# gRPC server
# ---------------------------------------------------------------------------


async def start_grpc_server(scorer: Scorer, port: int) -> grpc.aio.Server:
    server = grpc.aio.server(
        # Limit concurrent threads for CPU-bound inference
        migration_thread_pool=ThreadPoolExecutor(max_workers=4),
        options=[
            ("grpc.max_send_message_length", 1 * 1024 * 1024),
            ("grpc.max_receive_message_length", 1 * 1024 * 1024),
            # Keep connections healthy
            ("grpc.keepalive_time_ms", 30_000),
            ("grpc.keepalive_timeout_ms", 5_000),
            ("grpc.keepalive_permit_without_calls", 1),
        ],
    )
    scorer_pb2_grpc.add_MLScorerServicer_to_server(MLScorerServicer(scorer), server)
    server.add_insecure_port(f"[::]:{port}")
    await server.start()
    logger.info("gRPC MLScorer listening on port %d", port)
    return server


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


async def main() -> None:
    # Redis connection (synchronous client, used from ThreadPoolExecutor)
    redis_client = redis.from_url(REDIS_URL, decode_responses=False)
    scorer = Scorer(redis_client)

    # Start both servers concurrently
    grpc_server = await start_grpc_server(scorer, GRPC_PORT)
    http_runner = await start_http_server(HTTP_PORT)

    # Graceful shutdown on SIGTERM / SIGINT
    loop = asyncio.get_running_loop()
    stop_event = asyncio.Event()

    def _shutdown() -> None:
        logger.info("Shutdown signal received")
        stop_event.set()

    loop.add_signal_handler(signal.SIGTERM, _shutdown)
    loop.add_signal_handler(signal.SIGINT, _shutdown)

    logger.info("ML Sidecar ready (gRPC=%d, HTTP=%d)", GRPC_PORT, HTTP_PORT)
    await stop_event.wait()

    logger.info("Stopping gRPC server…")
    await grpc_server.stop(grace=5)
    logger.info("Stopping HTTP server…")
    await http_runner.cleanup()
    logger.info("Shutdown complete")


if __name__ == "__main__":
    asyncio.run(main())
