# Decision Engine — multi-stage build
FROM golang:1.22-alpine AS builder

# ca-certificates needed for TLS to Redis/Kafka/gRPC
RUN apk add --no-cache ca-certificates git

WORKDIR /app

# Cache dependency downloads separately from source
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /decision-engine ./cmd/server

# ── Runtime image ──────────────────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache ca-certificates

COPY --from=builder /decision-engine /usr/local/bin/decision-engine

EXPOSE 8080

ENTRYPOINT ["decision-engine"]
