"""
Scorer: combines Isolation Forest and LSTM anomaly scores into a single
weighted anomaly score.

  final_score = 0.6 × isolation_score + 0.4 × lstm_score

The model_version string is derived from the git-tag / package version
baked in at build time (falls back to a timestamp-based string).
"""

from __future__ import annotations

import importlib.metadata
import logging
import time
from dataclasses import dataclass

import redis

from models.isolation_forest import IsolationForestModel
from models.lstm_model import LSTMModel

logger = logging.getLogger(__name__)

# Weights must sum to 1.0
_ISO_WEIGHT = 0.6
_LSTM_WEIGHT = 0.4


def _resolve_version() -> str:
    try:
        return importlib.metadata.version("ml-sidecar")
    except importlib.metadata.PackageNotFoundError:
        # Fallback during local development / before build
        return f"dev-{int(time.time())}"


_MODEL_VERSION: str = _resolve_version()


@dataclass(frozen=True, slots=True)
class ScoreResult:
    anomaly_score: float   # [0.0, 1.0]
    model_version: str


class Scorer:
    """Combines the two model scores into the final anomaly score."""

    def __init__(self, redis_client: redis.Redis) -> None:
        self._iso = IsolationForestModel(redis_client)
        self._lstm = LSTMModel(redis_client)

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def score(self, feature_vector: dict) -> ScoreResult:
        """Compute and return the combined anomaly score.

        Parameters
        ----------
        feature_vector:
            Raw FeatureVector fields as a dict (from gRPC → dict conversion).
        """
        ip: str = feature_vector.get("client_ip", "0.0.0.0")

        # Push the current feature vector into the LSTM sequence store first
        # (so the LSTM score includes the current request)
        self._lstm.push_feature_vector(ip, feature_vector)

        iso_score = self._iso.score(ip, feature_vector)
        lstm_score = self._lstm.score(ip)

        final = _ISO_WEIGHT * iso_score + _LSTM_WEIGHT * lstm_score

        logger.debug(
            "ip=%s  iso=%.4f  lstm=%.4f  final=%.4f",
            ip,
            iso_score,
            lstm_score,
            final,
        )

        return ScoreResult(
            anomaly_score=float(final),
            model_version=_MODEL_VERSION,
        )

    # Expose sub-models so server.py can call fit() if needed
    @property
    def iso_model(self) -> IsolationForestModel:
        return self._iso

    @property
    def lstm_model(self) -> LSTMModel:
        return self._lstm
