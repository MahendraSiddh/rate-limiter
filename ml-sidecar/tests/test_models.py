"""
Unit tests for the ML sidecar — models and scorer.

Run with:
    pytest tests/ -v
"""

from __future__ import annotations

import io
import json
from unittest.mock import MagicMock, patch

import numpy as np
import pytest

# ──────────────────────────────────────────────────────────────────
# Fixtures
# ──────────────────────────────────────────────────────────────────

SAMPLE_FV = {
    "client_ip": "192.168.1.100",
    "ja3_hash": "abc123",
    "user_agent_hash": "def456",
    "req_rate_1s": 5.0,
    "req_rate_10s": 30.0,
    "req_rate_60s": 100.0,
    "http_method": "GET",
    "uri_hash": "xyz789",
    "content_length": 0,
    "header_count": 8,
    "is_http2": 1,
    "unix_timestamp": 1712300000,
}


def _make_redis_mock(stored: dict | None = None) -> MagicMock:
    """Return a MagicMock that behaves like a simple in-memory Redis."""
    store: dict[str, bytes] = {}

    mock = MagicMock()

    def _get(key):
        return store.get(key)

    def _setex(key, ttl, value):
        store[key] = value if isinstance(value, bytes) else value.encode()

    mock.get.side_effect = _get
    mock.setex.side_effect = _setex
    return mock


# ──────────────────────────────────────────────────────────────────
# IsolationForestModel tests
# ──────────────────────────────────────────────────────────────────


class TestIsolationForestModel:
    def test_score_returns_float_in_range(self):
        from models.isolation_forest import IsolationForestModel, create_and_save_baseline
        import pathlib, tempfile

        with tempfile.TemporaryDirectory() as tmp:
            baseline = pathlib.Path(tmp) / "baseline_iso.pkl"
            create_and_save_baseline(baseline)

            with patch("models.isolation_forest._BASELINE_PATH", baseline):
                m = IsolationForestModel(_make_redis_mock())
                score = m.score("10.0.0.1", SAMPLE_FV)
                assert 0.0 <= score <= 1.0

    def test_fit_stores_model_in_redis(self):
        from models.isolation_forest import IsolationForestModel, create_and_save_baseline, MIN_SAMPLES_FOR_FIT
        import pathlib, tempfile

        with tempfile.TemporaryDirectory() as tmp:
            baseline = pathlib.Path(tmp) / "baseline_iso.pkl"
            create_and_save_baseline(baseline)

            redis_mock = _make_redis_mock()
            with patch("models.isolation_forest._BASELINE_PATH", baseline):
                m = IsolationForestModel(redis_mock)
                history = [SAMPLE_FV.copy() for _ in range(MIN_SAMPLES_FOR_FIT)]
                m.fit("10.0.0.1", history)

            # setex should have been called once for the model bytes
            redis_mock.setex.assert_called_once()
            call_args = redis_mock.setex.call_args
            assert "10.0.0.1" in call_args.args[0]

    def test_fit_skipped_for_too_few_samples(self):
        from models.isolation_forest import IsolationForestModel, create_and_save_baseline

        import pathlib, tempfile

        with tempfile.TemporaryDirectory() as tmp:
            baseline = pathlib.Path(tmp) / "baseline_iso.pkl"
            create_and_save_baseline(baseline)

            redis_mock = _make_redis_mock()
            with patch("models.isolation_forest._BASELINE_PATH", baseline):
                m = IsolationForestModel(redis_mock)
                m.fit("10.0.0.1", [SAMPLE_FV])   # only 1 sample

            redis_mock.setex.assert_not_called()

    def test_missing_baseline_returns_neutral(self):
        from models.isolation_forest import IsolationForestModel
        import pathlib

        with patch("models.isolation_forest._BASELINE_PATH", pathlib.Path("/nonexistent/baseline.pkl")):
            m = IsolationForestModel(_make_redis_mock())
            score = m.score("10.0.0.1", SAMPLE_FV)
            assert score == 0.5


