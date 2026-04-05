"""
Offline retraining script for per-client Isolation Forest models.

Usage (as a cron job)
---------------------
  0 3 * * * /usr/local/bin/python /app/training/retrain.py >> /var/log/retrain.log 2>&1

What it does
------------
1. Reads the last 7 days of feature vectors from Kafka topic
   "rate-limit-decisions" (key: client_ip, value: JSON FeatureVector).
2. Groups vectors by client IP.
3. Retrains a per-client IsolationForest for each IP with enough samples
   and persists the model to Redis.
4. Aggregates ALL vectors into a new global baseline model and saves it
   to disk at models/baseline_iso.pkl.

Environment variables
---------------------
  KAFKA_BOOTSTRAP_SERVERS   comma-separated brokers (default: kafka:9092)
  KAFKA_TOPIC               topic name (default: rate-limit-decisions)
  KAFKA_GROUP_ID            consumer group (default: ml-retrain)
  KAFKA_LOOKBACK_DAYS       days of data to consume (default: 7)
  REDIS_URL                 Redis URL (default: redis://redis:6379/0)
  LOG_LEVEL                 Python log level (default: INFO)
"""

from __future__ import annotations

import collections
import json
import logging
import os
import pathlib
import time
from datetime import datetime, timedelta, timezone

import joblib
import redis
from kafka import KafkaConsumer
from sklearn.ensemble import IsolationForest

from models.isolation_forest import (
    MIN_SAMPLES_FOR_FIT,
    IsolationForestModel,
    _BASELINE_PATH,
    _encode_feature_vector,
)

# ---------------------------------------------------------------------------
# Logging / config
# ---------------------------------------------------------------------------

logging.basicConfig(
    level=os.getenv("LOG_LEVEL", "INFO").upper(),
    format="%(asctime)s %(levelname)-8s %(name)s — %(message)s",
)
logger = logging.getLogger(__name__)

KAFKA_BOOTSTRAP: str = os.getenv("KAFKA_BOOTSTRAP_SERVERS", "kafka:9092")
KAFKA_TOPIC: str = os.getenv("KAFKA_TOPIC", "rate-limit-decisions")
KAFKA_GROUP: str = os.getenv("KAFKA_GROUP_ID", "ml-retrain")
LOOKBACK_DAYS: int = int(os.getenv("KAFKA_LOOKBACK_DAYS", "7"))
REDIS_URL: str = os.getenv("REDIS_URL", "redis://redis:6379/0")

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _cutoff_ms() -> int:
    """Unix timestamp in milliseconds for (now - LOOKBACK_DAYS)."""
    cutoff = datetime.now(tz=timezone.utc) - timedelta(days=LOOKBACK_DAYS)
    return int(cutoff.timestamp() * 1000)


def _consume_feature_vectors() -> dict[str, list[dict]]:
    """Return a dict mapping client_ip → list of feature-vector dicts.

    Reads all messages in KAFKA_TOPIC with timestamps >= cutoff.
    """
    consumer = KafkaConsumer(
        KAFKA_TOPIC,
        bootstrap_servers=KAFKA_BOOTSTRAP.split(","),
        group_id=KAFKA_GROUP,
        auto_offset_reset="earliest",
        enable_auto_commit=False,
        consumer_timeout_ms=30_000,   # stop after 30 s of silence
        value_deserializer=lambda b: json.loads(b.decode("utf-8", errors="replace")),
    )

    cutoff = _cutoff_ms()
    vectors_by_ip: dict[str, list[dict]] = collections.defaultdict(list)
    total = 0

    logger.info(
        "Consuming from topic=%s  cutoff=%s  brokers=%s",
        KAFKA_TOPIC,
        datetime.fromtimestamp(cutoff / 1000, tz=timezone.utc).isoformat(),
        KAFKA_BOOTSTRAP,
    )

    try:
        for msg in consumer:
            if msg.timestamp < cutoff:
                continue  # older than our lookback window

            fv: dict = msg.value
            ip: str = fv.get("client_ip", "")
            if ip:
                vectors_by_ip[ip].append(fv)
                total += 1
    except Exception:  # noqa: BLE001
        logger.exception("Error reading from Kafka")
    finally:
        consumer.close()

    logger.info("Consumed %d feature vectors for %d unique IPs", total, len(vectors_by_ip))
    return dict(vectors_by_ip)


def _retrain_per_client(
    iso_model: IsolationForestModel,
    vectors_by_ip: dict[str, list[dict]],
) -> None:
    """Retrain and persist a per-client IF model for each qualifying IP."""
    retrained = 0
    skipped = 0
    for ip, vectors in vectors_by_ip.items():
        if len(vectors) >= MIN_SAMPLES_FOR_FIT:
            iso_model.fit(ip, vectors)
            retrained += 1
        else:
            skipped += 1
    logger.info("Retrained %d per-client models, skipped %d (too few samples)", retrained, skipped)


def _retrain_baseline(all_vectors: list[dict]) -> None:
    """Retrain the global baseline model using all available data."""
    if len(all_vectors) < MIN_SAMPLES_FOR_FIT:
        logger.warning(
            "Only %d vectors total; skipping baseline retrain (need %d)",
            len(all_vectors),
            MIN_SAMPLES_FOR_FIT,
        )
        return

    import numpy as np  # local import to keep top-level imports fast

    X = np.array([_encode_feature_vector(fv) for fv in all_vectors])
    model = IsolationForest(n_estimators=200, contamination=0.05, random_state=42, n_jobs=-1)
    model.fit(X)

    backup = _BASELINE_PATH.with_suffix(".pkl.bak")
    if _BASELINE_PATH.exists():
        _BASELINE_PATH.rename(backup)

    _BASELINE_PATH.parent.mkdir(parents=True, exist_ok=True)
    joblib.dump(model, _BASELINE_PATH)
    logger.info(
        "Saved new baseline IF model (%d samples) to %s",
        len(X),
        _BASELINE_PATH,
    )
    if backup.exists():
        backup.unlink()


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> None:
    start = time.monotonic()
    logger.info("=== ML retrain job started ===")

    redis_client = redis.from_url(REDIS_URL, decode_responses=False)
    iso_model = IsolationForestModel(redis_client)

    vectors_by_ip = _consume_feature_vectors()

    if not vectors_by_ip:
        logger.warning("No feature vectors found in Kafka; nothing to retrain.")
        return

    _retrain_per_client(iso_model, vectors_by_ip)

    all_vectors: list[dict] = [fv for vectors in vectors_by_ip.values() for fv in vectors]
    _retrain_baseline(all_vectors)

    elapsed = time.monotonic() - start
    logger.info("=== ML retrain job finished in %.1f s ===", elapsed)


if __name__ == "__main__":
    main()
