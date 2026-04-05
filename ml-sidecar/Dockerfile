# ─────────────────────────────────────────────────────────────────────────────
# ML Sidecar — Python gRPC service
# python:3.11-slim base, multi-stage for a lean final image
# ─────────────────────────────────────────────────────────────────────────────

# ── Stage 1: compile proto stubs ─────────────────────────────────────────────
FROM python:3.11-slim AS proto-builder

WORKDIR /build

COPY requirements.txt .
RUN pip install --no-cache-dir grpcio-tools==1.62.2

COPY proto/ proto/
RUN python -m grpc_tools.protoc \
      -I proto \
      --python_out=proto \
      --grpc_python_out=proto \
      proto/scorer.proto

# ── Stage 2: install Python dependencies ─────────────────────────────────────
FROM python:3.11-slim AS deps

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir --prefix=/install -r requirements.txt

# ── Stage 3: final runtime image ──────────────────────────────────────────────
FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONPATH=/app

WORKDIR /app

# Copy installed packages
COPY --from=deps /install /usr/local

# Copy compiled proto stubs
COPY --from=proto-builder /build/proto/ ./proto/

# Copy application source
COPY models/     ./models/
COPY training/   ./training/
COPY scorer.py   ./scorer.py
COPY server.py   ./server.py

# Generate a synthetic baseline model so the container has a fallback
# even when no real baseline_iso.pkl has been provided at build time.
RUN python - <<'EOF'
import pathlib, sys
sys.path.insert(0, "/app")
from models.isolation_forest import create_and_save_baseline
create_and_save_baseline(pathlib.Path("/app/models/baseline_iso.pkl"))
EOF

EXPOSE 50051 8000

# gRPC + HTTP health
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD python -c "import urllib.request; urllib.request.urlopen('http://localhost:8000/health')"

CMD ["python", "server.py"]
