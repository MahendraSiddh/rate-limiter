"""
Per-client Isolation Forest anomaly model.

Each client IP gets its own IsolationForest trained on its historical
feature vectors.  Serialised models are stored in Redis so they survive
service restarts.

Key layout
----------
  "model:iso:{ip}"   → joblib-serialised IsolationForest (TTL 7 days)

On first request for an unknown IP the global baseline model is loaded
from disk (models/baseline_iso.pkl) and used as a fallback until enough
data has been collected to fit a per-client model.
"""

from __future__ import annotations

import io
import logging
import os
import pathlib
from typing import Optional

import joblib
import numpy as np
import redis
from sklearn.ensemble import IsolationForest

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

_REDIS_TTL_SECONDS = 7 * 24 * 3600          # 7 days
_REDIS_KEY_PREFIX = "model:iso:"
_BASELINE_PATH = pathlib.Path(__file__).parent / "baseline_iso.pkl"

# Feature dimension: 9 numeric fields from FeatureVector
#   req_rate_1s, req_rate_10s, req_rate_60s,
#   content_length, header_count, is_http2,
#   _http_method_enc, _ja3_len, _ua_len
FEATURE_DIM = 9

# Minimum samples before we retrain a per-client model
MIN_SAMPLES_FOR_FIT = 20


def _encode_feature_vector(fv: dict) -> np.ndarray:
    """Convert a raw FeatureVector dict into a fixed-length float array."""
    return np.array(
        [
            float(fv.get("req_rate_1s", 0.0)),
            float(fv.get("req_rate_10s", 0.0)),
            float(fv.get("req_rate_60s", 0.0)),
            float(fv.get("content_length", 0)),
            float(fv.get("header_count", 0)),
            float(fv.get("is_http2", 0)),
            float(len(fv.get("http_method", ""))),
            float(len(fv.get("ja3_hash", ""))),
            float(len(fv.get("user_agent_hash", ""))),
        ],
        dtype=np.float64,
    )


class IsolationForestModel:
    """Thread-safe per-client Isolation Forest scorer backed by Redis."""

    def __init__(self, redis_client: redis.Redis) -> None:
        self._redis = redis_client
        self._baseline: Optional[IsolationForest] = self._load_baseline()

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def fit(self, ip: str, feature_history: list[dict]) -> None:
        """Retrain the per-client model and persist to Redis.

        Parameters
        ----------
        ip:
            Client IPv4/IPv6 string.
        feature_history:
            List of raw FeatureVector dicts (each as returned by the
            gRPC message → dict conversion).  Must contain at least
            MIN_SAMPLES_FOR_FIT entries.
        """
        if len(feature_history) < MIN_SAMPLES_FOR_FIT:
            logger.debug(
                "ip=%s  only %d samples available, skipping fit (need %d)",
                ip,
                len(feature_history),
                MIN_SAMPLES_FOR_FIT,
            )
            return

        X = np.array([_encode_feature_vector(fv) for fv in feature_history])

        model = IsolationForest(
            n_estimators=100,
            contamination=0.05,
            random_state=42,
            n_jobs=-1,
        )
        model.fit(X)
        self._store(ip, model)
        logger.info("ip=%s  per-client IF model trained on %d samples", ip, len(X))

    def score(self, ip: str, feature_vector: dict) -> float:
        """Return anomaly score in [0.0, 1.0] (higher → more anomalous).

        Parameters
        ----------
        ip:
            Client IPv4/IPv6 string.
        feature_vector:
            Single raw FeatureVector dict.
        """
        model = self._load(ip)
        if model is None:
            model = self._baseline
        if model is None:
            # No model at all – return neutral score
            logger.warning("ip=%s  no model available, returning 0.5", ip)
            return 0.5

        x = _encode_feature_vector(feature_vector).reshape(1, -1)

        # decision_function returns negative anomaly scores; lower = more anomalous.
        # We map to [0, 1]: score_raw in roughly [-0.5, 0.5] → invert and clip.
        raw = float(model.decision_function(x)[0])
        # Normalise: raw ∈ [-0.5, 0.5] → anomaly ∈ [0, 1]
        anomaly = float(np.clip(0.5 - raw, 0.0, 1.0))
        return anomaly

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _redis_key(self, ip: str) -> str:
        return f"{_REDIS_KEY_PREFIX}{ip}"

    def _store(self, ip: str, model: IsolationForest) -> None:
        buf = io.BytesIO()
        joblib.dump(model, buf)
        self._redis.setex(
            self._redis_key(ip),
            _REDIS_TTL_SECONDS,
            buf.getvalue(),
        )

    def _load(self, ip: str) -> Optional[IsolationForest]:
        data = self._redis.get(self._redis_key(ip))
        if data is None:
            return None
        try:
            return joblib.load(io.BytesIO(data))  # type: ignore[return-value]
        except Exception:  # noqa: BLE001
            logger.exception("ip=%s  failed to deserialise IF model from Redis", ip)
            return None

    def _load_baseline(self) -> Optional[IsolationForest]:
        if not _BASELINE_PATH.exists():
            logger.warning("Baseline model not found at %s", _BASELINE_PATH)
            return None
        try:
            model: IsolationForest = joblib.load(_BASELINE_PATH)
            logger.info("Loaded baseline IF model from %s", _BASELINE_PATH)
            return model
        except Exception:  # noqa: BLE001
            logger.exception("Failed to load baseline IF model")
            return None


def create_and_save_baseline(output_path: pathlib.Path | None = None) -> None:
    """Generate a minimal synthetic baseline model and save to disk.

    This is called once during Docker image build (or manually) so the
    sidecar always has a fallback model even before real data arrives.
    """
    rng = np.random.default_rng(0)
    X = rng.standard_normal((500, FEATURE_DIM))
    model = IsolationForest(n_estimators=100, contamination=0.05, random_state=42)
    model.fit(X)

    path = output_path or _BASELINE_PATH
    path.parent.mkdir(parents=True, exist_ok=True)
    joblib.dump(model, path)
    logger.info("Saved synthetic baseline IF model to %s", path)