# ──────────────────────────────────────────────────────────────────
# LSTMModel tests
# ──────────────────────────────────────────────────────────────────


class TestLSTMModel:
    def test_returns_neutral_without_onnx(self):
        from models.lstm_model import LSTMModel

        redis_mock = _make_redis_mock()
        with patch("models.lstm_model._ONNX_PATH", __import__("pathlib").Path("/nonexistent.onnx")):
            m = LSTMModel(redis_mock)
            score = m.score("10.0.0.1")
            assert score == 0.5

    def test_push_and_fetch_sequence(self):
        from models.lstm_model import LSTMModel, ip_to_subnet, MAX_SEQ_LEN

        store: dict = {}
        redis_mock = MagicMock()
        redis_mock.get.side_effect = lambda k: store.get(k)
        redis_mock.setex.side_effect = lambda k, ttl, v: store.update({k: v})

        with patch("models.lstm_model._ONNX_PATH", __import__("pathlib").Path("/nonexistent.onnx")):
            m = LSTMModel(redis_mock)
            for _ in range(5):
                m.push_feature_vector("10.0.0.1", SAMPLE_FV)

        subnet = ip_to_subnet("10.0.0.1")
        key = f"seq:{subnet}"
        assert key in store
        seq = json.loads(store[key])
        assert len(seq) == 5

    def test_sequence_trimmed_to_max_len(self):
        from models.lstm_model import LSTMModel, MAX_SEQ_LEN

        store: dict = {}
        redis_mock = MagicMock()
        redis_mock.get.side_effect = lambda k: store.get(k)
        redis_mock.setex.side_effect = lambda k, ttl, v: store.update({k: v})

        with patch("models.lstm_model._ONNX_PATH", __import__("pathlib").Path("/nonexistent.onnx")):
            m = LSTMModel(redis_mock)
            for i in range(MAX_SEQ_LEN + 10):
                m.push_feature_vector("10.0.0.1", SAMPLE_FV)

        subnet_key = [k for k in store if k.startswith("seq:")][0]
        seq = json.loads(store[subnet_key])
        assert len(seq) == MAX_SEQ_LEN

    def test_ip_to_subnet_ipv4(self):
        from models.lstm_model import ip_to_subnet
        assert ip_to_subnet("192.168.1.100") == "192.168.1"

    def test_ip_to_subnet_ipv6(self):
        from models.lstm_model import ip_to_subnet
        result = ip_to_subnet("2001:db8:85a3::8a2e:370:7334")
        assert result.startswith("2001:db8:85a3")


# ──────────────────────────────────────────────────────────────────
# Scorer tests
# ──────────────────────────────────────────────────────────────────


class TestScorer:
    def _make_scorer(self):
        from scorer import Scorer
        redis_mock = _make_redis_mock()

        with patch("models.isolation_forest._BASELINE_PATH", __import__("pathlib").Path("/nonexistent.pkl")), \
             patch("models.lstm_model._ONNX_PATH", __import__("pathlib").Path("/nonexistent.onnx")):
            return Scorer(redis_mock)

    def test_score_result_in_range(self):
        scorer = self._make_scorer()
        result = scorer.score(SAMPLE_FV)
        assert 0.0 <= result.anomaly_score <= 1.0

    def test_score_result_has_version(self):
        scorer = self._make_scorer()
        result = scorer.score(SAMPLE_FV)
        assert isinstance(result.model_version, str)
        assert len(result.model_version) > 0

    def test_weighted_combination(self):
        """Verify the 0.6/0.4 weighting is applied correctly."""
        from scorer import _ISO_WEIGHT, _LSTM_WEIGHT
        assert abs(_ISO_WEIGHT + _LSTM_WEIGHT - 1.0) < 1e-9
        assert _ISO_WEIGHT == pytest.approx(0.6)
        assert _LSTM_WEIGHT == pytest.approx(0.4)
