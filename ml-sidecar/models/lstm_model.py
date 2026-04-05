"""
Global LSTM sequence model backed by ONNX Runtime.

The model consumes a sliding window of the last 20 request feature vectors
from the same /24 subnet and returns an anomaly score in [0.0, 1.0].

Redis key layout
----------------
  "seq:{subnet}"  → JSON-serialised list of the last N feature vectors
                    (trimmed to MAX_SEQ_LEN on every write)

ONNX model contract
--------------------
  Input  name: "input"   shape: [1, MAX_SEQ_LEN, FEATURE_DIM]  dtype: float32
  Output name: "output"  shape: [1, 1]                          dtype: float32
           – a single anomaly probability in [0, 1]
"""

from __future__ import annotations

import json
import logging
import pathlib
from typing import Any

import numpy as np
import redis

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

MAX_SEQ_LEN: int = 20
FEATURE_DIM: int = 9   # must match isolation_forest.py

_ONNX_PATH = pathlib.Path(__file__).parent / "lstm.onnx"
_REDIS_SEQ_PREFIX = "seq:"
_REDIS_SEQ_TTL = 3600  # 1 hour – sequences are short-lived


def _fv_to_array(fv: dict) -> list[float]:
    """Encode a FeatureVector dict as a list of FEATURE_DIM floats."""
    return [
        float(fv.get("req_rate_1s", 0.0)),
        float(fv.get("req_rate_10s", 0.0)),
        float(fv.get("req_rate_60s", 0.0)),
        float(fv.get("content_length", 0)),
        float(fv.get("header_count", 0)),
        float(fv.get("is_http2", 0)),
        float(len(fv.get("http_method", ""))),
        float(len(fv.get("ja3_hash", ""))),
        float(len(fv.get("user_agent_hash", ""))),
    ]


def ip_to_subnet(ip: str) -> str:
    """Return the /24 subnet string for an IPv4 address (e.g. '10.0.0.1' → '10.0.0')."""
    parts = ip.split(".")
    if len(parts) == 4:  # IPv4
        return ".".join(parts[:3])
    # IPv6 – use first 48 bits (6 groups)
    parts6 = ip.split(":")
    return ":".join(parts6[:3]) if len(parts6) >= 3 else ip


class LSTMModel:
    """ONNX-backed LSTM sequence scorer."""

    def __init__(self, redis_client: redis.Redis) -> None:
        self._redis = redis_client
        self._session = self._load_session()

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def push_feature_vector(self, ip: str, feature_vector: dict) -> None:
        """Append the latest feature vector for *ip* into the subnet sequence.

        Automatically trims the list to MAX_SEQ_LEN entries.
        """
        subnet = ip_to_subnet(ip)
        key = f"{_REDIS_SEQ_PREFIX}{subnet}"
        raw = self._redis.get(key)
        seq: list[list[float]] = json.loads(raw) if raw else []
        seq.append(_fv_to_array(feature_vector))
        seq = seq[-MAX_SEQ_LEN:]  # keep only the most recent window
        self._redis.setex(key, _REDIS_SEQ_TTL, json.dumps(seq))

    def score(self, ip: str) -> float:
        """Return LSTM anomaly score in [0.0, 1.0] for the subnet of *ip*.

        Returns 0.5 (neutral) if no sequence data is available yet or if
        the ONNX model is not loaded.
        """
        if self._session is None:
            logger.warning("LSTM ONNX session not available, returning neutral 0.5")
            return 0.5

        subnet = ip_to_subnet(ip)
        key = f"{_REDIS_SEQ_PREFIX}{subnet}"
        raw = self._redis.get(key)
        if not raw:
            return 0.5  # no data yet

        seq: list[list[float]] = json.loads(raw)
        if not seq:
            return 0.5

        # Pad or truncate to exactly MAX_SEQ_LEN × FEATURE_DIM
        arr = self._prepare_input(seq)

        try:
            outputs: list[Any] = self._session.run(
                ["output"],  # output name from ONNX export
                {"input": arr},
            )
            raw_score = float(outputs[0].flatten()[0])
        except Exception:  # noqa: BLE001
            logger.exception("LSTM inference error for subnet=%s", subnet)
            return 0.5

        # Clamp to [0, 1]
        return float(np.clip(raw_score, 0.0, 1.0))

    def score_sequence(self, subnet_sequence: list[dict]) -> float:
        """Score an explicitly provided sequence (used by the retrain script).

        Parameters
        ----------
        subnet_sequence:
            List of FeatureVector dicts, most-recent last.
        """
        if self._session is None:
            return 0.5
        arr = self._prepare_input([_fv_to_array(fv) for fv in subnet_sequence])
        try:
            outputs: list[Any] = self._session.run(["output"], {"input": arr})
            return float(np.clip(float(outputs[0].flatten()[0]), 0.0, 1.0))
        except Exception:  # noqa: BLE001
            logger.exception("LSTM inference error (explicit sequence)")
            return 0.5

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _prepare_input(self, seq: list[list[float]]) -> np.ndarray:
        """Return a (1, MAX_SEQ_LEN, FEATURE_DIM) float32 array, zero-padded."""
        padded = np.zeros((MAX_SEQ_LEN, FEATURE_DIM), dtype=np.float32)
        usable = seq[-MAX_SEQ_LEN:]
        for i, vec in enumerate(usable):
            row = np.array(vec, dtype=np.float32)
            padded[i, : len(row)] = row[: FEATURE_DIM]
        return padded[np.newaxis, :, :]  # (1, MAX_SEQ_LEN, FEATURE_DIM)

    def _load_session(self):  # type: ignore[return]
        """Load the ONNX model; returns None if the file does not exist."""
        if not _ONNX_PATH.exists():
            logger.warning(
                "LSTM ONNX model not found at %s – scoring will return neutral 0.5",
                _ONNX_PATH,
            )
            return None
        try:
            import onnxruntime as ort  # pylint: disable=import-outside-toplevel

            sess_options = ort.SessionOptions()
            sess_options.intra_op_num_threads = 1
            sess_options.inter_op_num_threads = 1
            sess_options.graph_optimization_level = (
                ort.GraphOptimizationLevel.ORT_ENABLE_ALL
            )
            session = ort.InferenceSession(
                str(_ONNX_PATH),
                sess_options=sess_options,
                providers=["CPUExecutionProvider"],
            )
            logger.info("Loaded LSTM ONNX model from %s", _ONNX_PATH)
            return session
        except Exception:  # noqa: BLE001
            logger.exception("Failed to load LSTM ONNX model")
            return None
